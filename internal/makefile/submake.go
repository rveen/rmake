package makefile

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rveen/rmake/internal/syntax"
)

// invocation is a recursive make: $(MAKE) [-C dir] [-f file] [VAR=value...]
// [targets...], possibly as "cd dir && $(MAKE) ...".
type invocation struct {
	dir     string // relative to the invoking makefile's directory
	file    string
	vars    map[string]string
	targets []string
}

// parseSubmake recognizes a recipe line that is exactly one recursive make.
// make is the value of $(MAKE).
func parseSubmake(line, make string) (inv invocation, ok bool) {
	line = strings.TrimSpace(strings.TrimLeft(line, "@-+ \t"))
	if rest, found := strings.CutPrefix(line, "cd "); found {
		dir, cmd, found := strings.Cut(rest, "&&")
		dir = strings.TrimSpace(dir)
		if !found || dir == "" || strings.ContainsAny(dir, " \t") {
			return inv, false
		}
		inv.dir, line = dir, strings.TrimSpace(cmd)
	}
	if strings.ContainsAny(line, ";|&<>`$'\"(){}*?[]\\\n") {
		return inv, false // more than one simple command
	}
	ws := strings.Fields(line)
	mk := strings.Fields(make)
	if len(mk) == 0 || len(ws) < len(mk) {
		return inv, false
	}
	for i := range mk {
		if ws[i] != mk[i] {
			return inv, false
		}
	}
	join := func(d string) {
		if inv.dir == "" || path.IsAbs(d) {
			inv.dir = d
		} else {
			inv.dir = path.Join(inv.dir, d)
		}
	}
	args := ws[len(mk):]
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() (string, bool) {
			if i+1 < len(args) {
				i++
				return args[i], true
			}
			return "", false
		}
		switch {
		case a == "-C" || a == "--directory":
			d, ok := next()
			if !ok {
				return inv, false
			}
			join(d)
		case strings.HasPrefix(a, "--directory="):
			join(strings.TrimPrefix(a, "--directory="))
		case strings.HasPrefix(a, "-C"):
			join(a[2:])
		case a == "-f" || a == "--file" || a == "--makefile":
			f, ok := next()
			if !ok {
				return inv, false
			}
			inv.file = f
		case strings.HasPrefix(a, "--file=") || strings.HasPrefix(a, "--makefile="):
			_, inv.file, _ = strings.Cut(a, "=")
		case a == "-s" || a == "--silent" || a == "--quiet" || a == "-w" || a == "--print-directory" ||
			a == "--no-print-directory" || a == "-k" || a == "--keep-going" || a == "-S":
			// No effect on what is built.
		case a == "-j" || a == "--jobs":
			if i+1 < len(args) && isNumber(args[i+1]) {
				i++
			}
		case strings.HasPrefix(a, "-j") && isNumber(a[2:]) || strings.HasPrefix(a, "--jobs="):
			// rmake schedules the jobs.
		case strings.HasPrefix(a, "-"):
			return inv, false // an option that changes what make does
		case strings.Contains(a, "="):
			k, v, _ := strings.Cut(a, "=")
			if inv.vars == nil {
				inv.vars = map[string]string{}
			}
			inv.vars[k] = v
		default:
			inv.targets = append(inv.targets, a)
		}
	}
	if inv.dir != "" {
		inv.dir = path.Clean(inv.dir)
	}
	return inv, true
}

