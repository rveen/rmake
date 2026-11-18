//go:build unix

package execx

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// running holds the process groups of the commands Run is waiting for.
var running struct {
	sync.Mutex
	pgids map[int]bool
}

// ownGroup makes cmd the leader of a new process group and makes
// cancellation signal the whole group. A group that is still running
// KillDelay after the signal is killed. It returns a function to call when
// the command has exited.
func ownGroup(ctx context.Context, cmd *exec.Cmd) (exited func()) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	done := make(chan struct{})
	cmd.Cancel = func() error {
		sig := syscall.SIGKILL
		var in Interrupt
		if errors.As(context.Cause(ctx), &in) {
			if s, ok := in.Signal.(syscall.Signal); ok {
				sig = s
			}
		}
		pgid := cmd.Process.Pid
		if sig != syscall.SIGKILL {
			// A process that was being forked when the signal came may
			// have missed it.
			go func() {
				select {
				case <-done:
				case <-time.After(KillDelay):
					syscall.Kill(-pgid, syscall.SIGKILL)
				}
			}()
		}
		return syscall.Kill(-pgid, sig)
	}
	return func() { close(done) }
}

func track(cmd *exec.Cmd, start bool) {
	running.Lock()
	defer running.Unlock()
	if running.pgids == nil {
		running.pgids = map[int]bool{}
	}
	if start {
		running.pgids[cmd.Process.Pid] = true
	} else {
		delete(running.pgids, cmd.Process.Pid)
	}
}

// KillAll sends SIGKILL to the process groups of all running commands (for
// a second interrupt).
func KillAll() {
	running.Lock()
	defer running.Unlock()
	for pgid := range running.pgids {
		syscall.Kill(-pgid, syscall.SIGKILL)
	}
}
