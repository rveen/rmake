package sched

import (
	"bytes"
	"container/heap"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/rveen/rmake/internal/depfile"
	"github.com/rveen/rmake/internal/execx"
	"github.com/rveen/rmake/internal/graph"
	"github.com/rveen/rmake/internal/hash"
	"github.com/rveen/rmake/internal/state"
)

// The scheduler (README §7.1) runs on the goroutine that calls Build: it
// makes every rebuild decision, owns the hash cache, the state store and the
// output, and starts up to Jobs commands in worker goroutines. A worker only
// runs commands, captures their output and hashes the outputs (with
// hash.Cache.Compute, which is safe for concurrent use).
//
// A step is ready when every step it depends on has finished. A ready step
// is decided at once (so that up-to-date steps never wait for a job slot and
// the progress total shrinks early); a step that must run waits for a slot.
// Among waiting steps the one earliest in plan order starts first, so -j1
// runs steps in plan order.

// node is a step of the plan with its dependency edges.
type node struct {
	step       graph.Step
	deps       int   // dependencies not finished yet
	dependents []int // steps depending on this one
	failedDep  bool  // a dependency failed or was skipped

	// Set when the step is decided to run.
	d   decision      // rules
	run graph.TaskRun // tasks: the expanded task
}

// job is one step running in a worker.
type job struct {
	idx  int
	n    int // progress number
	desc string
	what string // outputs, or "task NAME ARGS", for failure messages
	rule *graph.Rule
	cmds []graph.Cmd
	env  []string
	dir  string // root-relative working directory

	// Results, written by the worker.
	out    bytes.Buffer // captured output and failure messages
	ran    int          // commands started
	err    error        // nil, ErrFailed (reported in out) or ErrInterrupted
	hashes []hash.Entry // rules: output hashes
	deps   []string     // rules with deps: prerequisites from the depfile
	dur    time.Duration
}

// run is the state of one Build.
type run struct {
	b        *Builder
	ctx      context.Context
	nodes    []node
	ready    intHeap         // dependencies finished, not decided yet
	runnable intHeap         // decided to run, waiting for a job slot
	pending  map[string]bool // dry run: outputs that would be rebuilt
	prog     *progress

	stop        bool // start no more steps
	interrupted bool
	failed      bool
	worked      bool
}

// Build runs the steps of a plan (dependencies first, as Plan returns them).
// It sets Worked if anything ran (the caller reports "no work to do"). It
// returns ErrFailed after reporting failures, ErrInterrupted if ctx was
// cancelled, or another error for problems that stop the build.
func (b *Builder) Build(ctx context.Context, steps []graph.Step) error {
	// Stat every file the plan looks at concurrently; the decisions then
	// run without waiting on system calls one by one.
	var paths []string
	seen := map[string]bool{}
	for _, s := range steps {
		if s.Rule == nil {
			continue
		}
		for _, p := range append(s.Rule.Inputs(), s.Rule.Outs...) {
			if !seen[p] {
				seen[p] = true
				paths = append(paths, p)
			}
		}
	}
	b.Hash.Prestat(paths)

	r := &run{
		b:       b,
		ctx:     ctx,
		nodes:   makeNodes(steps),
		pending: map[string]bool{},
		prog:    &progress{w: b.Out, tty: b.TTY, width: b.Width, total: len(steps), running: map[int]string{}},
	}
	return r.loop()
}

func taskKey(name string, args []string) string {
	return name + "\x00" + strings.Join(args, "\x00")
}

// makeNodes links each step to the steps producing its inputs and the tasks
// it needs.
func makeNodes(steps []graph.Step) []node {
	nodes := make([]node, len(steps))
	producer := map[string]int{}
	tasks := map[string]int{}
	for i, s := range steps {
		nodes[i].step = s
		if s.Rule != nil {
			for _, o := range s.Rule.Outs {
				producer[o] = i
			}
		} else {
			tasks[taskKey(s.Task.Name, s.Args)] = i
		}
	}
	seen := make([]int, len(steps)) // seen[k] == i+1: edge k -> i exists
	for i, s := range steps {
		var on []string
		if s.Rule != nil {
			on = s.Rule.Inputs()
		} else {
			on = s.Task.Needs
		}
		for _, name := range on {
			k, ok := -1, false
			if s.Task != nil {
				k, ok = tasks[taskKey(name, nil)]
			}
			if !ok {
				k, ok = producer[name]
			}
			if !ok || seen[k] == i+1 || k == i {
				continue
			}
			seen[k] = i + 1
			nodes[i].deps++
			nodes[k].dependents = append(nodes[k].dependents, i)
		}
	}
	return nodes
}

