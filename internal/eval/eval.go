// Package eval evaluates a parsed build file (and its includes) into a
// concrete build graph (README §5).
//
// Evaluation is a pure function of the set of outputs declared so far, which
// globs under $builddir match against. Load evaluates repeatedly until that
// set stops changing; this resolves globs over declared outputs and chains of
// pattern rules regardless of declaration order.
package eval

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"runtime"
	"sort"
	"strings"

	"github.com/rveen/rmake/internal/glob"
	"github.com/rveen/rmake/internal/graph"
	"github.com/rveen/rmake/internal/syntax"
)

// Options configure Load.
type Options struct {
	Root      string                      // project root directory; default "."
	File      string                      // root build file relative to Root; default "Rmakefile"
	Overrides map[string][]string         // command-line name=value assignments
	LookupEnv func(string) (string, bool) // environment for ?= variables; nil: none
	OS, Arch  string                      // default runtime.GOOS / GOARCH
}

// maxPasses bounds the fixpoint iteration.
const maxPasses = 32

// Load evaluates the build file into a checked graph.
func Load(opts Options) (*graph.Graph, error) {
	if opts.Root == "" {
		opts.Root = "."
	}
	if opts.File == "" {
		opts.File = "Rmakefile"
	}
	if opts.OS == "" {
		opts.OS = runtime.GOOS
	}
	if opts.Arch == "" {
		opts.Arch = runtime.GOARCH
	}
	l := &loader{opts: opts, parsed: map[string]*syntax.File{}, disk: map[string][]string{}}
	known := map[string]bool{}
	for range maxPasses {
		p := &pass{loader: l, known: known, g: graph.New()}
		if err := p.run(); err != nil {
			return nil, err
		}
		if p.stable() || sameKeys(p.g.Producer, known) {
			if err := p.finish(); err != nil {
				return nil, err
			}
			return p.g, nil
		}
		known = map[string]bool{}
		for o := range p.g.Producer {
			known[o] = true
		}
	}
	return nil, fmt.Errorf("%s: globs over $builddir do not reach a fixed point after %d passes", opts.File, maxPasses)
}

type globResult struct {
	pat     string
	matches []string
}

// stable reports whether this pass is a fixed point: every glob under
// $builddir matches the same outputs against the final set of declared
// outputs as it did during the pass. Evaluation is deterministic given the
// glob results, so another pass would produce the same graph.
func (p *pass) stable() bool {
	for _, gr := range p.builddirGlobs {
		n := 0
		for o := range p.g.Producer {
			if glob.Match(gr.pat, o) {
				n++
			}
		}
		// Matches during the pass were a subset of known ∪ declared; with
		// the same count against the final outputs they are the same set,
		// provided none came only from known.
		if n != len(gr.matches) {
			return false
		}
		for _, m := range gr.matches {
			if p.g.Producer[m] == nil {
				return false
			}
		}
	}
	return true
}

func sameKeys(m map[string]*graph.Rule, set map[string]bool) bool {
	if len(m) != len(set) {
		return false
	}
	for k := range m {
		if !set[k] {
			return false
		}
	}
	return true
}

// loader holds state shared by all passes.
type loader struct {
	opts   Options
	parsed map[string]*syntax.File
	disk   map[string][]string // disk glob cache
}

// pass is one evaluation of the whole build.
type pass struct {
	*loader
	known         map[string]bool // outputs declared in the previous pass
	g             *graph.Graph
	builddir      string
	locked        bool         // builddir may no longer change
	builddirGlobs []globResult // globs under builddir evaluated in this pass
	stack         []string
	first         string // first non-pattern rule output or task in the root file
	defaults      []pending
	needs         map[*graph.Task][]pending
	includes      []included
}

// included is a file named by an include statement.
type included struct {
	pos     syntax.Pos
	rel     string
	missing bool // did not exist; read as empty
}

// pending is a target name resolved after evaluation: a task name or a path
// relative to dir.
type pending struct {
	pos  syntax.Pos
	name string
	dir  string
}

// fileCtx is per build file.
type fileCtx struct {
	dir  string // directory of the file, root-relative ("" for the root)
	root bool   // the root build file
}

func (p *pass) run() error {
	sc := newScope(nil)
	sc.vars["os"] = []string{p.opts.OS}
	sc.vars["arch"] = []string{p.opts.Arch}
	p.builddir = "build"
	for k, v := range p.opts.Overrides {
		sc.vars[k] = v
	}
	if v, ok := p.opts.Overrides["builddir"]; ok {
		bd, err := checkBuilddir(v)
		if err != nil {
			return fmt.Errorf("command line: %v", err)
		}
		p.builddir = bd
	}
	sc.vars["builddir"] = []string{p.builddir}
	p.needs = map[*graph.Task][]pending{}
	return p.file(path.Clean(p.opts.File), sc, true)
}

