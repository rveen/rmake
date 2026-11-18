package makefile

import (
	"fmt"
	"path"
	"strings"

	"github.com/rveen/rmake/internal/syntax"
)

type flavor int

const (
	recursive flavor = iota
	simple
)

type origin int

const (
	oUndefined origin = iota
	oDefault
	oEnv
	oFile
	oCmdline
	oOverride
	oAutomatic
)

func (o origin) String() string {
	return [...]string{"undefined", "default", "environment", "file", "command line", "override", "automatic"}[o]
}

// piece is an assigned value with its flavor (raw text for recursive, the
// expanded text for simple).
type piece struct {
	value  string
	flavor flavor
}

// variable is a make variable.
type variable struct {
	name   string
	value  string // raw (recursive) or expanded (simple)
	flavor flavor
	origin origin
	pos    syntax.Pos

	// For emission: a variable whose value the environment could set at
	// rmake run time (make default, environment, ?=, or only appended to)
	// is emitted as "?= base" plus "+= appends".
	envable bool
	base    piece
	appends []piece
}

// failure is a fatal import error; it is raised with panic and recovered by
// Import.
type failure struct{ err error }

func (m *Makefile) fail(format string, args ...any) {
	panic(failure{syntax.Errorf(m.pos, format, args...)})
}

// taint records what an expansion depended on.
type taint struct {
	auto bool            // automatic variables
	side bool            // functions with side effects or import-time inputs ($(shell), $(eval), ...)
	refs map[string]bool // variables looked up
	env  map[string]bool // variables whose value came from the environment or command line
}

func (t *taint) ref(name string) {
	if t.refs == nil {
		t.refs = map[string]bool{}
	}
	t.refs[name] = true
}

// ctx is the context of an expansion.
type ctx struct {
	auto   map[string]string    // automatic variables (recipes)
	locals []map[string]string  // foreach/call/let variables, innermost last
	target map[string]*variable // target- and pattern-specific variables
	taint  *taint
	depth  int
}

func (c *ctx) withLocals(vars map[string]string) *ctx {
	n := *c
	n.locals = append(append([]map[string]string(nil), c.locals...), vars)
	return &n
}

// lookup finds a variable: locals, then target-specific, then global.
func (m *Makefile) lookup(name string, c *ctx) (value string, fl flavor, ok bool) {
	for i := len(c.locals) - 1; i >= 0; i-- {
		if v, ok := c.locals[i][name]; ok {
			return v, simple, true
		}
	}
	if c.taint != nil {
		c.taint.ref(name)
	}
	if v, ok := c.target[name]; ok {
		return v.value, v.flavor, true
	}
	if v, ok := m.vars[name]; ok {
		if c.taint != nil && (v.origin == oEnv || v.origin == oCmdline) {
			if c.taint.env == nil {
				c.taint.env = map[string]bool{}
			}
			c.taint.env[name] = true
		}
		return v.value, v.flavor, true
	}
	return "", simple, false
}

const maxDepth = 200

// valueOf expands the value of variable name.
func (m *Makefile) valueOf(name string, c *ctx) string {
	if v, ok := m.autoValue(name, c); ok {
		return v
	}
	value, fl, ok := m.lookup(name, c)
	if !ok {
		m.undefinedRefs[name] = true
		return ""
	}
	if fl == simple {
		return value
	}
	if c.depth > maxDepth {
		m.fail("recursive variable %s references itself (eventually)", name)
	}
	n := *c
	n.depth++
	return m.expand(value, &n)
}

// autoValue returns automatic variables ($@, $(@D), ...).
func (m *Makefile) autoValue(name string, c *ctx) (string, bool) {
	if len(name) == 0 || len(name) > 2 || !strings.ContainsRune("@<^+*?|%", rune(name[0])) {
		return "", false
	}
	if len(name) == 2 && name[1] != 'D' && name[1] != 'F' {
		return "", false
	}
	if c.taint != nil {
		c.taint.auto = true
	}
	if c.auto == nil {
		return "", true
	}
	v := c.auto[name[:1]]
	if len(name) == 1 {
		return v, true
	}
	var out []string
	for _, w := range splitWords(v) {
		if name[1] == 'D' {
			d := path.Dir(w)
			out = append(out, d)
		} else {
			out = append(out, path.Base(w))
		}
	}
	return strings.Join(out, " "), true
}

