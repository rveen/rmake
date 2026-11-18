// Command rmake is a content-hash based build tool that can import Makefiles.
// See README.md for the specification.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"time"

	"github.com/rveen/rmake/internal/eval"
	"github.com/rveen/rmake/internal/execx"
	"github.com/rveen/rmake/internal/graph"
	"github.com/rveen/rmake/internal/hash"
	"github.com/rveen/rmake/internal/sched"
	"github.com/rveen/rmake/internal/state"
	"github.com/rveen/rmake/internal/syntax"
)

// Exit codes (README §9).
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

func main() {
	// RMAKE_CPUPROFILE=file writes a CPU profile, for performance work.
	if f := os.Getenv("RMAKE_CPUPROFILE"); f != "" {
		if w, err := os.Create(f); err == nil {
			pprof.StartCPUProfile(w)
			code := run(os.Args[1:], os.Stdout, os.Stderr)
			pprof.StopCPUProfile()
			w.Close()
			os.Exit(code)
		}
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

type options struct {
	dir     string
	file    string
	jobs    int
	keep    bool
	dry     bool
	always  bool
	verbose bool
	tool    string
}

func run(args []string, stdout, stderr io.Writer) int {
	var o options
	fs := flag.NewFlagSet("rmake", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.dir, "C", "", "change to `dir` first")
	fs.StringVar(&o.file, "f", "", "build `file` (default: Rmakefile)")
	fs.IntVar(&o.jobs, "j", runtime.NumCPU(), "parallel `jobs`")
	fs.BoolVar(&o.keep, "k", false, "keep going after failures")
	fs.BoolVar(&o.dry, "n", false, "dry run: list what would run, and why")
	fs.BoolVar(&o.always, "B", false, "treat every rule as out of date")
	fs.BoolVar(&o.verbose, "v", false, "print every command before running it")
	fs.StringVar(&o.tool, "t", "", "run `tool`: why, query, graph, tasks, targets, clean, fmt, import")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: rmake [flags] [name=value ...] [target|task [args...] ...]")
		fs.PrintDefaults()
	}
	// Flags and name=value overrides may be interleaved up to the first
	// target or task.
	overrides := map[string][]string{}
	rest := makeStyleFlags(args)
	for {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return exitOK
			}
			return exitUsage
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		name, value, ok := strings.Cut(rest[0], "=")
		if !ok || !syntax.IsName(name) {
			break
		}
		overrides[name] = strings.Fields(value)
		rest = rest[1:]
	}

	if o.dir != "" {
		if err := os.Chdir(o.dir); err != nil {
			fmt.Fprintln(stderr, "rmake:", err)
			return exitUsage
		}
	}

	// The fmt tool only parses; it does not need a loadable build.
	if o.tool == "fmt" {
		return toolFmt(o, rest, stdout, stderr)
	}
	if o.tool == "import" {
		fmt.Fprintln(stderr, "rmake: -t import is not implemented yet (milestone M6)")
		return exitUsage
	}

	file, err := buildFile(o.file)
	if err != nil {
		fmt.Fprintln(stderr, "rmake:", err)
		return exitUsage
	}
	timing := newTimer(stderr)
	defer timing.done()

	// Open the state store while the build file is evaluated.
	type opened struct {
		st  state.Store
		err error
	}
	var stateCh chan opened
	switch o.tool {
	case "", "why", "query", "clean":
		stateCh = make(chan opened, 1)
		go func() {
			st, err := state.Open(".rmake")
			stateCh <- opened{st, err}
		}()
	}
	defer func() {
		if stateCh != nil { // never received: release the store
			if r := <-stateCh; r.st != nil {
				r.st.Close()
			}
		}
	}()

	g, err := eval.Load(eval.Options{File: file, Overrides: overrides, LookupEnv: os.LookupEnv})
	timing.mark("load build file")
	if err != nil {
		fmt.Fprintln(stderr, "rmake:", err)
		return exitUsage
	}

	switch o.tool {
	case "", "why", "query", "clean":
	case "targets":
		for _, t := range g.Targets() {
			fmt.Fprintln(stdout, t)
		}
		return exitOK
	case "tasks":
		return toolTasks(g, stdout)
	case "graph":
		if err := g.WriteDOT(stdout, cleanAll(rest)); err != nil {
			fmt.Fprintln(stderr, "rmake:", err)
			return exitUsage
		}
		return exitOK
	default:
		fmt.Fprintf(stderr, "rmake: unknown tool %q\n", o.tool)
		return exitUsage
	}

	so := <-stateCh
	stateCh = nil
	st := so.st
	timing.mark("open state")
	if so.err != nil {
		fmt.Fprintln(stderr, "rmake:", so.err)
		return exitFailure
	}
	defer st.Close()
	b := &sched.Builder{
		G:       g,
		Store:   st,
		Hash:    hash.New(".", st),
		Res:     execx.NewResolver("."),
		Root:    ".",
		BaseEnv: execx.BaseEnv(os.Environ()),
		Out:     stdout,
		Verbose: o.verbose,
		Force:   o.always,
		Keep:    o.keep,
		DryRun:  o.dry,
	}
	// Stat every file of the graph concurrently; planning and the rebuild
	// decisions then use the results.
	b.Hash.Prestat(g.Files())
	exists := b.Hash.Exists
	timing.mark("stat files")

	switch o.tool {
	case "why", "query":
		if len(rest) != 1 {
			fmt.Fprintf(stderr, "rmake: -t %s takes one target\n", o.tool)
			return exitUsage
		}
		t := path.Clean(rest[0])
		if o.tool == "why" {
			err = b.Why(stdout, t, exists)
		} else {
			err = b.Query(stdout, t)
		}
		if err != nil {
			fmt.Fprintln(stderr, "rmake:", err)
			return exitUsage
		}
		return exitOK
	case "clean":
		n, err := b.Clean(stdout)
		if err != nil {
			fmt.Fprintln(stderr, "rmake:", err)
			return exitFailure
		}
		fmt.Fprintf(stdout, "rmake: removed %d file(s)\n", n)
		if err := st.Flush(); err != nil {
			fmt.Fprintln(stderr, "rmake:", err)
			return exitFailure
		}
		return exitOK
	}

	reqs, err := requests(g, rest)
	if err != nil {
		fmt.Fprintln(stderr, "rmake:", err)
		return exitUsage
	}
	steps, err := g.Plan(reqs, exists)
	timing.mark("plan")
	if err != nil {
		fmt.Fprintln(stderr, "rmake:", err)
		return exitUsage
	}
	err = b.Build(context.Background(), steps)
	timing.mark("build")
	if ferr := st.Flush(); ferr != nil && err == nil {
		err = ferr
	}
	timing.mark("flush state")
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, sched.ErrFailed):
		return exitFailure
	default:
		fmt.Fprintln(stderr, "rmake:", err)
		return exitFailure
	}
}