func checkBuilddir(v []string) (string, error) {
	if len(v) != 1 {
		return "", fmt.Errorf("builddir must be exactly one word")
	}
	bd := path.Clean(v[0])
	if bd == "." || path.IsAbs(bd) || bd == ".." || strings.HasPrefix(bd, "../") || glob.HasMeta(bd) {
		return "", fmt.Errorf("builddir must be a relative directory below the project root, not %q", v[0])
	}
	return bd, nil
}

func (p *pass) parse(rel string) (*syntax.File, error) {
	if f, ok := p.parsed[rel]; ok {
		return f, nil
	}
	src, err := os.ReadFile(path.Join(p.opts.Root, rel))
	if err != nil {
		return nil, err
	}
	f, err := syntax.Parse(rel, src)
	if err != nil {
		return nil, err
	}
	p.parsed[rel] = f
	return f, nil
}

func (p *pass) file(rel string, sc *scope, root bool) error {
	for i, s := range p.stack {
		if s == rel {
			return fmt.Errorf("include cycle: %s -> %s", strings.Join(p.stack[i:], " -> "), rel)
		}
	}
	f, err := p.parse(rel)
	if err != nil {
		return err
	}
	p.stack = append(p.stack, rel)
	defer func() { p.stack = p.stack[:len(p.stack)-1] }()
	dir := path.Dir(rel)
	if dir == "." {
		dir = ""
	}
	return p.stmts(f.Stmts, sc, &fileCtx{dir: dir, root: root})
}

