// Package execx runs rule and task commands (README §5.6): argv commands
// without a shell, sh commands via /bin/sh -c, a filtered environment, and
// combined output capture.
package execx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rveen/rmake/internal/graph"
)

// Shell is the shell used for sh commands.
const Shell = "/bin/sh"

// maxArg is the longest sh script passed with -c; longer ones go to stdin.
const maxArg = 64 << 10

// keep lists the environment variables passed to every command.
var keep = map[string]bool{
	"PATH": true, "HOME": true, "TMPDIR": true, "TEMP": true, "TMP": true,
	"LANG": true, "TERM": true, "SYSTEMROOT": true,
}

// BaseEnv filters environ (KEY=VALUE pairs) down to the variables every
// command sees.
func BaseEnv(environ []string) []string {
	var out []string
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if keep[k] || strings.HasPrefix(k, "LC_") {
			out = append(out, kv)
		}
	}
	return out
}

// Env returns base plus the pass'ed variables (which take precedence).
func Env(base []string, pass map[string]string) []string {
	if len(pass) == 0 {
		return base
	}
	out := make([]string, 0, len(base)+len(pass))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := pass[k]; !ok {
			out = append(out, kv)
		}
	}
	keys := make([]string, 0, len(pass))
	for k := range pass {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+pass[k])
	}
	return out
}

// Resolver resolves argv[0] to an absolute path, caching PATH lookups for the
// duration of a run. It is safe for concurrent use.
type Resolver struct {
	root  string
	mu    sync.Mutex
	cache map[string]resolved
}

type resolved struct {
	path string
	err  error
}

// NewResolver returns a resolver for commands run in root.
func NewResolver(root string) *Resolver {
	return &Resolver{root: root, cache: map[string]resolved{}}
}

// Resolve returns the absolute path of the program a command runs.
func (r *Resolver) Resolve(c graph.Cmd) (string, error) {
	name := Shell
	if !c.Shell {
		name = c.Argv[0]
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok := r.cache[name]; ok {
		return v.path, v.err
	}
	var v resolved
	if strings.Contains(name, "/") {
		p := name
		if !filepath.IsAbs(p) {
			p = filepath.Join(r.root, p)
		}
		v.path, v.err = filepath.Abs(p)
	} else {
		v.path, v.err = exec.LookPath(name)
		if v.err == nil {
			v.path, v.err = filepath.Abs(v.path)
		}
	}
	if v.err != nil {
		v.err = fmt.Errorf("%s: command not found", name)
	}
	r.cache[name] = v
	return v.path, v.err
}

// Result is the outcome of running a command.
type Result struct {
	Output   []byte // combined stdout and stderr
	ExitCode int    // -1 if the command could not be started or was killed
}

// Interrupt is the cancellation cause (context.WithCancelCause) that asks Run
// to deliver Signal to the command's process group. Any other cause kills
// the group.
type Interrupt struct{ Signal os.Signal }

func (i Interrupt) Error() string { return "interrupted by " + i.Signal.String() }

// KillDelay is how long a cancelled command's process group may take to
// exit after the Interrupt signal before it is killed.
var KillDelay = 2 * time.Second

// WaitDelay is how long Run waits for a command's output after the command
// exited or was cancelled, before it kills the command and stops waiting.
// (A background process that keeps the output open would otherwise block
// the build.)
var WaitDelay = 10 * time.Second

// Run runs c in dir with env, in a process group of its own. A command that
// a relative argv[0] names (like ./build/app) is resolved against dir, not
// the current directory. When ctx is cancelled the process group receives
// the signal of an Interrupt cause, else SIGKILL.
func Run(ctx context.Context, r *Resolver, c graph.Cmd, dir string, env []string) (Result, error) {
	prog, err := r.Resolve(c)
	if err != nil {
		return Result{ExitCode: -1}, err
	}
	var cmd *exec.Cmd
	switch {
	case c.Shell && len(c.Script) > maxArg:
		// Too long for one argument (Linux limits each to 128 KB): feed
		// the script on stdin instead.
		cmd = exec.CommandContext(ctx, prog, "-s")
		cmd.Stdin = strings.NewReader(c.Script)
	case c.Shell:
		cmd = exec.CommandContext(ctx, prog, "-c", c.Script)
	default:
		cmd = exec.CommandContext(ctx, prog, c.Argv[1:]...)
		cmd.Args[0] = c.Argv[0]
	}
	cmd.Dir = dir
	cmd.Env = env
	exited := ownGroup(ctx, cmd)
	defer exited()
	cmd.WaitDelay = WaitDelay
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err = cmd.Start(); err == nil {
		track(cmd, true)
		err = cmd.Wait()
		track(cmd, false)
	}
	res := Result{Output: out.Bytes()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		res.ExitCode = ee.ExitCode()
		err = fmt.Errorf("exit status %d", res.ExitCode)
		if res.ExitCode < 0 {
			err = fmt.Errorf("%s", ee.ProcessState.String())
		}
	default:
		res.ExitCode = -1
	}
	return res, err
}

// Environ is os.Environ, replaceable in tests.
var Environ = os.Environ
