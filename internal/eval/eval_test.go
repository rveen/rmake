package eval

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rveen/rmake/internal/graph"
)

// setup writes files (path -> content) into a temp dir and returns it.
func setup(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for p, c := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func load(t *testing.T, files map[string]string, opts Options) *graph.Graph {
	t.Helper()
	g, err := loadErr(t, files, opts)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func loadErr(t *testing.T, files map[string]string, opts Options) (*graph.Graph, error) {
	t.Helper()
	opts.Root = setup(t, files)
	if opts.OS == "" {
		opts.OS, opts.Arch = "linux", "amd64"
	}
	return Load(opts)
}

func cmds(r *graph.Rule) []string {
	var out []string
	for _, c := range r.Cmds {
		out = append(out, c.String())
	}
	return out
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s:\n got %#v\nwant %#v", what, got, want)
	}
}

const example = `var cc ?= cc
var cflags ?= -O2 -Wall
var srcs = glob src/*.c

if $os == windows
  var exe = .exe
else
  var exe = ""
end

default $builddir/app$exe

rule $builddir/%.o from src/%.c | src/config.h
  desc "cc $stem"
  deps gcc
  run  $cc $cflags -MD -MF $out.d -c $in -o $out

rule $builddir/app$exe from ${srcs:src/%.c=$builddir/%.o}
  run $cc -o $out $in

rule bin/server from
  always
  run go build -o $out ./cmd/server

task test needs $builddir/app$exe bin/server
  desc "run all tests"
  run  ./$builddir/app$exe --selftest
  run  go test ./...

task release(version) needs test
  desc "tag release $version"
  run git tag v$version
`

func TestExample(t *testing.T) {
	files := map[string]string{"Rmakefile": example, "src/a.c": "", "src/b.c": "", "src/config.h": ""}
	g := load(t, files, Options{})
	eq(t, "targets", g.Targets(), []string{"bin/server", "build/a.o", "build/app", "build/b.o"})
	eq(t, "defaults", g.Defaults, []string{"build/app"})
	a := g.Producer["build/a.o"]
	eq(t, "a.o ins", a.Ins, []string{"src/a.c"})
	eq(t, "a.o implicit", a.Implicit, []string{"src/config.h"})
	eq(t, "a.o cmds", cmds(a), []string{"cc -O2 -Wall -MD -MF build/a.o.d -c src/a.c -o build/a.o"})
	eq(t, "a.o desc", a.Desc, "cc a")
	eq(t, "a.o depfile", a.Depfile, "build/a.o.d")
	app := g.Producer["build/app"]
	eq(t, "app ins", app.Ins, []string{"build/a.o", "build/b.o"})
	if !g.Producer["bin/server"].Always {
		t.Error("bin/server not always")
	}
	rel := g.Tasks["release"]
	c, _, desc, err := rel.Expand([]string{"1.4.0"})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "release", c[0].String(), "git tag v1.4.0")
	eq(t, "release desc", desc, "tag release 1.4.0")
	eq(t, "listed desc", rel.Desc, "tag release <version>")
	eq(t, "test needs", g.Tasks["test"].Needs, []string{"build/app", "bin/server"})

	// On windows, exe is .exe.
	g = load(t, files, Options{OS: "windows", Arch: "amd64"})
	eq(t, "windows defaults", g.Defaults, []string{"build/app.exe"})
}

func TestConcatenation(t *testing.T) {
	src := `var empty =
var one = ""
var incs = a b
var two = x y
rule out from
  run cc -I$incs -L$empty app$one "q $incs q" "$empty" ${incs}.h
`
	g := load(t, map[string]string{"Rmakefile": src}, Options{})
	eq(t, "argv", g.Producer["out"].Cmds[0].Argv,
		[]string{"cc", "-Ia", "-Ib", "app", "q a b q", "", "a.h", "b.h"})

	_, err := loadErr(t, map[string]string{"Rmakefile": src + "  run $incs$two\n"}, Options{})
	if err == nil || !strings.Contains(err.Error(), "Rmakefile:7:12: cannot concatenate a list of 2 words with a list of 2 words") {
		t.Errorf("got %v", err)
	}
}

