//go:build !unix

package execx

import (
	"context"
	"os/exec"
)

// Windows (job objects) is deferred (M8): cancellation kills only the
// command itself.
func ownGroup(ctx context.Context, cmd *exec.Cmd) (exited func()) { return func() {} }

func track(cmd *exec.Cmd, start bool) {}

// KillAll is a no-op without process groups.
func KillAll() {}