// expand expands all references in s.
func (m *Makefile) expand(s string, c *ctx) string {
	if !strings.Contains(s, "$") {
		return s
	}
	var b strings.Builder
	m.scan(s, func(lit string) { b.WriteString(lit) }, func(content string) {
		b.WriteString(m.resolve(content, c))
	})
	return b.String()
}

// scan splits s into literal text and references. For $(...) and ${...}
// content is the text between the delimiters; for $x it is "x". "$$" is a
// literal "$".
func (m *Makefile) scan(s string, lit func(string), ref func(content string)) {
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '$' {
			continue
		}
		if i > start {
			lit(s[start:i])
		}
		if i+1 >= len(s) {
			start = i + 1 // a lone '$' at the end expands to nothing
			break
		}
		c := s[i+1]
		switch c {
		case '$':
			lit("$")
			i++
		case '(', '{':
			cl := closer(c)
			depth := 1
			j := i + 2
			for ; j < len(s); j++ {
				if s[j] == c {
					depth++
				} else if s[j] == cl {
					if depth--; depth == 0 {
						break
					}
				}
			}
			if j >= len(s) {
				m.fail("unterminated variable reference")
			}
			ref(s[i+2 : j])
			i = j
		default:
			ref(s[i+1 : i+2])
			i++
		}
		start = i + 1
	}
	if start < len(s) {
		lit(s[start:])
	}
}

// resolve expands the content of one reference.
func (m *Makefile) resolve(content string, c *ctx) string {
	if name, args, ok := m.splitFunc(content); ok {
		return m.call(name, args, c)
	}
	if i := findTop(content, func(s string, i int) bool { return s[i] == ':' }); i >= 0 {
		rest := content[i+1:]
		if j := findTop(rest, func(s string, i int) bool { return s[i] == '=' }); j >= 0 {
			name := m.expand(content[:i], c)
			pat := m.expand(rest[:j], c)
			repl := m.expand(rest[j+1:], c)
			return substRef(m.valueOf(name, c), pat, repl)
		}
	}
	return m.valueOf(m.expand(content, c), c)
}

// substRef implements $(var:pat=repl).
func substRef(value, pat, repl string) string {
	if !strings.Contains(pat, "%") {
		pat, repl = "%"+pat, "%"+repl
	}
	return patsubst(pat, repl, value)
}

// patsubst implements $(patsubst pat,repl,text).
func patsubst(pat, repl, text string) string {
	words := splitWords(text)
	out := make([]string, len(words))
	for i, w := range words {
		if stem, ok := matchPattern(pat, w); ok {
			out[i] = substPattern(repl, stem)
		} else {
			out[i] = w
		}
	}
	return strings.Join(out, " ")
}

// matchPattern matches w against a pattern with at most one '%' (without
// '%', w must equal the pattern).
func matchPattern(pat, w string) (stem string, ok bool) {
	i := strings.IndexByte(pat, '%')
	if i < 0 {
		return "", pat == w
	}
	pre, suf := pat[:i], pat[i+1:]
	if len(w) < len(pre)+len(suf) || !strings.HasPrefix(w, pre) || !strings.HasSuffix(w, suf) {
		return "", false
	}
	return w[len(pre) : len(w)-len(suf)], true
}

// substPattern replaces the first '%' of repl with stem.
func substPattern(repl, stem string) string {
	if i := strings.IndexByte(repl, '%'); i >= 0 {
		return repl[:i] + stem + repl[i+1:]
	}
	return repl
}

// seg is a piece of a template: literal text, or a reference kept symbolic
// in the rmake output (with its concrete value).
type seg struct {
	lit string
	ref string // rmake variable name, or "" for literal
	val string // concrete value of ref
}