func (r *run) loop() error {
	b := r.b
	jobs := max(b.Jobs, 1)
	for i := range r.nodes {
		if r.nodes[i].deps == 0 {
			heap.Push(&r.ready, i)
		}
	}
	results := make(chan *job)
	running := 0
	done := r.ctx.Done()

	// With a jobserver, rmake has one implicit job slot and one more per
	// token it holds; tokens are acquired in the background and returned
	// as soon as they are not needed, so that nested makes can use them.
	type token struct {
		b   byte
		err error
	}
	var held []byte
	tokens := make(chan token, 1)
	acquiring := false
	slots := func() int {
		if b.Tokens == nil {
			return jobs
		}
		return 1 + len(held)
	}
	release := func() {
		b.Tokens.Release(held[len(held)-1])
		held = held[:len(held)-1]
	}
	for {
		if !r.stop && r.ctx.Err() != nil {
			r.stop, r.interrupted, done = true, true, nil
		}
		for !r.stop && r.ready.Len() > 0 {
			r.decide(heap.Pop(&r.ready).(int))
		}
		for !r.stop && running < slots() && r.runnable.Len() > 0 {
			j := r.start(heap.Pop(&r.runnable).(int))
			running++
			go func() {
				b.work(r.ctx, j)
				results <- j
			}()
		}
		if b.Tokens != nil && !r.stop && !acquiring && r.runnable.Len() > 0 && running < jobs {
			acquiring = true
			go func() {
				t, err := b.Tokens.Acquire()
				tokens <- token{t, err}
			}()
		}
		if running == 0 {
			break
		}
		select {
		case j := <-results:
			running--
			r.complete(j)
			if b.Tokens != nil && len(held) > 0 && running < slots() {
				release()
			}
		case t := <-tokens:
			acquiring = false
			if t.err != nil {
				b.Tokens, held = nil, nil // the jobserver is gone: fall back to -j
				break
			}
			held = append(held, t.b)
			if r.stop || r.runnable.Len() == 0 {
				release()
			}
		case <-done:
			r.stop, r.interrupted, done = true, true, nil
		}
	}
	if acquiring {
		// Nothing runs any more, so the token comes back soon.
		if t := <-tokens; t.err == nil {
			held = append(held, t.b)
		}
	}
	for len(held) > 0 {
		release()
	}
	r.prog.end()
	if r.worked {
		b.Worked = true
	}
	switch {
	case r.interrupted:
		return ErrInterrupted
	case r.failed:
		return ErrFailed
	}
	return nil
}

// finish marks step i finished and makes dependents whose dependencies are
// all finished ready.
func (r *run) finish(i int, ok bool) {
	for _, k := range r.nodes[i].dependents {
		n := &r.nodes[k]
		if !ok {
			n.failedDep = true
		}
		if n.deps--; n.deps == 0 {
			heap.Push(&r.ready, k)
		}
	}
}

// fail marks step i failed; without -k no more steps start.
func (r *run) fail(i int) {
	r.failed = true
	if !r.b.Keep {
		r.stop = true
	}
	r.finish(i, false)
}

