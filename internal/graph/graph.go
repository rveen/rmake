// Package graph holds the concrete build graph: file rules and tasks, with
// static checks (duplicate outputs, cycles, name collisions) and planning.
package graph

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/rveen/rmake/internal/syntax"
)

// Cmd is one command of a rule or task.
type Cmd struct {
	Pos    syntax.Pos
	Shell  bool     // run Script with /bin/sh -c
	Script string   // when Shell
	Argv   []string // when !Shell
}

// String renders the command for display. Argv words are shell-quoted where
// needed.
func (c Cmd) String() string {
	if c.Shell {
		return c.Script
	}
	q := make([]string, len(c.Argv))
	for i, a := range c.Argv {
		q[i] = Quote(a)
	}
	return strings.Join(q, " ")
}

// Quote quotes s for a POSIX shell if it contains special characters.
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./=+,:@%", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Rule is a concrete (instantiated) file rule. All paths are root-relative.
type Rule struct {
	Pos      syntax.Pos
	Outs     []string
	Ins      []string // explicit inputs ($in)
	Implicit []string // inputs after '|'
	Cmds     []Cmd
	Deps     string // "", "make" or "gcc"
	Depfile  string
	Env      map[string]string // variables listed in pass attributes
	Always   bool
	Desc     string
	Stem     string
}

// Name is the rule's display name: its first output.
func (r *Rule) Name() string { return r.Outs[0] }

// Inputs returns explicit then implicit inputs.
func (r *Rule) Inputs() []string {
	return append(append([]string(nil), r.Ins...), r.Implicit...)
}

// Task is a named task.
type Task struct {
	Pos    syntax.Pos
	Name   string
	Params []string
	Needs  []string // task names or root-relative file paths
	Desc   string
	// Expand returns the commands and pass'ed variables for the given
	// arguments (one per parameter).
	Expand func(args []string) ([]Cmd, map[string]string, error)
}

// Graph is the whole build.
type Graph struct {
	Rules    []*Rule
	Producer map[string]*Rule // output path -> rule
	Tasks    map[string]*Task
	TaskList []*Task // declaration order
	Defaults []string
	// Generated lists included files that rules produce, in include order.
	// They must be built (and the build file reloaded if they changed)
	// before the graph is used; a missing one was read as empty.
	Generated []string
}

// New returns an empty graph.
func New() *Graph {
	return &Graph{Producer: map[string]*Rule{}, Tasks: map[string]*Task{}}
}

// AddRule adds r, failing if one of its outputs is already produced.
func (g *Graph) AddRule(r *Rule) error {
	for _, o := range r.Outs {
		if prev, ok := g.Producer[o]; ok {
			return syntax.Errorf(r.Pos, "%s is produced by two rules (other rule at %s)", o, prev.Pos)
		}
	}
	for _, o := range r.Outs {
		g.Producer[o] = r
	}
	g.Rules = append(g.Rules, r)
	return nil
}

// AddTask adds t, failing on a duplicate name.
func (g *Graph) AddTask(t *Task) error {
	if prev, ok := g.Tasks[t.Name]; ok {
		return syntax.Errorf(t.Pos, "task %s already defined at %s", t.Name, prev.Pos)
	}
	g.Tasks[t.Name] = t
	g.TaskList = append(g.TaskList, t)
	return nil
}

// Targets returns all declared outputs, sorted.
func (g *Graph) Targets() []string {
	out := make([]string, 0, len(g.Producer))
	for o := range g.Producer {
		out = append(out, o)
	}
	sort.Strings(out)
	return out
}

