// Package sched decides which rules are out of date and runs them
// (README §4.2, §7). M2 runs steps serially in plan order.
package sched

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rveen/rmake/internal/execx"
	"github.com/rveen/rmake/internal/graph"
	"github.com/rveen/rmake/internal/hash"
	"github.com/rveen/rmake/internal/state"
)

// KeyVersion is part of every rule key; bump it when the key's composition
// changes so that all rules rebuild.
const KeyVersion = "rmake-key-1"

// maxHeader is the longest command shown in a progress line.
const maxHeader = 200

// ErrFailed is returned by Build when at least one rule or task failed. The
// failures have already been reported.
var ErrFailed = errors.New("build failed")

// Builder runs a plan.
type Builder struct {
	G       *graph.Graph
	Store   state.Store
	Hash    *hash.Cache
	Res     *execx.Resolver
	Root    string
	BaseEnv []string
	Out     io.Writer
	Verbose bool // print every command before running it
	Force   bool // -B: every rule is out of date
	Keep    bool // -k: keep going after failures
	DryRun  bool // -n: print what would run and why

	// Ran counts commands started (for tests).
	Ran int
}

// RuleID is the identity of a rule in the state store: a hash of its sorted
// outputs.
func RuleID(r *graph.Rule) string {
	outs := append([]string(nil), r.Outs...)
	sort.Strings(outs)
	h := sha256.Sum256([]byte(strings.Join(outs, "\x00")))
	return hex.EncodeToString(h[:16])
}

// parts are the components of a rule key.
type parts struct {
	cmds       []string
	tools      []string
	env        map[string]string
	inputs     map[string]string
	discovered map[string]string
}

// keyCmd is the unambiguous key form of a command.
func keyCmd(c graph.Cmd) string {
	if c.Shell {
		b, _ := json.Marshal(c.Script)
		return "sh " + string(b)
	}
	b, _ := json.Marshal(c.Argv)
	return string(b)
}