func isNumber(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// taskPrefix is the prefix of task names of a sub-make in dir.
func taskPrefix(dir string) string {
	b := []byte(dir)
	for i, c := range b {
		switch {
		case c == '/':
			b[i] = '.'
		case !(isNameByte(c) || c == '-' || c == '.'):
			b[i] = '_'
		}
	}
	s := string(b)
	if s == "" || !(s[0] == '_' || 'a' <= s[0] && s[0] <= 'z' || 'A' <= s[0] && s[0] <= 'Z') {
		s = "_" + s
	}
	return s + "."
}

// recursive handles a recipe line that runs $(MAKE). In a task, a
// recognized recursive make becomes needs on the sub-makefile's targets;
// otherwise the line is delegated: kept as a command that runs make (rules
// then always run). It returns false for other lines.
func (l *lowering) recursive(n *node, rl recipeLine, c *ctx) bool {
	m := l.m
	t := &taint{}
	tc := *c
	tc.taint = t
	conc := m.expand(rl.text, &tc)
	if !t.refs["MAKE"] {
		return false
	}
	reason := "it is not a single recursive make"
	if inv, ok := parseSubmake(conc, m.valueOf("MAKE", m.global())); ok {
		if n.kind == kTask {
			keys, refs, err := l.root.invoke(l, inv)
			if err == nil {
				if len(n.cmds) > 0 {
					m.note(false, "in %s, commands before a recursive make run after it (rmake runs a task's needs first)", n.name)
				}
				if len(n.subKeys) > 0 {
					l.root.pairs = append(l.root.pairs, orderPair{a: n.subKeys[len(n.subKeys)-1], b: keys, pos: rl.pos,
						what: fmt.Sprintf("the recursive makes in the recipe of %s", n.name)})
				}
				n.subNeeds = append(n.subNeeds, refs...)
				n.subKeys = append(n.subKeys, keys)
				return true
			}
			reason = err.Error()
		} else {
			reason = "it is in the recipe of a file rule"
		}
	}
	l.addCmd(n, []seg{{lit: conc}})
	if n.kind == kRule {
		n.always = true
	}
	m.note(false, "recursive make in the recipe of %s delegated to make (%s)", n.name, reason)
	return true
}

// invoke imports the makefile a recursive make runs and returns the graph
// keys and the rmake names of the targets it builds.
func (root *lowering) invoke(from *lowering, inv invocation) (keys, refs []string, err error) {
	dir := from.prefix
	if inv.dir != "" {
		if path.IsAbs(inv.dir) {
			return nil, nil, fmt.Errorf("%s is outside the project", inv.dir)
		}
		dir = path.Clean(path.Join(from.prefix, inv.dir))
	}
	if dir == "." {
		dir = ""
	}
	if dir == ".." || strings.HasPrefix(dir, "../") {
		return nil, nil, fmt.Errorf("%s is outside the project", dir)
	}
	file := inv.file
	if file == "" {
		for _, f := range []string{"GNUmakefile", "makefile", "Makefile"} {
			if _, err := os.Stat(filepath.Join(root.m.opts.Dir, dir, f)); err == nil {
				file = f
				break
			}
		}
		if file == "" {
			return nil, nil, fmt.Errorf("no makefile in %s", dir)
		}
	}
	var sub *lowering
	if dir == from.prefix && file == from.m.opts.File && len(inv.vars) == 0 {
		sub = from // $(MAKE) target in the same makefile
	} else if sub, err = root.importSub(from, dir, file, inv.vars); err != nil {
		return nil, nil, err
	}
	targets := inv.targets
	if len(targets) == 0 {
		if g := sub.m.defaultGoal(); g != "" {
			targets = []string{g}
		}
	}
	for _, t := range targets {
		n := sub.ensure(t)
		key, ref, ok := sub.key(n)
		if !ok {
			return nil, nil, fmt.Errorf("%s: no rule to make target %s", path.Join(dir, file), t)
		}
		keys = append(keys, key)
		refs = append(refs, ref)
	}
	return keys, refs, nil
}

// importSub imports the makefile of a sub-make.
func (root *lowering) importSub(from *lowering, dir, file string, vars map[string]string) (*lowering, error) {
	var names []string
	for k := range vars {
		names = append(names, k)
	}
	sort.Strings(names)
	key := dir + "\x00" + file
	for _, k := range names {
		key += "\x00" + k + "=" + vars[k]
	}
	if s := root.subs[key]; s != nil {
		return s, nil
	}
	for _, s := range root.subList {
		if s.prefix == dir && s.m.opts.File == file {
			return nil, fmt.Errorf("%s is run with different variables", path.Join(dir, file))
		}
	}
	if root.busy[key] {
		return nil, fmt.Errorf("recursive make cycle through %s", path.Join(dir, file))
	}
	if len(root.busy) > 20 {
		return nil, fmt.Errorf("recursive makes nested too deeply")
	}
	// make passes command-line variables and exported variables down.
	overrides := map[string]string{}
	for k, v := range from.m.opts.Overrides {
		overrides[k] = v
	}
	for k, v := range vars {
		overrides[k] = v
	}
	environ := append([]string(nil), from.m.opts.Environ...)
	var exported []string
	for e := range from.m.exports {
		exported = append(exported, e)
	}
	sort.Strings(exported)
	for _, e := range exported {
		environ = append(environ, e+"="+from.m.valueOf(e, from.m.global()))
	}
	opts := Options{
		File: file, Name: path.Join(dir, file), Dir: filepath.Join(root.m.opts.Dir, dir),
		Environ: environ, Overrides: overrides, AllowShell: root.m.opts.AllowShell, Stderr: root.m.opts.Stderr,
	}
	n := len(root.subList)
	var sub *lowering
	err := func() (err error) {
		defer catch(&err)
		m2, err := load(opts)
		if err != nil {
			return err
		}
		root.busy[key] = true
		sub = newLowering(m2, dir, root)
		sub.analyze()
		return nil
	}()
	delete(root.busy, key)
	if err == nil {
		// A sub-make that cannot be imported exactly is delegated instead.
		for _, s := range append([]*lowering{sub}, root.subList[n:]...) {
			for _, note := range s.m.notes {
				if note.Approx && err == nil {
					err = fmt.Errorf("importing it needs an approximation: %s", note)
				}
			}
		}
	}
	if err != nil {
		for _, s := range root.subList[n:] {
			for k, v := range root.subs {
				if v == s {
					delete(root.subs, k)
				}
			}
		}
		root.subList = root.subList[:n]
		return nil, err
	}
	root.subs[key] = sub
	root.subList = append(root.subList, sub)
	return sub, nil
}

// key returns the graph key and the rmake name of a node.
func (l *lowering) key(n *node) (key, ref string, ok bool) {
	ref, ok = l.ref(n)
	if !ok || l.kindOf(n) == kMissing && !l.m.exist(n.name) {
		return "", "", false
	}
	switch l.kindOf(n) {
	case kTask, kPseudo:
		return "t:" + l.tprefix + n.name, ref, true
	}
	return "f:" + l.path(n.name), ref, true
}

// orderPair says that make runs the targets b after the targets a.
type orderPair struct {
	a, b []string // graph keys
	pos  syntax.Pos
	what string
}

// checkOrder notes orders between sub-makes that rmake would not keep:
// make runs b after a, but nothing b builds depends on a file a builds, so
// rmake may build them in parallel.
func (root *lowering) checkOrder() {
	pairs := root.pairs
	all := append([]*lowering{root}, root.subList...)
	// Prerequisites between tasks that run recursive makes (app: lib).
	for _, l := range all {
		for _, n := range l.list {
			if n.alias != nil || len(n.subKeys) == 0 {
				continue
			}
			for _, p := range n.prereqs {
				pn := l.nodes[p]
				if pn == nil || len(pn.subKeys) == 0 {
					continue
				}
				for _, a := range pn.subKeys {
					for _, b := range n.subKeys {
						pairs = append(pairs, orderPair{a: a, b: b, pos: n.pos,
							what: fmt.Sprintf("the recursive makes of %s and %s", p, n.name)})
					}
				}
			}
		}
	}
	if len(pairs) == 0 {
		return
	}
	deps := map[string][]string{}
	isRule := map[string]bool{}
	for _, l := range all {
		for _, n := range l.list {
			if n.alias != nil {
				continue
			}
			switch l.kindOf(n) {
			case kRule:
				kept, extra, _ := l.inputs(n)
				var ds []string
				for _, x := range append(kept, extra...) {
					ds = append(ds, "f:"+x)
				}
				for _, o := range n.outs {
					k := "f:" + l.path(o)
					isRule[k] = true
					deps[k] = ds
				}
			case kTask, kPseudo:
				var ds []string
				for _, p := range append(append([]string(nil), n.prereqs...), n.orderOnly...) {
					if pn := l.nodes[p]; pn != nil {
						if k, _, ok := l.key(pn); ok {
							ds = append(ds, k)
						}
					}
				}
				for _, ks := range n.subKeys {
					ds = append(ds, ks...)
				}
				deps["t:"+l.tprefix+n.name] = ds
			}
		}
	}
	closure := func(keys []string) map[string]bool {
		seen := map[string]bool{}
		var walk func(k string)
		walk = func(k string) {
			if seen[k] {
				return
			}
			seen[k] = true
			for _, d := range deps[k] {
				walk(d)
			}
		}
		for _, k := range keys {
			walk(k)
		}
		return seen
	}
	for _, p := range pairs {
		ca, cb := closure(p.a), closure(p.b)
		built, connected := false, false
		for k := range ca {
			if isRule[k] {
				built = true
				if cb[k] {
					connected = true
				}
			}
		}
		if built && !connected {
			root.m.pos = p.pos
			root.m.note(true, "make runs %s one after the other, but nothing the second builds depends on a file the first builds; rmake would run them in parallel", p.what)
		}
	}
}