// timer prints phase durations to stderr when RMAKE_TIMING is set.
type timer struct {
	w           io.Writer
	start, last time.Time
}

func newTimer(w io.Writer) *timer {
	if os.Getenv("RMAKE_TIMING") == "" {
		return nil
	}
	now := time.Now()
	return &timer{w: w, start: now, last: now}
}

func (t *timer) mark(phase string) {
	if t == nil {
		return
	}
	now := time.Now()
	fmt.Fprintf(t.w, "rmake: %-16s %6.1f ms\n", phase, float64(now.Sub(t.last).Microseconds())/1000)
	t.last = now
}

func (t *timer) done() {
	if t != nil {
		fmt.Fprintf(t.w, "rmake: %-16s %6.1f ms\n", "total", float64(time.Since(t.start).Microseconds())/1000)
	}
}

// makeStyleFlags rewrites make-style -jN into -j=N, which the flag package
// understands.
func makeStyleFlags(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if len(a) > 2 && strings.HasPrefix(a, "-j") && a[2] >= '0' && a[2] <= '9' {
			a = "-j=" + a[2:]
		}
		out[i] = a
	}
	return out
}

// buildFile picks the build file: -f, else Rmakefile, else (later) an
// imported Makefile.
func buildFile(f string) (string, error) {
	if f != "" {
		return f, nil
	}
	if _, err := os.Stat("Rmakefile"); err == nil {
		return "Rmakefile", nil
	}
	for _, m := range []string{"GNUmakefile", "makefile", "Makefile"} {
		if _, err := os.Stat(m); err == nil {
			return "", fmt.Errorf("no Rmakefile; importing %s is not implemented yet (milestone M6)", m)
		}
	}
	return "", errors.New("no Rmakefile found")
}

func cleanAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = path.Clean(s)
	}
	return out
}

// requests turns positional arguments into targets and task invocations. A
// task's parameters consume the following arguments.
func requests(g *graph.Graph, args []string) ([]graph.Request, error) {
	if len(args) == 0 {
		args = g.Defaults
		if len(args) == 0 {
			return nil, errors.New("nothing to build: no targets given and no default")
		}
		for _, a := range args {
			if t := g.Tasks[a]; t != nil && len(t.Params) > 0 {
				return nil, fmt.Errorf("default task %s takes parameters", a)
			}
		}
	}
	var reqs []graph.Request
	for len(args) > 0 {
		name := args[0]
		args = args[1:]
		if t, ok := g.Tasks[name]; ok {
			if len(args) < len(t.Params) {
				return nil, fmt.Errorf("task %s takes %d argument(s) (%s), got %d",
					name, len(t.Params), strings.Join(t.Params, ", "), len(args))
			}
			reqs = append(reqs, graph.Request{Name: name, Args: args[:len(t.Params)]})
			args = args[len(t.Params):]
			continue
		}
		reqs = append(reqs, graph.Request{Name: path.Clean(name)})
	}
	return reqs, nil
}

func toolTasks(g *graph.Graph, w io.Writer) int {
	ts := append([]*graph.Task(nil), g.TaskList...)
	sort.Slice(ts, func(i, j int) bool { return ts[i].Name < ts[j].Name })
	for _, t := range ts {
		sig := t.Name
		if len(t.Params) > 0 {
			sig += "(" + strings.Join(t.Params, ", ") + ")"
		}
		if t.Desc != "" {
			fmt.Fprintf(w, "%-24s %s\n", sig, t.Desc)
		} else {
			fmt.Fprintln(w, sig)
		}
	}
	return exitOK
}

func toolFmt(o options, args []string, stdout, stderr io.Writer) int {
	file := o.file
	if len(args) > 0 {
		file = args[0]
	}
	if file == "" {
		file = "Rmakefile"
	}
	src, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintln(stderr, "rmake:", err)
		return exitUsage
	}
	f, err := syntax.Parse(file, src)
	if err != nil {
		fmt.Fprintln(stderr, "rmake:", err)
		return exitUsage
	}
	stdout.Write(syntax.Format(f))
	return exitOK
}
