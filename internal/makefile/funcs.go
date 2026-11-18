package makefile

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// funcArgs is the maximum number of arguments of each function (0: any);
// the last argument takes the remaining commas.
var funcArgs = map[string]int{
	"subst": 3, "patsubst": 3, "strip": 1, "findstring": 2, "filter": 2, "filter-out": 2,
	"sort": 1, "word": 2, "wordlist": 3, "words": 1, "firstword": 1, "lastword": 1,
	"dir": 1, "notdir": 1, "suffix": 1, "basename": 1, "addsuffix": 2, "addprefix": 2,
	"join": 2, "wildcard": 1, "realpath": 1, "abspath": 1, "error": 1, "warning": 1,
	"info": 1, "shell": 1, "origin": 1, "flavor": 1, "foreach": 3, "if": 3, "or": 0,
	"and": 0, "call": 0, "eval": 1, "value": 1, "file": 2, "let": 3, "intcmp": 5,
	"guile": 1,
}

// splitFunc recognizes a function call "name args".
func (m *Makefile) splitFunc(content string) (name string, args []string, ok bool) {
	i := strings.IndexAny(content, " \t")
	if i < 0 {
		return "", nil, false
	}
	name = content[:i]
	max, known := funcArgs[name]
	if !known {
		return "", nil, false
	}
	rest := strings.TrimLeft(content[i:], " \t")
	return name, splitArgs(rest, max), true
}

// splitArgs splits function arguments at top-level commas.
func splitArgs(s string, max int) []string {
	var args []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(', '{':
			depth++
		case ')', '}':
			depth--
		case ',':
			if depth == 0 && (max == 0 || len(args) < max-1) {
				args = append(args, s[start:i])
				start = i + 1
			}
		}
	}
	return append(args, s[start:])
}

