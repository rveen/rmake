//go:build unix

package execx

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/rveen/rmake/internal/graph"
)

func TestCancelSignalsGroup(t *testing.T) {
	r := NewResolver(t.TempDir())
	// The background sleep keeps the output open: unless the whole group
	// gets the signal, Run waits for WaitDelay.
	script := `trap "echo got TERM; exit 1" TERM; sleep 30 & wait`
	for _, tc := range []struct {
		name  string
		cause error
	}{
		{"interrupt", Interrupt{Signal: syscall.SIGTERM}},
		{"kill", context.Canceled},
	} {
		ctx, cancel := context.WithCancelCause(context.Background())
		time.AfterFunc(100*time.Millisecond, func() { cancel(tc.cause) })
		start := time.Now()
		res, err := Run(ctx, r, graph.Cmd{Shell: true, Script: script}, ".", nil)
		if d := time.Since(start); err == nil || d > 3*time.Second {
			t.Errorf("%s: %v after %v: %q", tc.name, err, d, res.Output)
		}
		if got := string(res.Output) == "got TERM\n"; got != (tc.name == "interrupt") {
			t.Errorf("%s: output %q", tc.name, res.Output)
		}
	}
}
