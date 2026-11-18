// Command rmake is a content-hash based build tool that can import Makefiles.
// See README.md for the specification.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"sort"
	"strings"

	"github.com/rveen/rmake/internal/eval"
	"github.com/rveen/rmake/internal/graph"
	"github.com/rveen/rmake/internal/syntax"
)

// Exit codes (README §9).
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

func main() {
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
	rest := args
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
	g, err := eval.Load(eval.Options{File: file, Overrides: overrides, LookupEnv: os.LookupEnv})
	if err != nil {
		fmt.Fprintln(stderr, "rmake:", err)
		return exitUsage
	}

	switch o.tool {
	case "":
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
	case "why", "query", "clean":
		fmt.Fprintf(stderr, "rmake: -t %s is not implemented yet (milestone M2)\n", o.tool)
		return exitUsage
	default:
		fmt.Fprintf(stderr, "rmake: unknown tool %q\n", o.tool)
		return exitUsage
	}

	reqs, err := requests(g, rest)
	if err != nil {
		fmt.Fprintln(stderr, "rmake:", err)
		return exitUsage
	}
	steps, err := g.Plan(reqs, exists)
	if err != nil {
		fmt.Fprintln(stderr, "rmake:", err)
		return exitUsage
	}
	if !o.dry {
		fmt.Fprintln(stderr, "rmake: building is not implemented yet (milestone M2); use -n")
		return exitFailure
	}
	return dryRun(steps, stdout, stderr)
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

func exists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
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

// dryRun prints what would run, and why. Without a state store (M2) every
// rule would run because it has no build record.
func dryRun(steps []graph.Step, stdout, stderr io.Writer) int {
	for _, s := range steps {
		var cmds []graph.Cmd
		if s.Task != nil {
			c, _, err := s.Task.Expand(s.Args)
			if err != nil {
				fmt.Fprintln(stderr, "rmake:", err)
				return exitUsage
			}
			cmds = c
			fmt.Fprintf(stdout, "# task %s: tasks always run\n", strings.Join(append([]string{s.Task.Name}, s.Args...), " "))
		} else {
			cmds = s.Rule.Cmds
			fmt.Fprintf(stdout, "# %s: no build record\n", strings.Join(s.Rule.Outs, " "))
		}
		for _, c := range cmds {
			fmt.Fprintln(stdout, c.String())
		}
	}
	return exitOK
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