func (p *pass) stmts(ss []syntax.Stmt, sc *scope, ctx *fileCtx) error {
	for _, s := range ss {
		var err error
		switch s := s.(type) {
		case *syntax.Var:
			err = p.assign(s, sc, ctx, false)
		case *syntax.Include:
			err = p.include(s, sc, ctx)
		case *syntax.Default:
			var ts []string
			ts, err = expandWords(s.Targets, sc)
			for _, t := range ts {
				p.defaults = append(p.defaults, pending{s.Pos, t, ctx.dir})
			}
		case *syntax.Rule:
			err = p.rule(s, sc, ctx)
		case *syntax.Task:
			err = p.task(s, sc, ctx)
		case *syntax.If:
			var ok bool
			ok, err = cond(s, sc)
			if err == nil {
				if ok {
					err = p.stmts(s.Then, sc, ctx)
				} else {
					err = p.stmts(s.Else, sc, ctx)
				}
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func cond(s *syntax.If, sc *scope) (bool, error) {
	if s.Op == "defined" {
		_, ok := sc.lookup(s.Defined)
		return ok, nil
	}
	l, err := expandJoined([]syntax.Word{s.Left}, sc)
	if err != nil {
		return false, err
	}
	r, err := expandJoined([]syntax.Word{s.Right}, sc)
	if err != nil {
		return false, err
	}
	return (l == r) == (s.Op == "=="), nil
}

// assign evaluates a var statement or attribute. local marks rule/task
// attributes.
func (p *pass) assign(v *syntax.Var, sc *scope, ctx *fileCtx, local bool) error {
	name := v.Name
	if _, ok := p.opts.Overrides[name]; ok {
		return nil // command-line values win over every definition
	}
	switch name {
	case "os", "arch":
		return syntax.Errorf(v.Pos, "cannot assign built-in variable %s", name)
	case "in", "out", "stem":
		if local {
			return syntax.Errorf(v.Pos, "cannot assign %s", name)
		}
	case "builddir":
		if local || p.locked {
			return syntax.Errorf(v.Pos, "builddir may only be assigned at top level, before the first rule or include")
		}
	}
	if v.Op == "?=" {
		if _, ok := sc.lookup(name); ok {
			return nil
		}
		if p.opts.LookupEnv != nil {
			if s, ok := p.opts.LookupEnv(name); ok {
				sc.vars[name] = strings.Fields(s)
				return p.setBuilddir(v, sc)
			}
		}
	}
	val, err := p.value(v, sc, ctx)
	if err != nil {
		return err
	}
	if v.Op == "+=" {
		base, _ := sc.lookup(name)
		val = append(append([]string(nil), base...), val...)
	}
	sc.vars[name] = val
	return p.setBuilddir(v, sc)
}

func (p *pass) setBuilddir(v *syntax.Var, sc *scope) error {
	if v.Name != "builddir" {
		return nil
	}
	bd, err := checkBuilddir(sc.vars["builddir"])
	if err != nil {
		return syntax.Errorf(v.Pos, "%v", err)
	}
	p.builddir = bd
	sc.vars["builddir"] = []string{bd}
	return nil
}

func (p *pass) value(v *syntax.Var, sc *scope, ctx *fileCtx) ([]string, error) {
	vals, err := expandWords(v.Values, sc)
	if err != nil || !v.Glob {
		return vals, err
	}
	var out []string
	for _, pat := range vals {
		m, err := p.glob(p.rel(ctx.dir, pat), v.Pos)
		if err != nil {
			return nil, err
		}
		out = append(out, m...)
	}
	return out, nil
}

// inBuilddir reports whether the root-relative path p is under $builddir.
func (p *pass) inBuilddir(s string) bool {
	return s == p.builddir || strings.HasPrefix(s, p.builddir+"/")
}

// rel makes a path written in a file in dir root-relative. Absolute paths
// and paths under $builddir are already root-relative (README §5.5).
func (p *pass) rel(dir, s string) string {
	c := path.Clean(s)
	if path.IsAbs(c) || dir == "" || p.inBuilddir(c) {
		return c
	}
	return path.Join(dir, c)
}

// glob expands a root-relative pattern (README §5.4).
func (p *pass) glob(pat string, pos syntax.Pos) ([]string, error) {
	if !glob.Valid(pat) {
		return nil, syntax.Errorf(pos, "invalid glob pattern %q", pat)
	}
	if p.inBuilddir(pat) {
		set := map[string]bool{}
		for o := range p.known {
			if glob.Match(pat, o) {
				set[o] = true
			}
		}
		for o := range p.g.Producer {
			if glob.Match(pat, o) {
				set[o] = true
			}
		}
		out := make([]string, 0, len(set))
		for o := range set {
			out = append(out, o)
		}
		sort.Strings(out)
		p.builddirGlobs = append(p.builddirGlobs, globResult{pat, out})
		return out, nil
	}
	key := p.builddir + "\x00" + pat
	if m, ok := p.disk[key]; ok {
		return m, nil
	}
	bd := p.builddir
	m := glob.Disk(p.opts.Root, pat, func(rel string) bool { return rel == bd || rel == ".rmake" })
	p.disk[key] = m
	return m, nil
}

// inputs makes input words root-relative and expands globs among them.
func (p *pass) inputs(ws []string, dir string, pos syntax.Pos) ([]string, error) {
	var out []string
	for _, w := range ws {
		r := p.rel(dir, w)
		if !glob.HasMeta(r) {
			out = append(out, r)
			continue
		}
		m, err := p.glob(r, pos)
		if err != nil {
			return nil, err
		}
		out = append(out, m...)
	}
	return out, nil
}

func isMakefile(rel string) bool {
	switch b := path.Base(rel); b {
	case "Makefile", "makefile", "GNUmakefile":
		return true
	default:
		return strings.HasSuffix(b, ".mk")
	}
}

func (p *pass) include(s *syntax.Include, sc *scope, ctx *fileCtx) error {
	p.locked = true
	v, err := expandWord(s.Path, sc)
	if err != nil {
		return err
	}
	if len(v) != 1 {
		return syntax.Errorf(s.Pos, "include path expands to %d words, want one", len(v))
	}
	rel := p.rel(ctx.dir, v[0])
	if isMakefile(rel) {
		return syntax.Errorf(s.Pos, "including Makefiles is not implemented yet (milestone M6)")
	}
	if _, err := os.Stat(path.Join(p.opts.Root, rel)); errors.Is(err, fs.ErrNotExist) {
		// It may be generated by a rule (declared anywhere): read it as
		// empty; finish checks that a rule produces it.
		p.includes = append(p.includes, included{s.Pos, rel, true})
		return nil
	}
	p.includes = append(p.includes, included{s.Pos, rel, false})
	if err := p.file(rel, newScope(sc), false); err != nil {
		var se *syntax.Error
		if errors.As(err, &se) {
			return err
		}
		return syntax.Errorf(s.Pos, "include %s: %v", rel, err)
	}
	return nil
}

func (p *pass) rule(r *syntax.Rule, sc *scope, ctx *fileCtx) error {
	p.locked = true
	outs, err := expandWords(r.Outs, sc)
	if err != nil {
		return err
	}
	ins, err := expandWords(r.Ins, sc)
	if err != nil {
		return err
	}
	imps, err := expandWords(r.Implicit, sc)
	if err != nil {
		return err
	}
	if len(outs) == 0 {
		return syntax.Errorf(r.Pos, "rule outputs expand to nothing")
	}
	pattern := false
	for _, o := range outs {
		if o == "" {
			return syntax.Errorf(r.Pos, "empty output path")
		}
		if glob.HasMeta(o) {
			return syntax.Errorf(r.Pos, "output %s contains glob characters", o)
		}
		if strings.Contains(o, "%") {
			pattern = true
		}
	}
	for i := range outs {
		outs[i] = p.rel(ctx.dir, outs[i])
	}
	if !pattern {
		for _, in := range append(append([]string(nil), ins...), imps...) {
			if strings.Contains(in, "%") {
				return syntax.Errorf(r.Pos, "input %s contains %% but no output does", in)
			}
		}
		ins, err := p.inputs(ins, ctx.dir, r.Pos)
		if err != nil {
			return err
		}
		imps, err := p.inputs(imps, ctx.dir, r.Pos)
		if err != nil {
			return err
		}
		return p.instantiate(r, sc, ctx, outs, ins, imps, nil)
	}

	for _, o := range outs {
		if strings.Count(o, "%") != 1 {
			return syntax.Errorf(r.Pos, "pattern rule output %s must contain exactly one %%", o)
		}
	}
	if len(ins) == 0 || strings.Count(ins[0], "%") != 1 {
		return syntax.Errorf(r.Pos, "pattern rule needs exactly one %% in its first input")
	}
	for _, in := range append(append([]string(nil), ins...), imps...) {
		if strings.Count(in, "%") > 1 {
			return syntax.Errorf(r.Pos, "input %s contains more than one %%", in)
		}
	}
	first := p.rel(ctx.dir, ins[0])
	if glob.HasMeta(first) {
		return syntax.Errorf(r.Pos, "the first input of a pattern rule cannot contain glob characters")
	}
	i := strings.IndexByte(first, '%')
	pre, suf := first[:i], first[i+1:]
	matches, err := p.glob(pre+"*"+suf, r.Pos)
	if err != nil {
		return err
	}
	for _, m := range matches {
		stem := m[len(pre) : len(m)-len(suf)]
		sub := func(ws []string) []string {
			out := make([]string, len(ws))
			for k, w := range ws {
				out[k] = strings.Replace(w, "%", stem, 1)
			}
			return out
		}
		cins, err := p.inputs(sub(ins), ctx.dir, r.Pos)
		if err != nil {
			return err
		}
		cimps, err := p.inputs(sub(imps), ctx.dir, r.Pos)
		if err != nil {
			return err
		}
		if err := p.instantiate(r, sc, ctx, sub(outs), cins, cimps, &stem); err != nil {
			return err
		}
	}
	return nil
}

// instantiate expands a rule's attributes for concrete paths and adds it.
func (p *pass) instantiate(r *syntax.Rule, sc *scope, ctx *fileCtx, outs, ins, imps []string, stem *string) error {
	rs := newScope(sc)
	rs.vars["in"] = ins
	rs.vars["out"] = outs
	gr := &graph.Rule{Pos: r.Pos, Outs: outs, Ins: ins, Implicit: imps}
	if stem != nil {
		rs.vars["stem"] = []string{*stem}
		gr.Stem = *stem
	}
	cmds, env, err := p.attrs(r.Attrs, rs, ctx, gr)
	if err != nil {
		return err
	}
	if len(cmds) == 0 {
		return syntax.Errorf(r.Pos, "rule has no run or sh command")
	}
	gr.Cmds, gr.Env = cmds, env
	if gr.Depfile != "" && gr.Deps == "" {
		return syntax.Errorf(r.Pos, "depfile requires deps")
	}
	if gr.Deps != "" && gr.Depfile == "" {
		gr.Depfile = outs[0] + ".d"
	}
	if err := p.g.AddRule(gr); err != nil {
		return err
	}
	if ctx.root && p.first == "" && stem == nil {
		p.first = outs[0] // pattern rules never provide the default (as in make)
	}
	return nil
}

// attrs evaluates rule or task attributes in scope rs. Local variables are
// evaluated first, in order, then the remaining attributes. gr is nil for
// tasks; rule-only attributes are stored in it.
func (p *pass) attrs(as []*syntax.Attr, rs *scope, ctx *fileCtx, gr *graph.Rule) ([]graph.Cmd, map[string]string, error) {
	for _, a := range as {
		if a.Kind == "var" {
			if err := p.assign(a.Var, rs, ctx, true); err != nil {
				return nil, nil, err
			}
		}
	}
	var cmds []graph.Cmd
	var env map[string]string
	seen := map[string]bool{}
	for _, a := range as {
		switch a.Kind {
		case "desc", "deps", "depfile", "always":
			if seen[a.Kind] {
				return nil, nil, syntax.Errorf(a.Pos, "duplicate %s", a.Kind)
			}
			seen[a.Kind] = true
		}
		switch a.Kind {
		case "run":
			argv, err := expandWords(a.Args, rs)
			if err != nil {
				return nil, nil, err
			}
			if len(argv) == 0 || argv[0] == "" {
				return nil, nil, syntax.Errorf(a.Pos, "command expands to nothing")
			}
			cmds = append(cmds, graph.Cmd{Pos: a.Pos, Argv: argv})
		case "sh":
			s, err := expandJoined(a.Args, rs)
			if err != nil {
				return nil, nil, err
			}
			cmds = append(cmds, graph.Cmd{Pos: a.Pos, Shell: true, Script: s})
		case "pass":
			for _, w := range a.Args {
				v, ok := rs.lookup(w.Raw)
				if !ok {
					return nil, nil, syntax.Errorf(w.Pos, "pass: undefined variable %s", w.Raw)
				}
				if env == nil {
					env = map[string]string{}
				}
				env[w.Raw] = strings.Join(v, " ")
			}
		case "desc", "depfile":
			s, err := expandJoined(a.Args, rs)
			if err != nil {
				return nil, nil, err
			}
			if gr == nil {
				continue // task desc is handled by the caller
			}
			if a.Kind == "desc" {
				gr.Desc = s
			} else {
				gr.Depfile = path.Clean(s)
			}
		case "deps":
			gr.Deps = a.Args[0].Raw
		case "always":
			gr.Always = true
		}
	}
	return cmds, env, nil
}

func (p *pass) task(t *syntax.Task, sc *scope, ctx *fileCtx) error {
	p.locked = true
	needs, err := expandWords(t.Needs, sc)
	if err != nil {
		return err
	}
	snap := sc.snapshot()
	params := t.Params
	expand := func(args []string) (*scope, []graph.Cmd, map[string]string, error) {
		ts := newScope(snap)
		for i, name := range params {
			ts.vars[name] = []string{args[i]}
		}
		cmds, env, err := p.attrs(t.Attrs, ts, ctx, nil)
		return ts, cmds, env, err
	}
	// Check the task now, with placeholder arguments.
	ph := make([]string, len(params))
	for i, name := range params {
		ph[i] = "<" + name + ">"
	}
	ts, _, _, err := expand(ph)
	if err != nil {
		return err
	}
	gt := &graph.Task{Pos: t.Pos, Name: t.Name, Params: params}
	for _, a := range t.Attrs {
		if a.Kind == "desc" {
			gt.Desc, _ = expandJoined(a.Args, ts)
		}
	}
	gt.Expand = func(args []string) ([]graph.Cmd, map[string]string, error) {
		if len(args) != len(params) {
			return nil, nil, fmt.Errorf("task %s takes %d argument(s), got %d", t.Name, len(params), len(args))
		}
		_, cmds, env, err := expand(args)
		return cmds, env, err
	}
	if err := p.g.AddTask(gt); err != nil {
		return err
	}
	for _, n := range needs {
		p.needs[gt] = append(p.needs[gt], pending{t.Pos, n, ctx.dir})
	}
	if ctx.root && p.first == "" {
		p.first = t.Name
	}
	return nil
}

// finish resolves deferred target names and runs the graph checks.
func (p *pass) finish() error {
	resolve := func(pd pending) string {
		if _, ok := p.g.Tasks[pd.name]; ok {
			return pd.name
		}
		return p.rel(pd.dir, pd.name)
	}
	for _, d := range p.defaults {
		p.g.Defaults = append(p.g.Defaults, resolve(d))
	}
	if len(p.g.Defaults) == 0 && p.first != "" {
		p.g.Defaults = []string{p.first}
	}
	for t, ns := range p.needs {
		for _, n := range ns {
			t.Needs = append(t.Needs, resolve(n))
		}
	}
	seen := map[string]bool{}
	for _, inc := range p.includes {
		switch {
		case p.g.Producer[inc.rel] != nil:
			if !seen[inc.rel] {
				seen[inc.rel] = true
				p.g.Generated = append(p.g.Generated, inc.rel)
			}
		case inc.missing:
			return syntax.Errorf(inc.pos, "include %s: no such file", inc.rel)
		}
	}
	return p.g.Check()
}