// Files returns every file the graph mentions: inputs and outputs of rules
// and file dependencies of tasks, without duplicates.
func (g *Graph) Files() []string {
	seen := map[string]bool{}
	var out []string
	add := func(ps []string) {
		for _, p := range ps {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	for _, r := range g.Rules {
		add(r.Outs)
		add(r.Ins)
		add(r.Implicit)
	}
	for _, t := range g.TaskList {
		for _, n := range t.Needs {
			if g.Tasks[n] == nil {
				add([]string{n})
			}
		}
	}
	return out
}

// node is a rule or a task in graph traversals.
type node struct {
	rule *Rule
	task *Task
}

func (n node) name() string {
	if n.task != nil {
		return n.task.Name
	}
	return n.rule.Name()
}

// deps returns the nodes n depends on. Files without a producer are leaves
// and are omitted.
func (g *Graph) deps(n node) []node {
	var targets []string
	if n.task != nil {
		targets = n.task.Needs
	} else {
		targets = n.rule.Inputs()
	}
	var out []node
	for _, t := range targets {
		if n.task != nil {
			if tk, ok := g.Tasks[t]; ok {
				out = append(out, node{task: tk})
				continue
			}
		}
		if r, ok := g.Producer[t]; ok {
			out = append(out, node{rule: r})
		}
	}
	return out
}

func (g *Graph) nodes() []node {
	var ns []node
	for _, r := range g.Rules {
		ns = append(ns, node{rule: r})
	}
	for _, t := range g.TaskList {
		ns = append(ns, node{task: t})
	}
	return ns
}

// Check runs the static checks of README §5.8 that need the whole graph:
// task/file name collisions and cycles.
func (g *Graph) Check() error {
	for _, t := range g.TaskList {
		if r, ok := g.Producer[t.Name]; ok {
			return syntax.Errorf(t.Pos, "task %s has the same name as a file produced by the rule at %s", t.Name, r.Pos)
		}
	}
	const (
		white = iota
		grey
		black
	)
	color := map[node]int{}
	var stack []node
	var visit func(n node) error
	visit = func(n node) error {
		color[n] = grey
		stack = append(stack, n)
		for _, d := range g.deps(n) {
			switch color[d] {
			case grey:
				i := len(stack) - 1
				for stack[i] != d {
					i--
				}
				var names []string
				for _, s := range stack[i:] {
					names = append(names, s.name())
				}
				names = append(names, d.name())
				return &syntax.Error{Pos: posOf(d), Msg: "dependency cycle: " + strings.Join(names, " -> ")}
			case white:
				if err := visit(d); err != nil {
					return err
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = black
		return nil
	}
	for _, n := range g.nodes() {
		if color[n] == white {
			if err := visit(n); err != nil {
				return err
			}
		}
	}
	return nil
}

func posOf(n node) syntax.Pos {
	if n.task != nil {
		return n.task.Pos
	}
	return n.rule.Pos
}

// Request is a target or a task invocation from the command line.
type Request struct {
	Name string
	Args []string // task arguments
}

// Step is one unit of work in a plan.
type Step struct {
	Rule *Rule
	Task *Task
	Args []string
}

// Plan returns the rules and tasks needed for reqs, dependencies first.
// exists reports whether a source file exists; an input that is neither
// produced by a rule nor exists is an error. Check must have succeeded.
func (g *Graph) Plan(reqs []Request, exists func(string) bool) ([]Step, error) {
	var steps []Step
	done := map[*Rule]bool{}
	doneTask := map[string]bool{}

	var file func(path, neededBy string) error
	var rule func(r *Rule) error
	var task func(t *Task, args []string) error

	file = func(p, neededBy string) error {
		if r, ok := g.Producer[p]; ok {
			return rule(r)
		}
		if exists(p) {
			return nil
		}
		if neededBy == "" {
			return fmt.Errorf("no rule to make %s", p)
		}
		return fmt.Errorf("no rule to make %s, needed by %s", p, neededBy)
	}
	rule = func(r *Rule) error {
		if done[r] {
			return nil
		}
		done[r] = true
		for _, in := range r.Inputs() {
			if err := file(in, r.Name()); err != nil {
				return err
			}
		}
		steps = append(steps, Step{Rule: r})
		return nil
	}
	task = func(t *Task, args []string) error {
		key := t.Name + "\x00" + strings.Join(args, "\x00")
		if doneTask[key] {
			return nil
		}
		doneTask[key] = true
		for _, n := range t.Needs {
			if nt, ok := g.Tasks[n]; ok {
				if len(nt.Params) > 0 {
					return fmt.Errorf("task %s needs task %s, which takes parameters", t.Name, nt.Name)
				}
				if err := task(nt, nil); err != nil {
					return err
				}
				continue
			}
			if err := file(n, t.Name); err != nil {
				return err
			}
		}
		steps = append(steps, Step{Task: t, Args: args})
		return nil
	}

	for _, rq := range reqs {
		if t, ok := g.Tasks[rq.Name]; ok {
			if len(rq.Args) != len(t.Params) {
				return nil, fmt.Errorf("task %s takes %d argument(s), got %d", t.Name, len(t.Params), len(rq.Args))
			}
			if err := task(t, rq.Args); err != nil {
				return nil, err
			}
			continue
		}
		if err := file(rq.Name, ""); err != nil {
			return nil, err
		}
	}
	return steps, nil
}

// WriteDOT writes the graph (or the part reachable from targets) in DOT
// format. Rules are boxes, tasks are ellipses, source files are plain text.
func (g *Graph) WriteDOT(w io.Writer, targets []string) error {
	var ns []node
	if len(targets) == 0 {
		ns = g.nodes()
	} else {
		seen := map[node]bool{}
		var visit func(n node)
		visit = func(n node) {
			if seen[n] {
				return
			}
			seen[n] = true
			ns = append(ns, n)
			for _, d := range g.deps(n) {
				visit(d)
			}
		}
		for _, t := range targets {
			switch {
			case g.Tasks[t] != nil:
				visit(node{task: g.Tasks[t]})
			case g.Producer[t] != nil:
				visit(node{rule: g.Producer[t]})
			default:
				return fmt.Errorf("unknown target %s", t)
			}
		}
	}
	q := func(s string) string {
		return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
	}
	b := &strings.Builder{}
	b.WriteString("digraph rmake {\n  rankdir=LR;\n  node [fontsize=10];\n")
	sources := map[string]bool{}
	for _, n := range ns {
		if n.task != nil {
			fmt.Fprintf(b, "  %s [shape=ellipse];\n", q("task "+n.task.Name))
			for _, d := range n.task.Needs {
				from := d
				if g.Tasks[d] != nil {
					from = "task " + d
				} else if g.Producer[d] == nil {
					sources[d] = true
				}
				fmt.Fprintf(b, "  %s -> %s;\n", q(from), q("task "+n.task.Name))
			}
			continue
		}
		r := n.rule
		for _, o := range r.Outs {
			fmt.Fprintf(b, "  %s [shape=box];\n", q(o))
		}
		for _, in := range r.Inputs() {
			if g.Producer[in] == nil {
				sources[in] = true
			}
			for _, o := range r.Outs {
				fmt.Fprintf(b, "  %s -> %s;\n", q(in), q(o))
			}
		}
	}
	srcs := make([]string, 0, len(sources))
	for s := range sources {
		srcs = append(srcs, s)
	}
	sort.Strings(srcs)
	for _, s := range srcs {
		fmt.Fprintf(b, "  %s [shape=plaintext];\n", q(s))
	}
	b.WriteString("}\n")
	_, err := io.WriteString(w, b.String())
	return err
}