// symbolicer decides, during template expansion, which references stay
// symbolic.
type symbolicer interface {
	// variable returns the rmake name of make variable name if a reference
	// to it may stay symbolic in context c.
	variable(name string, c *ctx) (string, bool)
	// automatic returns the rmake name ("in", "out") for an automatic
	// variable if it may stay symbolic.
	automatic(name string) (string, bool)
}

// template expands s like expand, but keeps references that sym allows as
// symbolic segments. Non-symbolic recursive variables are expanded inline as
// templates, so that references inside them can stay symbolic too.
func (m *Makefile) template(s string, c *ctx, sym symbolicer) []seg {
	var out []seg
	addLit := func(t string) {
		if t == "" {
			return
		}
		if k := len(out) - 1; k >= 0 && out[k].ref == "" {
			out[k].lit += t
			return
		}
		out = append(out, seg{lit: t})
	}
	m.scan(s, addLit, func(content string) {
		concrete := func() { addLit(m.resolve(content, c)) }
		if _, _, ok := m.splitFunc(content); ok || strings.ContainsAny(content, "$:") {
			concrete() // function, computed name or substitution reference
			return
		}
		name := content
		if _, ok := m.autoValue(name, &ctx{}); ok {
			if r, ok := sym.automatic(name); ok {
				out = append(out, seg{ref: r, val: m.valueOf(name, c)})
				return
			}
			concrete()
			return
		}
		if r, ok := sym.variable(name, c); ok {
			out = append(out, seg{ref: r, val: m.valueOf(name, c)})
			return
		}
		value, fl, ok := m.lookup(name, c)
		if !ok || fl == simple || c.depth > maxDepth {
			concrete()
			return
		}
		n := *c
		n.depth++
		for _, sg := range m.template(value, &n, sym) {
			if sg.ref == "" {
				addLit(sg.lit)
			} else {
				out = append(out, sg)
			}
		}
	})
	return out
}

// concrete joins a template's concrete text.
func concrete(t []seg) string {
	var b strings.Builder
	for _, s := range t {
		if s.ref == "" {
			b.WriteString(s.lit)
		} else {
			b.WriteString(s.val)
		}
	}
	return b.String()
}

// set assigns a global variable following make's precedence rules.
func (m *Makefile) set(name, op, value string, org origin) {
	if name == "" {
		m.fail("empty variable name")
	}
	v := m.vars[name]
	if v != nil && (v.origin == oCmdline || v.origin == oOverride) && org < v.origin && org != oAutomatic {
		if v.origin == oCmdline {
			m.cmdlineAssigned[name] = true
		}
		return // command-line and override values win
	}
	switch op {
	case "?=":
		if v != nil {
			if v.origin == oEnv && v.envable && v.base.value == "" && len(v.appends) == 0 {
				v.base = piece{value, recursive}
			}
			return
		}
		m.vars[name] = &variable{name: name, value: value, flavor: recursive, origin: org, pos: m.pos,
			envable: org == oFile, base: piece{value, recursive}}
	case "+=":
		if v == nil {
			m.vars[name] = &variable{name: name, value: value, flavor: recursive, origin: org, pos: m.pos,
				envable: org == oFile, appends: []piece{{value, recursive}}}
			return
		}
		p := piece{value, v.flavor}
		if v.flavor == simple {
			p.value = m.expand(value, m.global())
		}
		if v.value == "" {
			v.value = p.value
		} else if p.value != "" {
			v.value += " " + p.value
		}
		if org == oOverride {
			v.origin = oOverride
		}
		if v.envable {
			v.appends = append(v.appends, p)
		}
	case "=", ":=", "::=", ":::=":
		fl := recursive
		if op != "=" {
			fl = simple
			value = m.expand(value, m.global())
			if op == ":::=" {
				value = strings.ReplaceAll(value, "$", "$$")
				fl = recursive
			}
		}
		m.vars[name] = &variable{name: name, value: value, flavor: fl, origin: org, pos: m.pos}
	default:
		panic(fmt.Sprintf("bad op %q", op))
	}
}

func (m *Makefile) global() *ctx { return &ctx{} }