// call evaluates a function. Arguments are raw; most are expanded first.
func (m *Makefile) call(name string, raw []string, c *ctx) string {
	arg := func(i int) string {
		if i >= len(raw) {
			return ""
		}
		return m.expand(raw[i], c)
	}
	need := func(n int) {
		if len(raw) < n {
			m.fail("insufficient number of arguments (%d) to function '%s'", len(raw), name)
		}
	}
	switch name {
	case "subst":
		need(3)
		from, to, text := arg(0), arg(1), arg(2)
		if from == "" {
			return text + to
		}
		return strings.ReplaceAll(text, from, to)
	case "patsubst":
		need(3)
		return patsubst(arg(0), arg(1), arg(2))
	case "strip":
		return strings.Join(splitWords(arg(0)), " ")
	case "findstring":
		need(2)
		if strings.Contains(arg(1), arg(0)) {
			return arg(0)
		}
		return ""
	case "filter", "filter-out":
		need(2)
		pats := splitWords(arg(0))
		var out []string
		for _, w := range splitWords(arg(1)) {
			match := false
			for _, p := range pats {
				if _, ok := matchPattern(p, w); ok {
					match = true
					break
				}
			}
			if match == (name == "filter") {
				out = append(out, w)
			}
		}
		return strings.Join(out, " ")
	case "sort":
		ws := splitWords(arg(0))
		sort.Strings(ws)
		var out []string
		for i, w := range ws {
			if i == 0 || w != ws[i-1] {
				out = append(out, w)
			}
		}
		return strings.Join(out, " ")
	case "word":
		need(2)
		n := m.number(name, arg(0))
		ws := splitWords(arg(1))
		if n < 1 {
			m.fail("first argument to 'word' function must be greater than 0")
		}
		if n > len(ws) {
			return ""
		}
		return ws[n-1]
	case "wordlist":
		need(3)
		s, e := m.number(name, arg(0)), m.number(name, arg(1))
		ws := splitWords(arg(2))
		if s < 1 {
			m.fail("invalid first argument to 'wordlist' function")
		}
		if e > len(ws) {
			e = len(ws)
		}
		if s > e {
			return ""
		}
		return strings.Join(ws[s-1:e], " ")
	case "words":
		return strconv.Itoa(len(splitWords(arg(0))))
	case "firstword":
		if ws := splitWords(arg(0)); len(ws) > 0 {
			return ws[0]
		}
		return ""
	case "lastword":
		if ws := splitWords(arg(0)); len(ws) > 0 {
			return ws[len(ws)-1]
		}
		return ""
	case "dir", "notdir", "suffix", "basename", "realpath", "abspath":
		var out []string
		for _, w := range splitWords(arg(0)) {
			if r, ok := m.pathFunc(name, w); ok {
				out = append(out, r)
			}
		}
		return strings.Join(out, " ")
	case "addsuffix", "addprefix":
		need(2)
		a := arg(0)
		ws := splitWords(arg(1))
		for i, w := range ws {
			if name == "addsuffix" {
				ws[i] = w + a
			} else {
				ws[i] = a + w
			}
		}
		return strings.Join(ws, " ")
	case "join":
		need(2)
		a, b := splitWords(arg(0)), splitWords(arg(1))
		var out []string
		for i := 0; i < len(a) || i < len(b); i++ {
			s := ""
			if i < len(a) {
				s += a[i]
			}
			if i < len(b) {
				s += b[i]
			}
			out = append(out, s)
		}
		return strings.Join(out, " ")
	case "wildcard":
		m.usedWildcard = true
		var out []string
		for _, p := range splitWords(arg(0)) {
			out = append(out, m.wildcard(p)...)
		}
		return strings.Join(out, " ")
	case "error":
		m.fail("%s", arg(0))
	case "warning":
		fmt.Fprintf(m.opts.Stderr, "%s: %s\n", m.pos, arg(0))
		m.side(c)
		return ""
	case "info":
		fmt.Fprintln(m.opts.Stderr, arg(0))
		m.side(c)
		return ""
	case "shell":
		m.side(c)
		return m.shell(arg(0))
	case "origin":
		n := arg(0)
		if _, ok := m.autoValue(n, &ctx{}); ok {
			return "automatic"
		}
		for i := len(c.locals) - 1; i >= 0; i-- {
			if _, ok := c.locals[i][n]; ok {
				return "automatic"
			}
		}
		if v, ok := c.target[n]; ok && v.origin != oUndefined {
			return v.origin.String()
		}
		if v, ok := m.vars[n]; ok {
			return v.origin.String()
		}
		return "undefined"
	case "flavor":
		n := arg(0)
		if _, fl, ok := m.lookup(n, &ctx{target: c.target, locals: c.locals}); ok {
			if fl == recursive {
				return "recursive"
			}
			return "simple"
		}
		return "undefined"
	case "foreach":
		need(3)
		v := strings.TrimSpace(arg(0))
		var out []string
		for _, w := range splitWords(arg(1)) {
			out = append(out, m.expand(raw[2], c.withLocals(map[string]string{v: w})))
		}
		return strings.Join(out, " ")
	case "let":
		need(3)
		names := splitWords(arg(0))
		words := splitWords(arg(1))
		vars := map[string]string{}
		for i, n := range names {
			switch {
			case i == len(names)-1 && i < len(words):
				vars[n] = strings.Join(words[i:], " ")
			case i < len(words):
				vars[n] = words[i]
			default:
				vars[n] = ""
			}
		}
		return m.expand(raw[2], c.withLocals(vars))
	case "if":
		need(2)
		if strings.TrimSpace(arg(0)) != "" {
			return arg(1)
		}
		return arg(2)
	case "or":
		for i := range raw {
			if v := arg(i); strings.TrimSpace(v) != "" {
				return v
			}
		}
		return ""
	case "and":
		v := ""
		for i := range raw {
			if v = arg(i); strings.TrimSpace(v) == "" {
				return ""
			}
		}
		return v
	case "intcmp":
		need(2)
		a, b := m.number(name, arg(0)), m.number(name, arg(1))
		if len(raw) == 2 {
			if a == b {
				return strconv.Itoa(a)
			}
			return ""
		}
		// gt defaults to eq, eq to empty.
		switch {
		case a < b:
			return arg(2)
		case a == b:
			return arg(3)
		case len(raw) > 4:
			return arg(4)
		}
		return arg(3)
	case "call":
		fn := strings.TrimSpace(arg(0))
		vars := map[string]string{"0": fn}
		for i := 1; i < len(raw); i++ {
			vars[strconv.Itoa(i)] = arg(i)
		}
		// Unset numbered parameters beyond the arguments (nested calls).
		for i := len(raw); i < 10; i++ {
			vars[strconv.Itoa(i)] = ""
		}
		value, fl, ok := m.lookup(fn, c)
		if !ok {
			return ""
		}
		if fl == simple {
			return value
		}
		n := c.withLocals(vars)
		n.depth++
		if n.depth > maxDepth {
			m.fail("recursive call of %s", fn)
		}
		return m.expand(value, n)
	case "value":
		value, _, _ := m.lookup(strings.TrimSpace(arg(0)), c)
		return value
	case "eval":
		m.side(c)
		m.evalText(arg(0))
		return ""
	case "file":
		m.fail("the file function is not supported by the importer")
	case "guile":
		m.fail("the guile function is not supported")
	}
	m.fail("unknown function %s", name)
	return ""
}

