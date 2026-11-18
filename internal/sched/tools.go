package sched

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/rveen/rmake/internal/graph"
)

// Why writes why target would rebuild, or why it was built last time
// (rmake -t why).
func (b *Builder) Why(w io.Writer, target string, exists func(string) bool) error {
	if t, ok := b.G.Tasks[target]; ok {
		fmt.Fprintf(w, "%s is a task; tasks always run\n", t.Name)
		return nil
	}
	r, ok := b.G.Producer[target]
	if !ok {
		return fmt.Errorf("%s is not produced by any rule", target)
	}
	steps, err := b.G.Plan([]graph.Request{{Name: target}}, exists)
	if err != nil {
		return err
	}
	b.DryRun = true
	pending := map[string]bool{}
	for _, s := range steps {
		d, err := b.decide(s.Rule, pending)
		if err != nil {
			return fmt.Errorf("%s: %w", s.Rule.Pos, err)
		}
		if s.Rule != r {
			if len(d.reasons) > 0 {
				for _, o := range s.Rule.Outs {
					pending[o] = true
				}
			}
			continue
		}
		if len(d.reasons) > 0 {
			fmt.Fprintf(w, "%s would rebuild:\n", target)
			for _, x := range d.reasons {
				fmt.Fprintf(w, "  - %s\n", x)
			}
			return nil
		}
		rec, _ := b.Store.Rule(RuleID(r))
		fmt.Fprintf(w, "%s is up to date\n", target)
		if rec.Reason != "" {
			fmt.Fprintf(w, "last built %s because: %s\n", time.Unix(0, rec.Time).Format(time.DateTime), rec.Reason)
		}
		return nil
	}
	return nil
}

// Query writes what rmake knows about target (rmake -t query).
func (b *Builder) Query(w io.Writer, target string) error {
	if t, ok := b.G.Tasks[target]; ok {
		fmt.Fprintf(w, "task %s", t.Name)
		if len(t.Params) > 0 {
			fmt.Fprintf(w, "(%s)", strings.Join(t.Params, ", "))
		}
		fmt.Fprintf(w, "\n  defined at: %s\n", t.Pos)
		if t.Desc != "" {
			fmt.Fprintf(w, "  desc: %s\n", t.Desc)
		}
		list(w, "needs", t.Needs)
		return nil
	}
	r, ok := b.G.Producer[target]
	if !ok {
		return fmt.Errorf("%s is not produced by any rule", target)
	}
	fmt.Fprintf(w, "%s\n  defined at: %s\n", target, r.Pos)
	if r.Desc != "" {
		fmt.Fprintf(w, "  desc: %s\n", r.Desc)
	}
	list(w, "outputs", r.Outs)
	list(w, "inputs", r.Ins)
	list(w, "implicit inputs", r.Implicit)
	var cmds []string
	for _, c := range r.Cmds {
		cmds = append(cmds, c.String())
	}
	list(w, "commands", cmds)
	if r.Deps != "" {
		fmt.Fprintf(w, "  deps: %s (depfile %s)\n", r.Deps, r.Depfile)
	}
	if len(r.Env) > 0 {
		var env []string
		for _, k := range sortedKeys(r.Env) {
			env = append(env, k+"="+r.Env[k])
		}
		list(w, "pass", env)
	}
	if r.Always {
		fmt.Fprintln(w, "  always")
	}
	rec, ok := b.Store.Rule(RuleID(r))
	if !ok {
		fmt.Fprintln(w, "  no build record")
		return nil
	}
	list(w, "discovered inputs", sortedKeys(rec.Discovered))
	fmt.Fprintf(w, "  last built: %s (%d ms)\n", time.Unix(0, rec.Time).Format(time.DateTime), rec.Duration)
	fmt.Fprintf(w, "  key: %s\n", rec.Key)
	return nil
}

func list(w io.Writer, what string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(w, "  %s:\n", what)
	for _, it := range items {
		fmt.Fprintf(w, "    %s\n", it)
	}
}

// Clean deletes every output recorded in the state store, and nothing else
// (rmake -t clean). It returns the number of files removed.
func (b *Builder) Clean(w io.Writer) (int, error) {
	n := 0
	recs := b.Store.Rules()
	var outs []string
	for _, rec := range recs {
		outs = append(outs, rec.Outs...)
	}
	sort.Strings(outs)
	for _, o := range outs {
		err := os.Remove(b.abs(o))
		switch {
		case err == nil:
			n++
			fmt.Fprintf(w, "removed %s\n", o)
		case errors.Is(err, fs.ErrNotExist):
		default:
			return n, err
		}
		b.Hash.Forget(o)
	}
	for _, rec := range recs {
		b.Store.DeleteRule(rec.ID)
	}
	return n, nil
}