// decide decides a ready step: skip it, finish it (up to date, dry run), or
// queue it to run.
func (r *run) decide(i int) {
	b := r.b
	n := &r.nodes[i]
	if n.failedDep {
		r.prog.skip()
		r.finish(i, false)
		return
	}
	if t := n.step.Task; t != nil {
		run, err := t.Expand(n.step.Args)
		if err != nil {
			r.prog.message(fmt.Sprintf("FAILED: task %s\nrmk: %s: %v\n", t.Name, t.Pos, err))
			r.fail(i)
			return
		}
		r.worked = true
		if b.DryRun {
			fmt.Fprintf(b.Out, "# task %s: tasks always run\n", taskName(n.step))
			for _, c := range run.Cmds {
				fmt.Fprintln(b.Out, c.String())
			}
			r.finish(i, true)
			return
		}
		n.run = run
		heap.Push(&r.runnable, i)
		return
	}
	rule := n.step.Rule
	d, err := b.decide(rule, r.pending)
	if err != nil {
		r.prog.message(fmt.Sprintf("FAILED: %s\nrmk: %s: %v\n", strings.Join(rule.Outs, " "), rule.Pos, err))
		r.fail(i)
		return
	}
	if len(d.reasons) == 0 {
		r.prog.skip()
		r.finish(i, true)
		return
	}
	r.worked = true
	if b.DryRun {
		fmt.Fprintf(b.Out, "# %s: %s\n", strings.Join(rule.Outs, " "), strings.Join(d.reasons, "; "))
		for _, c := range rule.Cmds {
			fmt.Fprintln(b.Out, c.String())
		}
		for _, o := range rule.Outs {
			r.pending[o] = true
		}
		r.finish(i, true)
		return
	}
	n.d = d
	heap.Push(&r.runnable, i)
}

func taskName(s graph.Step) string {
	return strings.Join(append([]string{s.Task.Name}, s.Args...), " ")
}

// start prepares the job for step i and announces it.
func (r *run) start(i int) *job {
	b := r.b
	n := &r.nodes[i]
	j := &job{idx: i}
	if t := n.step.Task; t != nil {
		name := taskName(n.step)
		j.what = "task " + name
		j.cmds = n.run.Cmds
		j.env = execx.Env(b.BaseEnv, n.run.Env)
		j.desc = n.run.Desc
		j.dir = n.run.Dir
		if j.desc == "" {
			j.desc = j.what
		}
	} else {
		rule := n.step.Rule
		j.rule = rule
		j.what = strings.Join(rule.Outs, " ")
		j.cmds = rule.Cmds
		j.env = execx.Env(b.BaseEnv, rule.Env)
		j.dir = rule.Dir
		j.desc = rule.Desc
		if j.desc == "" && len(rule.Cmds) > 0 {
			j.desc = rule.Cmds[0].String()
			if len(j.desc) > maxHeader && !b.Verbose {
				j.desc = j.desc[:maxHeader] + " ..."
			}
		}
		// The outputs are about to be rewritten.
		for _, o := range rule.Outs {
			b.Hash.Forget(o)
		}
	}
	j.n = r.prog.start(j.desc)
	return j
}

// work runs a job's commands and hashes its outputs. It runs in a worker
// goroutine and touches nothing shared but the resolver (which is safe for
// concurrent use).
func (b *Builder) work(ctx context.Context, j *job) {
	if j.rule != nil {
		for _, o := range j.rule.Outs {
			if err := os.MkdirAll(filepath.Dir(b.abs(o)), 0o755); err != nil {
				fmt.Fprintf(&j.out, "FAILED: %s\nrmk: %v\n", j.what, err)
				j.err = ErrFailed
				return
			}
		}
	}
	start := time.Now()
	j.err = b.runCmds(ctx, j)
	j.dur = time.Since(start)
	if j.rule != nil && j.rule.Deps != "" {
		// Read and delete the depfile, whatever the outcome.
		deps, err := readDepfile(b.abs(j.rule.Depfile))
		if err != nil && j.err == nil {
			fmt.Fprintf(&j.out, "FAILED: %s\nrmk: %s: %v\n", j.what, j.rule.Pos, err)
			j.err = ErrFailed
		}
		j.deps = deps
	}
	if j.err != nil || j.rule == nil {
		return
	}
	for _, o := range j.rule.Outs {
		e, err := b.Hash.Compute(o)
		if err != nil {
			fmt.Fprintf(&j.out, "FAILED: %s\nrmk: hashing %s: %v\n", j.what, o, err)
			j.err = ErrFailed
			return
		}
		j.hashes = append(j.hashes, e)
	}
}

// runCmds runs a job's commands in sequence, capturing their output. A failure
// is reported in the job's output.
func (b *Builder) runCmds(ctx context.Context, j *job) error {
	for _, c := range j.cmds {
		if ctx.Err() != nil {
			return ErrInterrupted
		}
		if b.Verbose {
			fmt.Fprintln(&j.out, c.String())
		}
		j.ran++
		res, err := execx.Run(ctx, b.Res, c, b.abs(j.dir), j.env)
		if err != nil {
			if ctx.Err() != nil {
				return ErrInterrupted
			}
			fmt.Fprintf(&j.out, "FAILED: %s\n%s\n", j.what, c.String())
			if len(res.Output) > 0 {
				j.out.Write(res.Output)
				if res.Output[len(res.Output)-1] != '\n' {
					j.out.WriteByte('\n')
				}
			}
			fmt.Fprintf(&j.out, "rmk: %s: %v\n", c.Pos, err)
			return ErrFailed
		}
		j.out.Write(res.Output)
	}
	return nil
}