func (m *Makefile) side(c *ctx) {
	if c.taint != nil {
		c.taint.side = true
	}
}

func (m *Makefile) number(fn, s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		m.fail("non-numeric argument to '%s' function: '%s'", fn, strings.TrimSpace(s))
	}
	return n
}

// pathFunc implements dir, notdir, suffix, basename, realpath, abspath for
// one word.
func (m *Makefile) pathFunc(fn, w string) (string, bool) {
	slash := strings.LastIndexByte(w, '/')
	switch fn {
	case "dir":
		if slash < 0 {
			return "./", true
		}
		return w[:slash+1], true
	case "notdir":
		return w[slash+1:], true
	case "suffix":
		if dot := strings.LastIndexByte(w, '.'); dot > slash {
			return w[dot:], true
		}
		return "", false
	case "basename":
		if dot := strings.LastIndexByte(w, '.'); dot > slash {
			return w[:dot], true
		}
		return w, true
	case "abspath", "realpath":
		p := w
		if !path.IsAbs(p) {
			p = path.Join(m.absRoot, p)
		}
		p = path.Clean(p)
		if fn == "realpath" {
			r, err := filepath.EvalSymlinks(p)
			if err != nil {
				return "", false
			}
			p = filepath.ToSlash(r)
		}
		return p, true
	}
	return "", false
}

// wildcard expands one glob pattern relative to the root, as make does
// (sorted; a pattern without metacharacters returns itself if it exists).
func (m *Makefile) wildcard(p string) []string {
	full := p
	if !path.IsAbs(p) {
		full = filepath.Join(m.opts.Dir, p)
	}
	if !strings.ContainsAny(p, "*?[") {
		if _, err := os.Lstat(full); err == nil {
			return []string{p}
		}
		return nil
	}
	matches, _ := filepath.Glob(full)
	var out []string
	for _, f := range matches {
		if !path.IsAbs(p) {
			rel, err := filepath.Rel(m.opts.Dir, f)
			if err != nil {
				continue
			}
			f = filepath.ToSlash(rel)
			if strings.HasPrefix(p, "./") {
				f = "./" + f
			}
		}
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// shell runs a command at import time, if allowed.
func (m *Makefile) shell(cmd string) string {
	if !m.opts.AllowShell {
		m.fail("$(shell %s) needs -allow-shell (it runs at import time)", strings.TrimSpace(cmd))
	}
	c := exec.Command("/bin/sh", "-c", cmd)
	c.Dir = m.opts.Dir
	c.Env = m.opts.Environ
	c.Stderr = m.opts.Stderr
	out, _ := c.Output()
	s := strings.TrimRight(string(out), "\n")
	return strings.ReplaceAll(s, "\n", " ")
}
