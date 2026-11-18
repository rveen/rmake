// Package sched decides which rules are out of date and runs them
// (README §4.2, §7).
package sched

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rveen/rmake/internal/execx"
	"github.com/rveen/rmake/internal/graph"
	"github.com/rveen/rmake/internal/hash"
	"github.com/rveen/rmake/internal/state"
)

// KeyVersion is part of every rule key; bump it when the key's composition
// changes so that all rules rebuild.
const KeyVersion = "rmake-key-1"

// maxHeader is the longest command shown in a progress line.
const maxHeader = 200

// ErrFailed is returned by Build when at least one rule or task failed. The
// failures have already been reported.
var ErrFailed = errors.New("build failed")

// ErrInterrupted is returned by Build when its context was cancelled. Running
// commands were stopped and the outputs of their rules deleted.
var ErrInterrupted = errors.New("interrupted")

// Builder runs a plan.
type Builder struct {
	G       *graph.Graph
	Store   state.Store
	Hash    *hash.Cache
	Res     *execx.Resolver
	Root    string
	BaseEnv []string
	Out     io.Writer
	Verbose bool // print every command before running it
	Force   bool // -B: every rule is out of date
	Keep    bool // -k: keep going after failures
	DryRun  bool // -n: print what would run and why
	Jobs    int  // -j: jobs run in parallel (values below 1 mean 1)
	TTY     bool // Out is a terminal: show one status line, overwritten in place
	Width   int  // terminal width for the status line (0: unknown)

	// Ran counts commands started (for tests).
	Ran int
}

// RuleID is the identity of a rule in the state store: a hash of its sorted
// outputs.
func RuleID(r *graph.Rule) string {
	outs := append([]string(nil), r.Outs...)
	sort.Strings(outs)
	h := sha256.Sum256([]byte(strings.Join(outs, "\x00")))
	return hex.EncodeToString(h[:16])
}

// parts are the components of a rule key.
type parts struct {
	cmds       []string
	tools      []string
	env        map[string]string
	inputs     map[string]string
	discovered map[string]string
}

// keyCmd is the unambiguous key form of a command.
func keyCmd(c graph.Cmd) string {
	if c.Shell {
		b, _ := json.Marshal(c.Script)
		return "sh " + string(b)
	}
	b, _ := json.Marshal(c.Argv)
	return string(b)
}

func (p *parts) key() string {
	h := sha256.New()
	var buf []byte
	w := func(tag string, kv ...string) {
		buf = append(buf[:0], tag...)
		for _, s := range kv {
			buf = binary.AppendUvarint(buf, uint64(len(s)))
			buf = append(buf, s...)
		}
		h.Write(buf)
	}
	w("version", KeyVersion)
	for i, c := range p.cmds {
		w("cmd", c, p.tools[i])
	}
	for _, m := range []struct {
		tag string
		m   map[string]string
	}{{"env", p.env}, {"in", p.inputs}, {"disc", p.discovered}} {
		for _, k := range sortedKeys(m.m) {
			w(m.tag, k, m.m[k])
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// missingHash marks a discovered input that no longer exists.
const missingHash = "missing"

// decision is the rebuild decision for one rule.
type decision struct {
	reasons []string // empty: up to date
	parts   parts
	key     string
}

// decide computes r's key and whether it must run. In a dry run, inputs in
// pending (outputs of rules that would run) cannot be hashed yet; they make
// the rule run.
func (b *Builder) decide(r *graph.Rule, pending map[string]bool) (decision, error) {
	var d decision
	p := &d.parts
	for _, c := range r.Cmds {
		p.cmds = append(p.cmds, keyCmd(c))
		tool, err := b.Res.Resolve(c)
		if err != nil && !b.DryRun {
			return d, err
		}
		p.tools = append(p.tools, tool)
	}
	p.env = r.Env
	p.inputs = map[string]string{}
	var waiting []string
	for _, in := range r.Inputs() {
		if pending[in] {
			waiting = append(waiting, in)
			continue
		}
		h, ok, err := b.Hash.Hash(in)
		if err != nil {
			return d, err
		}
		if !ok {
			return d, fmt.Errorf("input %s of %s is missing", in, r.Name())
		}
		p.inputs[in] = h
	}
	rec, haveRec := b.Store.Rule(RuleID(r))
	if haveRec && len(rec.Discovered) > 0 {
		p.discovered = map[string]string{}
		for in := range rec.Discovered {
			if pending[in] {
				waiting = append(waiting, in)
				continue
			}
			h, ok, err := b.Hash.Hash(in)
			if err != nil {
				return d, err
			}
			if !ok {
				h = missingHash
			}
			p.discovered[in] = h
		}
	}
	d.key = p.key()

	why := func(format string, args ...any) { d.reasons = append(d.reasons, fmt.Sprintf(format, args...)) }
	switch {
	case !haveRec:
		why("no build record")
	case b.Force:
		why("forced (-B)")
	case r.Always:
		why("always")
	}
	for _, in := range waiting {
		why("input %s will be rebuilt", in)
	}
	if haveRec {
		for _, o := range r.Outs {
			h, ok, err := b.Hash.Hash(o)
			switch {
			case err != nil:
				return d, err
			case !ok:
				why("output %s is missing", o)
			case h != rec.OutHashes[o]:
				why("output %s was modified outside rmake", o)
			}
		}
		if len(waiting) == 0 && d.key != rec.Key {
			d.reasons = append(d.reasons, explain(rec, p)...)
		}
	}
	return d, nil
}

// explain lists the differences between a record and the current key parts.
func explain(rec *state.Rule, p *parts) []string {
	var out []string
	if len(rec.Cmds) != len(p.cmds) {
		out = append(out, fmt.Sprintf("number of commands changed from %d to %d", len(rec.Cmds), len(p.cmds)))
	} else {
		for i := range p.cmds {
			if rec.Cmds[i] != p.cmds[i] {
				out = append(out, fmt.Sprintf("command %d changed:\n    was: %s\n    now: %s", i+1, rec.Cmds[i], p.cmds[i]))
			} else if i < len(rec.Tools) && rec.Tools[i] != p.tools[i] {
				out = append(out, fmt.Sprintf("command %d now runs %s (was %s)", i+1, p.tools[i], rec.Tools[i]))
			}
		}
	}
	out = append(out, diffMaps("variable", rec.Env, p.env)...)
	out = append(out, diffMaps("input", rec.Inputs, p.inputs)...)
	out = append(out, diffMaps("discovered input", rec.Discovered, p.discovered)...)
	if len(out) == 0 {
		out = append(out, "build key changed (new rmake version?)")
	}
	return out
}

func diffMaps(what string, was, now map[string]string) []string {
	var out []string
	for _, k := range sortedKeys(now) {
		old, ok := was[k]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("new %s %s", what, k))
		case now[k] == missingHash:
			out = append(out, fmt.Sprintf("%s %s is missing", what, k))
		case old != now[k]:
			out = append(out, fmt.Sprintf("%s %s changed", what, k))
		}
	}
	for _, k := range sortedKeys(was) {
		if _, ok := now[k]; !ok {
			out = append(out, fmt.Sprintf("%s %s removed", what, k))
		}
	}
	return out
}

func (b *Builder) abs(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(b.Root, p)
}

func (b *Builder) removeOutputs(r *graph.Rule) {
	for _, o := range r.Outs {
		b.Hash.Forget(o)
		os.Remove(b.abs(o))
	}
}