func TestSubstitution(t *testing.T) {
	src := `var srcs = src/a.c src/b.c lib/c.h
var b = out
var x = ${srcs:src/%.c=$b/%.o}
var y = ${srcs:.c=.o}
var z = ${srcs:%=-I%}
rule o from
  run echo $x $y $z
`
	g := load(t, map[string]string{"Rmakefile": src}, Options{})
	eq(t, "argv", g.Producer["o"].Cmds[0].Argv, []string{"echo",
		"out/a.o", "out/b.o", "lib/c.h",
		"src/a.o", "src/b.o", "lib/c.h",
		"-Isrc/a.c", "-Isrc/b.c", "-Ilib/c.h"})
}

func TestPrecedence(t *testing.T) {
	src := `var a ?= file
var b ?= file
var c ?= file
var d = file
var d += more
rule o from
  run echo $a $b $c $d
`
	env := map[string]string{"b": "env", "c": "env words", "d": "env"}
	opts := Options{
		Overrides: map[string][]string{"a": {"cli"}, "c": {"cli"}},
		LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
	}
	g := load(t, map[string]string{"Rmakefile": src}, opts)
	// a: CLI; b: env over ?=; c: CLI over env; d: plain = ignores env.
	eq(t, "argv", g.Producer["o"].Cmds[0].Argv, []string{"echo", "cli", "env", "cli", "file", "more"})

	g = load(t, map[string]string{"Rmakefile": src}, Options{Overrides: map[string][]string{"d": {"cli"}}})
	eq(t, "CLI disables +=", g.Producer["o"].Cmds[0].Argv, []string{"echo", "file", "file", "file", "cli"})
}

func TestErrors(t *testing.T) {
	tests := []struct{ src, want string }{
		{"var x = $y\n", "Rmakefile:1:9: undefined variable y"},
		{"var x = $in\n", "Rmakefile:1:9: $in is only defined inside rules"},
		{"rule a from b\n  run echo $stem\n", "Rmakefile:2:12: $stem is only defined inside pattern rules"},
		{"rule a from b\n  desc x\n", "Rmakefile:1:1: rule has no run or sh command"},
		{"rule a from b\n  run x\nrule a from c\n  run y\n", "Rmakefile:3:1: a is produced by two rules (other rule at Rmakefile:1:1)"},
		{"rule a from b\n  run x\nrule b from a\n  run y\n", "dependency cycle: a -> b -> a"},
		{"rule test from\n  run x\ntask test\n  run y\n", "same name as a file"},
		{"rule a/%.o from b/*.c\n  run x\n", "exactly one % in its first input"},
		{"rule a/%.o from b/%.c\n  run x\nvar builddir = out\n", "builddir may only be assigned"},
		{"var os = x\n", "cannot assign built-in variable os"},
		{"rule a* from b\n  run x\n", "output a* contains glob characters"},
		{"rule a from b\n  depfile x\n  run x\n", "depfile requires deps"},
		{"rule a from b\n  desc x\n  desc y\n  run x\n", "Rmakefile:3:3: duplicate desc"},
		{"include sub/Makefile\n", "not implemented yet (milestone M6)"},
		{"include nope\n", "include nope: no such file"},
		{"include Rmakefile\n", "include cycle: Rmakefile -> Rmakefile"},
		{"task t\n  run echo $nope\n", "Rmakefile:2:12: undefined variable nope"},
		{"task t(os)\n  run echo\n", "Rmakefile:1:1: task t: parameter os would hide the built-in variable"},
		{"task t(in)\n  run echo\n", "parameter in would hide"},
		{"task t(a, b, a)\n  run echo\n", "Rmakefile:1:1: task t: duplicate parameter a"},
		{"var builddir = ../x\n", "builddir must be a relative directory"},
	}
	for _, tt := range tests {
		_, err := loadErr(t, map[string]string{"Rmakefile": tt.src}, Options{})
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%q:\n got %v\nwant %s", tt.src, err, tt.want)
		}
	}
}

func TestBuilddirGlobIgnoresStaleFiles(t *testing.T) {
	src := `rule build/%.o from src/%.c
  run cc -c $in -o $out
rule build/app from build/*.o
  run cc -o $out $in
`
	files := map[string]string{"Rmakefile": src, "src/a.c": "", "build/stale.o": ""}
	g := load(t, files, Options{})
	eq(t, "app ins", g.Producer["build/app"].Ins, []string{"build/a.o"})
}

