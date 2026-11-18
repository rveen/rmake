package execx

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/rveen/rmake/internal/graph"
)

func TestBaseEnv(t *testing.T) {
	got := BaseEnv([]string{"PATH=/bin", "SECRET=x", "LC_ALL=C", "HOME=/h", "LCX=no"})
	want := []string{"PATH=/bin", "LC_ALL=C", "HOME=/h"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q", got)
	}
	got = Env(want, map[string]string{"HOME": "/other", "cc": "clang"})
	want = []string{"PATH=/bin", "LC_ALL=C", "HOME=/other", "cc=clang"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q", got)
	}
}

func TestRun(t *testing.T) {
	r := NewResolver(t.TempDir())
	ctx := context.Background()
	res, err := Run(ctx, r, graph.Cmd{Argv: []string{"sh", "-c", "echo out; echo err >&2"}}, ".", nil)
	if err != nil || string(res.Output) != "out\nerr\n" {
		t.Errorf("run: %v %q", err, res.Output)
	}
	res, err = Run(ctx, r, graph.Cmd{Shell: true, Script: "exit 7"}, ".", nil)
	if err == nil || res.ExitCode != 7 {
		t.Errorf("exit: %v %d", err, res.ExitCode)
	}
	long := "true" + strings.Repeat(" ", 200<<10) + "; echo long"
	res, err = Run(ctx, r, graph.Cmd{Shell: true, Script: long}, ".", nil)
	if err != nil || string(res.Output) != "long\n" {
		t.Errorf("long script: %v %q", err, res.Output)
	}
	_, err = Run(ctx, r, graph.Cmd{Argv: []string{"no-such-tool-xyz"}}, ".", nil)
	if err == nil || !strings.Contains(err.Error(), "command not found") {
		t.Errorf("not found: %v", err)
	}
	p, err := r.Resolve(graph.Cmd{Argv: []string{"./build/app"}})
	if err != nil || !strings.HasSuffix(p, "/build/app") || p[0] != '/' {
		t.Errorf("relative: %s %v", p, err)
	}
}
