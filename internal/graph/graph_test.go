package graph

import (
	"strings"
	"testing"
)

func rule(outs string, ins ...string) *Rule {
	return &Rule{Outs: strings.Fields(outs), Ins: ins, Cmds: []Cmd{{Argv: []string{"true"}}}}
}

func build(t *testing.T, rules ...*Rule) *Graph {
	t.Helper()
	g := New()
	for _, r := range rules {
		if err := g.AddRule(r); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

func TestDuplicateOutput(t *testing.T) {
	g := New()
	if err := g.AddRule(rule("a b", "x")); err != nil {
		t.Fatal(err)
	}
	err := g.AddRule(rule("b", "y"))
	if err == nil || !strings.Contains(err.Error(), "b is produced by two rules") {
		t.Errorf("got %v", err)
	}
}

func TestCycle(t *testing.T) {
	g := build(t, rule("a", "b"), rule("b", "c"), rule("c", "a"))
	err := g.Check()
	if err == nil || !strings.Contains(err.Error(), "dependency cycle: a -> b -> c -> a") {
		t.Errorf("got %v", err)
	}
}

func TestTaskCycleAndCollision(t *testing.T) {
	g := New()
	_ = g.AddTask(&Task{Name: "x", Needs: []string{"y"}})
	_ = g.AddTask(&Task{Name: "y", Needs: []string{"x"}})
	if err := g.Check(); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("got %v", err)
	}
	g = build(t, rule("test", "src"))
	_ = g.AddTask(&Task{Name: "test"})
	if err := g.Check(); err == nil || !strings.Contains(err.Error(), "same name as a file") {
		t.Errorf("got %v", err)
	}
}

func TestPlan(t *testing.T) {
	g := build(t, rule("app", "a.o", "b.o"), rule("a.o", "a.c"), rule("b.o", "b.c"))
	_ = g.AddTask(&Task{Name: "test", Needs: []string{"app"}})
	_ = g.AddTask(&Task{Name: "release", Params: []string{"v"}, Needs: []string{"test"}})
	if err := g.Check(); err != nil {
		t.Fatal(err)
	}
	src := map[string]bool{"a.c": true, "b.c": true}
	steps, err := g.Plan([]Request{{Name: "release", Args: []string{"1.0"}}}, func(p string) bool { return src[p] })
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range steps {
		if s.Task != nil {
			names = append(names, "task:"+s.Task.Name)
		} else {
			names = append(names, s.Rule.Name())
		}
	}
	if got := strings.Join(names, " "); got != "a.o b.o app task:test task:release" {
		t.Errorf("plan: %s", got)
	}

	delete(src, "b.c")
	_, err = g.Plan([]Request{{Name: "app"}}, func(p string) bool { return src[p] })
	if err == nil || err.Error() != "no rule to make b.c, needed by b.o" {
		t.Errorf("got %v", err)
	}
	_, err = g.Plan([]Request{{Name: "release"}}, func(string) bool { return true })
	if err == nil || !strings.Contains(err.Error(), "takes 1 argument") {
		t.Errorf("got %v", err)
	}
}

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{"a.c": "a.c", "": "''", "a b": "'a b'", "it's": `'it'\''s'`, "-DX=1": "-DX=1"} {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestDOT(t *testing.T) {
	g := build(t, rule("app", "a.o"), rule("a.o", "a.c"))
	var b strings.Builder
	if err := g.WriteDOT(&b, []string{"app"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"a.o" -> "app"`, `"a.c" -> "a.o"`, `"a.c" [shape=plaintext]`} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("DOT lacks %s:\n%s", want, b.String())
		}
	}
}
