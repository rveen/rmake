package main

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// proj is a temporary project. Every rule appends its first output to
// ran.log, so tests can see exactly which rules ran.
type proj struct {
	t   *testing.T
	dir string
}

func newProj(t *testing.T, files map[string]string) *proj {
	t.Helper()
	wd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(wd) })
	p := &proj{t: t, dir: t.TempDir()}
	for name, c := range files {
		p.write(name, c)
	}
	return p
}

func (p *proj) write(name, content string) {
	p.t.Helper()
	full := filepath.Join(p.dir, name)
	os.MkdirAll(filepath.Dir(full), 0o755)
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		p.t.Fatal(err)
	}
}

func (p *proj) read(name string) string {
	b, _ := os.ReadFile(filepath.Join(p.dir, name))
	return string(b)
}

func (p *proj) exists(name string) bool {
	_, err := os.Stat(filepath.Join(p.dir, name))
	return err == nil
}

// build runs rmake and returns the exit code, the output and the rules that
// ran (sorted).
func (p *proj) build(args ...string) (int, string, []string) {
	p.t.Helper()
	os.Remove(filepath.Join(p.dir, "ran.log"))
	code, out, errs := runIn(p.t, p.dir, args...)
	var ran []string
	for _, l := range strings.Split(strings.TrimSpace(p.read("ran.log")), "\n") {
		if l != "" {
			ran = append(ran, l)
		}
	}
	sort.Strings(ran)
	return code, out + errs, ran
}

func (p *proj) expect(what string, wantRan []string, args ...string) string {
	p.t.Helper()
	code, out, ran := p.build(args...)
	if code != 0 {
		p.t.Fatalf("%s: exit %d\n%s", what, code, out)
	}
	if !reflect.DeepEqual(ran, wantRan) {
		p.t.Fatalf("%s: ran %q, want %q\n%s", what, ran, wantRan, out)
	}
	return out
}

const cbuild = `var cflags ?= -O2
rule build/%.o from src/%.c | src/config.h
  sh "echo $out >> ran.log; (echo $cflags; cat $in) > $out"
rule build/app from build/*.o
  sh "echo $out >> ran.log; cat $in > $out"
`

func cproj(t *testing.T) *proj {
	return newProj(t, map[string]string{
		"Rmakefile":    cbuild,
		"src/a.c":      "a\n",
		"src/b.c":      "b\n",
		"src/config.h": "h\n",
	})
}

func TestNoOpAndTouch(t *testing.T) {
	p := cproj(t)
	p.expect("first build", []string{"build/a.o", "build/app", "build/b.o"})
	if got := p.read("build/app"); got != "-O2\na\n-O2\nb\n" {
		t.Errorf("app = %q", got)
	}
	out := p.expect("second build", nil)
	if !strings.Contains(out, "no work to do") {
		t.Errorf("no-op output: %q", out)
	}
	// Touch without content change.
	future := time.Now().Add(time.Hour)
	for _, f := range []string{"src/a.c", "src/config.h", "build/a.o"} {
		os.Chtimes(filepath.Join(p.dir, f), future, future)
	}
	p.expect("after touch", nil)
}

func TestChanges(t *testing.T) {
	p := cproj(t)
	p.expect("first build", []string{"build/a.o", "build/app", "build/b.o"})

	// Changing a variable used in a command reruns exactly the rules using it.
	p.expect("flag change", []string{"build/a.o", "build/app", "build/b.o"}, "cflags=-O0")
	p.expect("same flags again", nil, "cflags=-O0")

	// Content change of one source.
	p.write("src/a.c", "a2\n")
	p.expect("source change", []string{"build/a.o", "build/app"}, "cflags=-O0")

	// An implicit input reruns every rule that lists it.
	p.write("src/config.h", "h2\n")
	p.expect("implicit input change", []string{"build/a.o", "build/b.o"}, "cflags=-O0")
	if strings.Contains(p.read("build/a.o"), "h2") {
		t.Error("implicit input appeared in $in")
	}

	// Externally modified output.
	p.write("build/b.o", "tampered\n")
	p.expect("modified output", []string{"build/b.o"}, "cflags=-O0")

	// Deleted output.
	os.Remove(filepath.Join(p.dir, "build/app"))
	p.expect("deleted output", []string{"build/app"}, "cflags=-O0")
}

func TestEarlyCutoff(t *testing.T) {
	// gen.txt is derived from the first line of in.txt only; a change to
	// the second line rebuilds gen.txt identically, so final.txt is skipped.
	p := newProj(t, map[string]string{
		"Rmakefile": `default final.txt
rule gen.txt from in.txt
  sh "echo $out >> ran.log; head -n 1 $in > $out"
rule final.txt from gen.txt
  sh "echo $out >> ran.log; cat $in $in > $out"
`,
		"in.txt": "one\ntwo\n",
	})
	p.expect("first", []string{"final.txt", "gen.txt"})
	p.write("in.txt", "one\nchanged\n")
	p.expect("identical intermediate", []string{"gen.txt"})
	p.write("in.txt", "uno\n")
	p.expect("changed intermediate", []string{"final.txt", "gen.txt"})
}