func TestPatternChainOrderIndependent(t *testing.T) {
	// The link rule comes first, and .o files come from .c files that are
	// themselves generated from .y files.
	src := `var objs = glob $builddir/*.o
rule $builddir/app from $objs
  run ld -o $out $in
rule $builddir/%.o from $builddir/%.c
  run cc -c $in -o $out
rule $builddir/%.c from src/%.y
  run yacc -o $out $in
rule $builddir/main.c from main.c
  run cp $in $out
`
	files := map[string]string{"Rmakefile": src, "src/p.y": "", "src/q.y": "", "main.c": ""}
	g := load(t, files, Options{})
	eq(t, "app ins", g.Producer["build/app"].Ins, []string{"build/main.o", "build/p.o", "build/q.o"})
	eq(t, "defaults", g.Defaults, []string{"build/app"})
}

func TestIncludeScopingAndPaths(t *testing.T) {
	root := `var cflags = -O2
var top = 1
include lib/Rmakefile
rule build/app from build/lib.a main.c
  run ld $cflags -o $out $in
`
	sub := `var cflags += -fPIC
var local = x
rule $builddir/lib.a from *.c gen/h.h
  run ar $cflags $out $in
task libtest needs $builddir/lib.a check.txt
  run echo $local $top
`
	files := map[string]string{
		"Rmakefile": root, "lib/Rmakefile": sub,
		"lib/a.c": "", "lib/b.c": "", "lib/gen/h.h": "", "lib/check.txt": "", "main.c": "",
	}
	g := load(t, files, Options{})
	lib := g.Producer["build/lib.a"]
	eq(t, "lib ins", lib.Ins, []string{"lib/a.c", "lib/b.c", "lib/gen/h.h"})
	eq(t, "lib cmd", cmds(lib), []string{"ar -O2 -fPIC build/lib.a lib/a.c lib/b.c lib/gen/h.h"})
	eq(t, "app cmd", cmds(g.Producer["build/app"]), []string{"ld -O2 -o build/app build/lib.a main.c"})
	eq(t, "task needs", g.Tasks["libtest"].Needs, []string{"build/lib.a", "lib/check.txt"})
	// The first target comes from the root file only.
	eq(t, "defaults", g.Defaults, []string{"build/app"})

	_, err := loadErr(t, map[string]string{"Rmakefile": "include lib/Rmakefile\nrule x from\n  run echo $local\n", "lib/Rmakefile": "var local = x\n"}, Options{})
	if err == nil || !strings.Contains(err.Error(), "undefined variable local") {
		t.Errorf("include leaked a variable: %v", err)
	}
}

func TestRuleLocalVarsAndDefined(t *testing.T) {
	src := `var cflags = -O2
if defined debug
  var cflags += -g
end
rule a.o from a.c
  run cc $cflags -c $in
  var cflags += -Wno-unused
rule b.o from b.c
  run cc $cflags -c $in
  pass cflags
`
	files := map[string]string{"Rmakefile": src, "a.c": "", "b.c": ""}
	g := load(t, files, Options{})
	eq(t, "a", cmds(g.Producer["a.o"]), []string{"cc -O2 -Wno-unused -c a.c"})
	eq(t, "b", cmds(g.Producer["b.o"]), []string{"cc -O2 -c b.c"})
	eq(t, "b env", g.Producer["b.o"].Env, map[string]string{"cflags": "-O2"})
	g = load(t, files, Options{Overrides: map[string][]string{"debug": {"1"}}})
	eq(t, "b debug", cmds(g.Producer["b.o"]), []string{"cc -O2 -g -c b.c"})
}

func TestShAndQuotes(t *testing.T) {
	src := `var files = a.txt b.txt
rule out.txt from $files
  sh "cat $in > $out"
  desc "concat $in"
`
	files := map[string]string{"Rmakefile": src, "a.txt": "", "b.txt": ""}
	r := load(t, files, Options{}).Producer["out.txt"]
	eq(t, "sh", r.Cmds[0].Script, "cat a.txt b.txt > out.txt")
	eq(t, "desc", r.Desc, "concat a.txt b.txt")
}

func TestTaskSnapshot(t *testing.T) {
	src := `var v = early
task t
  run echo $v
var v = late
`
	g := load(t, map[string]string{"Rmakefile": src}, Options{})
	c, _, _, err := g.Tasks["t"].Expand(nil)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "task", c[0].String(), "echo early")
	eq(t, "default", g.Defaults, []string{"t"})
}
