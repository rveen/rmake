package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const releaseBuild = `default build/app
rule build/app from src/main.c
  sh "echo $out >> ran.log; cp $in $out"
task test needs build/app
  sh "echo test >> ran.log"
task release(version) needs test
  desc "tag release $version"
  sh "echo release $version >> ran.log"
task lint
  sh "echo lint >> ran.log"
task all needs lint test
  sh "echo all >> ran.log"
task fail
  sh "echo fail >> ran.log; exit 1"
task after needs fail
  sh "echo after >> ran.log"
task say(a, b)
  run sh -c "echo say $$1 $$2 >> ran.log" sh $a $b
`

// order runs rmake and returns the lines of ran.log in order.
func (p *proj) order(args ...string) (int, string, []string) {
	p.t.Helper()
	os.Remove(filepath.Join(p.dir, "ran.log"))
	code, out, errs := runIn(p.t, p.dir, args...)
	var ran []string
	for _, l := range strings.Split(strings.TrimSpace(p.read("ran.log")), "\n") {
		if l != "" {
			ran = append(ran, l)
		}
	}
	return code, out + errs, ran
}

func TestTasks(t *testing.T) {
	p := newProj(t, map[string]string{"Rmakefile": releaseBuild, "src/main.c": "main\n"})

	// rmake release 1.0: the file, then test, then release, whose desc
	// shows the argument.
	code, out, ran := p.order("-j16", "release", "1.0")
	if code != 0 || !reflect.DeepEqual(ran, []string{"build/app", "test", "release 1.0"}) {
		t.Fatalf("release: %d %q\n%s", code, ran, out)
	}
	if !strings.Contains(out, "] tag release 1.0\n") {
		t.Errorf("desc not expanded:\n%s", out)
	}
	// Tasks are never cached; the file is.
	if _, out, ran = p.order("release", "1.0"); !reflect.DeepEqual(ran, []string{"test", "release 1.0"}) {
		t.Errorf("second release: %q\n%s", ran, out)
	}

	// A task runs after everything it needs, also with -j.
	_, out, ran = p.order("-j16", "all")
	if len(ran) != 3 || ran[2] != "all" {
		t.Errorf("all: %q\n%s", ran, out)
	}

	// The same invocation twice runs once; different arguments run twice.
	// Arguments are literal: no expansion, and name=value after a task is
	// an argument, not an override.
	_, out, ran = p.order("-j1", "say", "x", "y", "say", "x", "y", "say", "a$b", "k=v")
	if !reflect.DeepEqual(ran, []string{"say x y", "say a$b k=v"}) {
		t.Errorf("say: %q\n%s", ran, out)
	}
	if code, out, _ = p.order("say", "x"); code != 2 || !strings.Contains(out, "task say takes 2 argument(s) (a, b), got 1") {
		t.Errorf("missing argument: %d\n%s", code, out)
	}

	// A failed task fails the build and blocks what needs it.
	code, out, ran = p.order("after")
	if code != 1 || !reflect.DeepEqual(ran, []string{"fail"}) || !strings.Contains(out, "FAILED: task fail") {
		t.Errorf("fail: %d %q\n%s", code, ran, out)
	}

	// -n lists tasks with their arguments and runs nothing.
	code, out, ran = p.order("-n", "release", "2.0")
	if code != 0 || ran != nil || !strings.Contains(out, "# task release 2.0: tasks always run\necho release 2.0 >> ran.log\n") {
		t.Errorf("-n: %d %q\n%s", code, ran, out)
	}

	// -t tasks lists parameters and the desc with placeholders.
	_, out, _ = p.order("-t", "tasks")
	if !strings.Contains(out, "release(version)         tag release <version>\n") || !strings.Contains(out, "say(a, b)\n") {
		t.Errorf("-t tasks:\n%s", out)
	}
}

func TestVariablePrecedence(t *testing.T) {
	// Command line > environment (only for ?=) > the file.
	p := newProj(t, map[string]string{
		"Rmakefile": `var a ?= file
var b ?= file
var c = file
var d ?= file
var d = later
task show
  sh "echo $a $b $c $d >> ran.log"
`,
	})
	t.Setenv("a", "env")
	t.Setenv("b", "env")
	t.Setenv("c", "env")
	// a: env beats ?=; b: the command line beats env; c: env is never read
	// for =; d: the command line makes every definition a no-op.
	_, out, ran := p.order("b=cli", "d=cli", "show")
	if !reflect.DeepEqual(ran, []string{"env cli file cli"}) {
		t.Errorf("got %q\n%s", ran, out)
	}
	_, out, ran = p.order("show")
	if !reflect.DeepEqual(ran, []string{"env env file later"}) {
		t.Errorf("without overrides: %q\n%s", ran, out)
	}
}