func TestFailure(t *testing.T) {
	p := newProj(t, map[string]string{
		"Rmakefile": `rule bad.txt from
  sh "echo $out >> ran.log; echo partial > $out; echo oops >&2; exit 3"
rule good.txt from
  sh "echo $out >> ran.log; echo ok > $out"
rule all.txt from bad.txt good.txt
  sh "echo $out >> ran.log; cat $in > $out"
`,
	})
	code, out, ran := p.build("all.txt")
	if code != 1 || !reflect.DeepEqual(ran, []string{"bad.txt"}) {
		t.Fatalf("exit %d, ran %q\n%s", code, ran, out)
	}
	for _, want := range []string{"FAILED: bad.txt", "oops", "exit status 3"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if p.exists("bad.txt") {
		t.Error("failed rule left its output")
	}
	// -k builds everything that does not depend on the failure.
	code, out, ran = p.build("-k", "all.txt")
	if code != 1 || !reflect.DeepEqual(ran, []string{"bad.txt", "good.txt"}) {
		t.Fatalf("-k: exit %d, ran %q\n%s", code, ran, out)
	}
	// No record was written for the failed rule.
	_, out, _ = p.build("-t", "query", "bad.txt")
	if !strings.Contains(out, "no build record") {
		t.Errorf("query after failure:\n%s", out)
	}
	// A rule that does not create its output fails.
	p.write("Rmakefile", "rule x.txt from\n  sh \"true\"\n")
	code, out, _ = p.build()
	if code != 1 || !strings.Contains(out, "did not create x.txt") {
		t.Errorf("missing output: %d\n%s", code, out)
	}
}

func TestDryRunWhyQueryClean(t *testing.T) {
	p := cproj(t)
	code, out, ran := p.build("-n")
	if code != 0 || ran != nil || !strings.Contains(out, "# build/a.o: no build record") ||
		!strings.Contains(out, "input build/a.o will be rebuilt") {
		t.Fatalf("-n: %d %q\n%s", code, ran, out)
	}
	if p.exists("build/a.o") {
		t.Fatal("-n built something")
	}
	p.expect("build", []string{"build/a.o", "build/app", "build/b.o"})

	p.write("src/b.c", "b2\n")
	_, out, _ = p.build("cflags=-O3", "-t", "why", "build/b.o")
	for _, want := range []string{"build/b.o would rebuild", "input src/b.c changed", "command 1 changed", "-O3"} {
		if !strings.Contains(out, want) {
			t.Errorf("why lacks %q:\n%s", want, out)
		}
	}
	_, out, _ = p.build("-t", "why", "build/app")
	if !strings.Contains(out, "input build/b.o will be rebuilt") {
		t.Errorf("why app:\n%s", out)
	}
	_, out, _ = p.build("-t", "why", "build/a.o")
	if !strings.Contains(out, "build/a.o is up to date") || !strings.Contains(out, "because: no build record") {
		t.Errorf("why up to date:\n%s", out)
	}
	_, out, _ = p.build("-t", "query", "build/a.o")
	for _, want := range []string{"defined at: Rmakefile:2:1", "src/a.c", "implicit inputs:", "src/config.h", "last built:", "key:"} {
		if !strings.Contains(out, want) {
			t.Errorf("query lacks %q:\n%s", want, out)
		}
	}

	p.write("src/keep.txt", "not an output\n")
	_, out, _ = p.build("-t", "clean")
	if !strings.Contains(out, "removed 3 file(s)") || p.exists("build/app") || !p.exists("src/a.c") || !p.exists("src/keep.txt") {
		t.Errorf("clean:\n%s", out)
	}
	p.expect("rebuild after clean", []string{"build/a.o", "build/app", "build/b.o"})
}

func TestTasksAlwaysRunAndEnv(t *testing.T) {
	p := newProj(t, map[string]string{
		"Rmakefile": `var greeting = hello
rule out.txt from
  pass greeting
  sh "echo $out >> ran.log; echo \"$$greeting $$HOME_IS_FILTERED\" > $out"
task say(name) needs out.txt
  sh "echo task >> ran.log; echo $name"
`,
	})
	os.Setenv("HOME_IS_FILTERED", "leak")
	defer os.Unsetenv("HOME_IS_FILTERED")
	p.expect("first", []string{"out.txt", "task"}, "say", "bob")
	if got := p.read("out.txt"); got != "hello \n" {
		t.Errorf("env: %q", got)
	}
	out := p.expect("second", []string{"task"}, "say", "bob")
	if !strings.Contains(out, "bob\n") {
		t.Errorf("task output: %q", out)
	}
	// Changing a pass'ed variable reruns the rule.
	p.expect("pass change", []string{"out.txt"}, "greeting=hi", "out.txt")
}