func (p *parts) key() string {
	h := sha256.New()
	var buf []byte
	w := func(tag string, kv ...string) {
		buf = append(buf[:0], tag...)
		for _, s := range kv {
			buf = binary.AppendUvarint(buf, uint64(len(s)))
			buf = append(buf, s...)
		}
		h.Write(buf)
	}
	w("version", KeyVersion)
	for i, c := range p.cmds {
		w("cmd", c, p.tools[i])
	}
	for _, m := range []struct {
		tag string
		m   map[string]string
	}{{"env", p.env}, {"in", p.inputs}, {"disc", p.discovered}} {
		for _, k := range sortedKeys(m.m) {
			w(m.tag, k, m.m[k])
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// missingHash marks a discovered input that no longer exists.
const missingHash = "missing"

// decision is the rebuild decision for one rule.
type decision struct {
	reasons []string // empty: up to date
	parts   parts
	key     string
}

// decide computes r's key and whether it must run. In a dry run, inputs in
// pending (outputs of rules that would run) cannot be hashed yet; they make
// the rule run.
func (b *Builder) decide(r *graph.Rule, pending map[string]bool) (decision, error) {
	var d decision
	p := &d.parts
	for _, c := range r.Cmds {
		p.cmds = append(p.cmds, keyCmd(c))
		tool, err := b.Res.Resolve(c)
		if err != nil && !b.DryRun {
			return d, err
		}
		p.tools = append(p.tools, tool)
	}
	p.env = r.Env
	p.inputs = map[string]string{}
	var waiting []string
	for _, in := range r.Inputs() {
		if pending[in] {
			waiting = append(waiting, in)
			continue
		}
		h, ok, err := b.Hash.Hash(in)
		if err != nil {
			return d, err
		}
		if !ok {
			return d, fmt.Errorf("input %s of %s is missing", in, r.Name())
		}
		p.inputs[in] = h
	}
	rec, haveRec := b.Store.Rule(RuleID(r))
	if haveRec && len(rec.Discovered) > 0 {
		p.discovered = map[string]string{}
		for in := range rec.Discovered {
			if pending[in] {
				waiting = append(waiting, in)
				continue
			}
			h, ok, err := b.Hash.Hash(in)
			if err != nil {
				return d, err
			}
			if !ok {
				h = missingHash
			}
			p.discovered[in] = h
		}
	}
	d.key = p.key()

	why := func(format string, args ...any) { d.reasons = append(d.reasons, fmt.Sprintf(format, args...)) }
	switch {
	case !haveRec:
		why("no build record")
	case b.Force:
		why("forced (-B)")
	case r.Always:
		why("always")
	}
	for _, in := range waiting {
		why("input %s will be rebuilt", in)
	}
	if haveRec {
		for _, o := range r.Outs {
			h, ok, err := b.Hash.Hash(o)
			switch {
			case err != nil:
				return d, err
			case !ok:
				why("output %s is missing", o)
			case h != rec.OutHashes[o]:
				why("output %s was modified outside rmake", o)
			}
		}
		if len(waiting) == 0 && d.key != rec.Key {
			d.reasons = append(d.reasons, explain(rec, p)...)
		}
	}
	return d, nil
}

// explain lists the differences between a record and the current key parts.
func explain(rec *state.Rule, p *parts) []string {
	var out []string
	if len(rec.Cmds) != len(p.cmds) {
		out = append(out, fmt.Sprintf("number of commands changed from %d to %d", len(rec.Cmds), len(p.cmds)))
	} else {
		for i := range p.cmds {
			if rec.Cmds[i] != p.cmds[i] {
				out = append(out, fmt.Sprintf("command %d changed:\n    was: %s\n    now: %s", i+1, rec.Cmds[i], p.cmds[i]))
			} else if i < len(rec.Tools) && rec.Tools[i] != p.tools[i] {
				out = append(out, fmt.Sprintf("command %d now runs %s (was %s)", i+1, p.tools[i], rec.Tools[i]))
			}
		}
	}
	out = append(out, diffMaps("variable", rec.Env, p.env)...)
	out = append(out, diffMaps("input", rec.Inputs, p.inputs)...)
	out = append(out, diffMaps("discovered input", rec.Discovered, p.discovered)...)
	if len(out) == 0 {
		out = append(out, "build key changed (new rmake version?)")
	}
	return out
}

func diffMaps(what string, was, now map[string]string) []string {
	var out []string
	for _, k := range sortedKeys(now) {
		old, ok := was[k]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("new %s %s", what, k))
		case now[k] == missingHash:
			out = append(out, fmt.Sprintf("%s %s is missing", what, k))
		case old != now[k]:
			out = append(out, fmt.Sprintf("%s %s changed", what, k))
		}
	}
	for _, k := range sortedKeys(was) {
		if _, ok := now[k]; !ok {
			out = append(out, fmt.Sprintf("%s %s removed", what, k))
		}
	}
	return out
}

// Build runs steps (in plan order). It returns ErrFailed after reporting
// failures, or another error for problems that stop the build.
func (b *Builder) Build(ctx context.Context, steps []graph.Step) error {
	failed := map[string]bool{}     // outputs of failed or skipped rules
	failedTask := map[string]bool{} // failed or skipped tasks
	pending := map[string]bool{}    // dry run: outputs that would be rebuilt
	anyFailed := false
	worked := false
	total := len(steps)

	blocked := func(deps []string) string {
		for _, d := range deps {
			if failed[d] || failedTask[d] {
				return d
			}
		}
		return ""
	}

	// Stat every file the plan looks at concurrently; the decisions below
	// then run without waiting on system calls one by one.
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

	for i, s := range steps {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := i + 1
		if s.Task != nil {
			if blocked(s.Task.Needs) != "" {
				failedTask[s.Task.Name] = true
				continue
			}
			worked = true
			if err := b.runTask(ctx, s, n, total); err != nil {
				if !errors.Is(err, ErrFailed) {
					return err
				}
				anyFailed = true
				failedTask[s.Task.Name] = true
				if !b.Keep {
					return ErrFailed
				}
			}
			continue
		}
		r := s.Rule
		if blocked(r.Inputs()) != "" {
			for _, o := range r.Outs {
				failed[o] = true
			}
			continue
		}
		d, err := b.decide(r, pending)
		if err != nil {
			fmt.Fprintf(b.Out, "FAILED: %s\nrmake: %s: %v\n", strings.Join(r.Outs, " "), r.Pos, err)
			anyFailed = true
			for _, o := range r.Outs {
				failed[o] = true
			}
			if !b.Keep {
				return ErrFailed
			}
			continue
		}
		if len(d.reasons) == 0 {
			continue
		}
		worked = true
		if b.DryRun {
			fmt.Fprintf(b.Out, "# %s: %s\n", strings.Join(r.Outs, " "), strings.Join(d.reasons, "; "))
			for _, c := range r.Cmds {
				fmt.Fprintln(b.Out, c.String())
			}
			for _, o := range r.Outs {
				pending[o] = true
			}
			continue
		}
		if err := b.runRule(ctx, r, d, n, total); err != nil {
			if !errors.Is(err, ErrFailed) {
				return err
			}
			anyFailed = true
			for _, o := range r.Outs {
				failed[o] = true
			}
			if !b.Keep {
				return ErrFailed
			}
		}
	}
	if anyFailed {
		return ErrFailed
	}
	if !worked {
		fmt.Fprintln(b.Out, "rmake: no work to do")
	}
	return nil
}

func (b *Builder) header(n, total int, desc string, cmds []graph.Cmd) {
	if desc == "" && len(cmds) > 0 {
		desc = cmds[0].String()
		if len(desc) > maxHeader && !b.Verbose {
			desc = desc[:maxHeader] + " ..."
		}
	}
	fmt.Fprintf(b.Out, "[%d/%d] %s\n", n, total, desc)
}

// runCmds runs commands in sequence, printing their output. It reports a
// failure and returns ErrFailed if one fails.
func (b *Builder) runCmds(ctx context.Context, what string, cmds []graph.Cmd, env []string) error {
	for _, c := range cmds {
		if b.Verbose {
			fmt.Fprintln(b.Out, c.String())
		}
		b.Ran++
		res, err := execx.Run(ctx, b.Res, c, b.Root, env)
		if err != nil {
			fmt.Fprintf(b.Out, "FAILED: %s\n%s\n", what, c.String())
			if len(res.Output) > 0 {
				b.Out.Write(res.Output)
				if res.Output[len(res.Output)-1] != '\n' {
					fmt.Fprintln(b.Out)
				}
			}
			fmt.Fprintf(b.Out, "rmake: %s: %v\n", c.Pos, err)
			return ErrFailed
		}
		b.Out.Write(res.Output)
	}
	return nil
}

func (b *Builder) runTask(ctx context.Context, s graph.Step, n, total int) error {
	cmds, env, err := s.Task.Expand(s.Args)
	if err != nil {
		return err
	}
	name := strings.Join(append([]string{s.Task.Name}, s.Args...), " ")
	if b.DryRun {
		fmt.Fprintf(b.Out, "# task %s: tasks always run\n", name)
		for _, c := range cmds {
			fmt.Fprintln(b.Out, c.String())
		}
		return nil
	}
	desc := s.Task.Desc
	if desc != "" && len(s.Task.Params) > 0 {
		desc = "" // the stored desc has placeholder arguments
	}
	if desc == "" {
		desc = "task " + name
	}
	b.header(n, total, desc, nil)
	return b.runCmds(ctx, "task "+name, cmds, execx.Env(b.BaseEnv, env))
}

func (b *Builder) runRule(ctx context.Context, r *graph.Rule, d decision, n, total int) error {
	id := RuleID(r)
	for _, o := range r.Outs {
		b.Hash.Forget(o)
		if err := os.MkdirAll(filepath.Dir(b.abs(o)), 0o755); err != nil {
			return err
		}
	}
	b.header(n, total, r.Desc, r.Cmds)
	start := time.Now()
	fail := func() error {
		b.removeOutputs(r)
		b.Store.DeleteRule(id)
		return ErrFailed
	}
	if err := b.runCmds(ctx, strings.Join(r.Outs, " "), r.Cmds, execx.Env(b.BaseEnv, r.Env)); err != nil {
		return fail()
	}
	outHashes := map[string]string{}
	for _, o := range r.Outs {
		b.Hash.Forget(o)
		h, ok, err := b.Hash.Hash(o)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintf(b.Out, "FAILED: %s\nrmake: %s: the commands did not create %s\n", strings.Join(r.Outs, " "), r.Pos, o)
			return fail()
		}
		outHashes[o] = h
	}
	reason := ""
	if len(d.reasons) > 0 {
		reason = d.reasons[0]
	}
	b.Store.PutRule(&state.Rule{
		ID:         id,
		Outs:       r.Outs,
		Key:        d.key,
		Cmds:       d.parts.cmds,
		Tools:      d.parts.tools,
		Env:        d.parts.env,
		Inputs:     d.parts.inputs,
		Discovered: d.parts.discovered,
		OutHashes:  outHashes,
		Duration:   time.Since(start).Milliseconds(),
		Reason:     reason,
		Time:       time.Now().UnixNano(),
	})
	return nil
}

func (b *Builder) abs(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(b.Root, p)
}

func (b *Builder) removeOutputs(r *graph.Rule) {
	for _, o := range r.Outs {
		b.Hash.Forget(o)
		os.Remove(b.abs(o))
	}
}