// complete records the result of a finished job.
func (r *run) complete(j *job) {
	b := r.b
	b.Ran += j.ran
	discard := func() {
		if j.rule != nil {
			b.removeOutputs(j.rule)
			b.Store.DeleteRule(RuleID(j.rule))
		}
	}
	switch {
	case errors.Is(j.err, ErrInterrupted):
		discard()
		r.prog.finish(j.n, j.desc, nil)
		r.finish(j.idx, false)
		return
	case j.err != nil:
		discard()
		r.prog.finish(j.n, j.desc, j.out.Bytes())
		r.fail(j.idx)
		return
	case j.rule == nil:
		r.prog.finish(j.n, j.desc, j.out.Bytes())
		r.finish(j.idx, true)
		return
	}
	rule := j.rule
	outHashes := map[string]string{}
	for _, e := range j.hashes {
		h, ok := b.Hash.Put(e)
		if !ok {
			for o := range outHashes {
				b.Hash.Forget(o)
			}
			fmt.Fprintf(&j.out, "FAILED: %s\nrmk: %s: the commands did not create %s\n", j.what, rule.Pos, e.Path())
			discard()
			r.prog.finish(j.n, j.desc, j.out.Bytes())
			r.fail(j.idx)
			return
		}
		outHashes[e.Path()] = h
	}
	d := r.nodes[j.idx].d
	if rule.Deps != "" {
		// The key records the inputs the depfile names now.
		d.parts.discovered = b.discovered(rule, j.deps)
		d.key = d.parts.key()
	}
	reason := ""
	if len(d.reasons) > 0 {
		reason = d.reasons[0]
	}
	b.Store.PutRule(&state.Rule{
		ID:         RuleID(rule),
		Outs:       rule.Outs,
		Key:        d.key,
		Cmds:       d.parts.cmds,
		Tools:      d.parts.tools,
		Env:        d.parts.env,
		Inputs:     d.parts.inputs,
		Discovered: d.parts.discovered,
		OutHashes:  outHashes,
		Duration:   j.dur.Milliseconds(),
		Reason:     reason,
		Time:       time.Now().UnixNano(),
	})
	r.prog.finish(j.n, j.desc, j.out.Bytes())
	r.finish(j.idx, true)
}

// readDepfile reads, parses and removes a depfile. A missing depfile means
// no discovered inputs (as in Ninja).
func readDepfile(p string) ([]string, error) {
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	os.Remove(p)
	deps, err := depfile.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", p, err)
	}
	return deps, nil
}

// discovered hashes the prerequisites of a depfile, normalized, without the
// rule's declared inputs and outputs (missing files get missingHash). It
// returns nil if nothing remains.
func (b *Builder) discovered(r *graph.Rule, deps []string) map[string]string {
	if len(deps) == 0 {
		return nil
	}
	declared := map[string]bool{}
	for _, p := range append(r.Inputs(), r.Outs...) {
		declared[p] = true
	}
	root := b.absRoot()
	m := map[string]string{}
	for _, p := range deps {
		if r.Dir != "" && !path.IsAbs(p) {
			p = path.Join(r.Dir, p) // the commands ran in r.Dir
		}
		p = depfile.Normalize(p, root)
		if declared[p] {
			continue
		}
		h, ok, err := b.Hash.Hash(p)
		if err != nil || !ok {
			h = missingHash
		}
		m[p] = h
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// intHeap is a min-heap of step indexes (plan order).
type intHeap []int

func (h intHeap) Len() int           { return len(h) }
func (h intHeap) Less(i, k int) bool { return h[i] < h[k] }
func (h intHeap) Swap(i, k int)      { h[i], h[k] = h[k], h[i] }
func (h *intHeap) Push(x any)        { *h = append(*h, x.(int)) }
func (h *intHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}
