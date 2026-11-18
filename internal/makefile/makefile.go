// Package makefile imports GNU Makefiles (README §10): it evaluates a
// Makefile as GNU make would (variables, conditionals, includes, functions,
// explicit, pattern, static pattern and suffix rules, GNU make's built-in
// rules and the implicit rule search) and lowers the result to rmake build
// language text, keeping global variables symbolic where that is exact.
package makefile

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rveen/rmake/internal/syntax"
)

// Options configure Import.
type Options struct {
	File       string            // the makefile, relative to Dir
	Dir        string            // the directory make would run in
	Environ    []string          // the environment, which make imports as variables
	Overrides  map[string]string // command-line variable assignments
	Goals      []string          // command-line goals ($(MAKECMDGOALS))
	AllowShell bool              // allow $(shell) and != at import time
	NoDefault  bool              // do not emit a default statement (included makefiles)
	// Fragment exports the makefile's pattern rules as rmake pattern rules
	// (included makefiles, whose targets the importer does not know).
	Fragment bool
	Stderr   io.Writer // $(info), $(warning)
}

// Note is a remark about the import. An approximation (Approx) means the
// rmake build may behave differently from make.
type Note struct {
	Pos    syntax.Pos
	Msg    string
	Approx bool
}

func (n Note) String() string { return n.Pos.String() + ": " + n.Msg }

// Result is an imported makefile.
type Result struct {
	Source []byte // rmake build file text
	Notes  []Note
	// Dropped lists the commands of directory targets that the import
	// dropped (rmake creates output directories itself); the differential
	// tests ignore them in make's output.
	Dropped []string
}

// Approx reports whether any note is an approximation.
func (r *Result) Approx() bool {
	for _, n := range r.Notes {
		if n.Approx {
			return true
		}
	}
	return false
}

// recipe is the recipe of a rule (shared by the targets of one rule line).
type recipe struct {
	lines []recipeLine
	pos   syntax.Pos
}

type recipeLine struct {
	text string
	pos  syntax.Pos
}

// target is an explicitly mentioned target.
type target struct {
	name      string
	pos       syntax.Pos
	prereqs   []string // with duplicates, in order
	orderOnly []string
	recipe    *recipe
	stem      string   // static pattern rules
	group     []string // grouped targets (&:), including this one
	vars      []tvar   // target-specific variables
	order     int      // definition order
}

// tvar is a target- or pattern-specific variable assignment.
type tvar struct {
	name, op, value string
	org             origin
	pos             syntax.Pos
}

// patternRule is a pattern rule (also built-in and suffix rules).
type patternRule struct {
	targets   []string
	prereqs   []string
	orderOnly []string
	recipe    *recipe
	builtin   bool
	suffixes  []string // built-in rules: the suffixes that enable them
	pos       syntax.Pos
}

type patternVar struct {
	pattern string
	tv      tvar
}

// cond is an open conditional.
type cond struct {
	active  bool // this branch is taken (and the parent is active)
	taken   bool // some branch was taken
	parent  bool // the parent was active
	sawElse bool
}

// Makefile is the state of an import.
type Makefile struct {
	opts    Options
	absRoot string
	pos     syntax.Pos

	vars            map[string]*variable
	undefinedRefs   map[string]bool
	cmdlineAssigned map[string]bool
	usedWildcard    bool

	targets     map[string]*target
	nextOrder   int
	patterns    []*patternRule
	suffixRules map[string]*recipe // ".c.o" or ".c" -> recipe
	suffixPos   map[string]syntax.Pos
	phony       map[string]bool
	suffixes    []string
	firstGoal   string
	mentioned   map[string]bool // explicit targets and prerequisites
	patternVars []patternVar
	exports     map[string]bool
	exportAll   bool
	depIncludes bool // .d files were included (and dropped)

	notes  []Note
	prefix byte
	conds  []cond
	cur    []*target    // targets of the current rule (recipe lines go there)
	curPat *patternRule // or the current pattern rule
	curRec *recipe
	inRule bool
	depth  int // include depth

	exists      map[string]bool
	ruleCache   []*patternRule
	searchCache map[string]*match
}

func (m *Makefile) note(approx bool, format string, args ...any) {
	m.notes = append(m.notes, Note{Pos: m.pos, Msg: sprintf(format, args...), Approx: approx})
}

// Import evaluates the makefile and lowers it to rmake text.
func Import(opts Options) (res *Result, err error) {
	if opts.Dir == "" {
		opts.Dir = "."
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	abs, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, err
	}
	m := &Makefile{
		opts: opts, absRoot: filepath.ToSlash(abs),
		vars: map[string]*variable{}, undefinedRefs: map[string]bool{}, cmdlineAssigned: map[string]bool{},
		targets: map[string]*target{}, suffixRules: map[string]*recipe{}, suffixPos: map[string]syntax.Pos{},
		phony: map[string]bool{}, mentioned: map[string]bool{}, exports: map[string]bool{},
		prefix: '\t', exists: map[string]bool{},
	}
	defer func() {
		if r := recover(); r != nil {
			f, ok := r.(failure)
			if !ok {
				panic(r)
			}
			res, err = nil, f.err
		}
	}()
	m.init()
	src, rerr := os.ReadFile(filepath.Join(opts.Dir, opts.File))
	if rerr != nil {
		return nil, rerr
	}
	m.read(newSource(opts.File, string(src)))
	return m.lower(), nil
}

// init defines the default, environment and command-line variables.
func (m *Makefile) init() {
	m.suffixes = append([]string(nil), defaultSuffixes...)
	for _, d := range builtinVars {
		m.vars[d[0]] = &variable{name: d[0], value: d[1], flavor: recursive, origin: oDefault,
			envable: true, base: piece{d[1], recursive}}
	}
	m.vars["CURDIR"] = &variable{name: "CURDIR", value: m.absRoot, flavor: simple, origin: oFile}
	m.vars["MAKECMDGOALS"] = &variable{name: "MAKECMDGOALS", value: strings.Join(m.opts.Goals, " "), flavor: simple, origin: oDefault}
	for _, kv := range m.opts.Environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" || skipEnv[k] {
			continue
		}
		if old, ok := m.vars[k]; ok && old.origin == oDefault {
			old.value, old.origin = v, oEnv
			continue
		}
		m.vars[k] = &variable{name: k, value: v, flavor: recursive, origin: oEnv, envable: true}
	}
	keys := make([]string, 0, len(m.opts.Overrides))
	for k := range m.opts.Overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		m.vars[k] = &variable{name: k, value: m.opts.Overrides[k], flavor: recursive, origin: oCmdline}
	}
}

// skipEnv lists environment variables make does not import (or that would
// change its behaviour).
var skipEnv = map[string]bool{"SHELL": true, "MAKEFLAGS": true, "MFLAGS": true, "MAKELEVEL": true,
	"MAKEFILES": true, "MAKEOVERRIDES": true, "GNUMAKEFLAGS": true, "MAKE_RESTARTS": true, "CURDIR": true}

// active reports whether lines are currently read (not in a false branch).
func (m *Makefile) active() bool {
	return len(m.conds) == 0 || m.conds[len(m.conds)-1].active
}

// read reads a makefile.
func (m *Makefile) read(s *source) {
	for !s.eof() {
		l := s.lines[s.next]
		if m.inRule && len(l) > 0 && l[0] == m.prefix {
			text, pos := s.recipe(m.prefix)
			if m.active() {
				m.pos = pos
				m.addRecipe(text, pos)
			}
			continue
		}
		text, pos := s.logical()
		m.pos = pos
		m.statement(s, text)
	}
	if m.depth == 0 && len(m.conds) > 0 {
		m.fail("missing 'endif'")
	}
}

// evalText implements $(eval text).
func (m *Makefile) evalText(text string) {
	if m.depth > 50 {
		m.fail("$(eval) nested too deeply")
	}
	m.depth++
	defer func() { m.depth-- }()
	saved := m.pos
	s := newSource(m.pos.File, text)
	s.next = 0
	// Positions inside eval'ed text refer to the eval call.
	for !s.eof() {
		l := s.lines[s.next]
		if m.inRule && len(l) > 0 && l[0] == m.prefix {
			text, _ := s.recipe(m.prefix)
			if m.active() {
				m.addRecipe(text, saved)
			}
			continue
		}
		text, _ := s.logical()
		m.pos = saved
		m.statement(s, text)
	}
	m.pos = saved
}

var condWords = map[string]bool{"ifeq": true, "ifneq": true, "ifdef": true, "ifndef": true, "else": true, "endif": true}

// statement handles one logical non-recipe line.
func (m *Makefile) statement(s *source, line string) {
	stripped := stripComment(line)
	trimmed := strings.TrimSpace(stripped)
	if trimmed == "" {
		return // blank and comment lines keep the rule context
	}
	word, rest := firstWord(trimmed)
	if condWords[word] {
		m.conditional(word, rest)
		return
	}
	if !m.active() {
		if word == "define" || strings.HasSuffix(word, "define") && isDefinePrefix(trimmed) {
			m.skipDefine(s)
		}
		return
	}
	// Prefixes of variable definitions.
	org := oFile
	exported := false
	for {
		switch word {
		case "override":
			org = oOverride
			trimmed = rest
			word, rest = firstWord(trimmed)
			continue
		case "export":
			if rest == "" {
				m.inRule = false
				m.exportAll = true
				m.note(false, "export of all variables: rmake passes only the variables listed with pass")
				return
			}
			exported = true
			trimmed = rest
			word, rest = firstWord(trimmed)
			continue
		case "private":
			trimmed = rest
			word, rest = firstWord(trimmed)
			continue
		}
		break
	}
	switch word {
	case "define":
		m.inRule = false
		m.define(s, rest, org, exported)
		return
	case "endef":
		m.fail("extraneous 'endef'")
	case "include", "-include", "sinclude":
		if !exported && org == oFile {
			m.inRule = false
			m.include(word != "include", rest)
			return
		}
	case "unexport":
		if !exported && org == oFile {
			m.inRule = false
			for _, n := range splitWords(m.expand(rest, m.global())) {
				delete(m.exports, n)
			}
			return
		}
	case "undefine":
		m.inRule = false
		for _, n := range splitWords(m.expand(rest, m.global())) {
			if v := m.vars[n]; v != nil && v.origin == oCmdline && org != oOverride {
				continue
			}
			delete(m.vars, n)
		}
		return
	case "vpath":
		m.inRule = false
		m.note(true, "vpath is not supported by the importer")
		return
	}
	if name, op, value, ok := splitAssign(trimmed); ok {
		m.inRule = false
		name = strings.TrimSpace(m.expand(name, m.global()))
		if op == "!=" {
			value = m.shell(m.expand(value, m.global()))
			op = "="
			value = strings.ReplaceAll(value, "$", "$$")
		}
		if name == "VPATH" {
			m.note(true, "VPATH is not supported by the importer")
		}
		if name == "SHELL" || name == ".SHELLFLAGS" {
			m.note(true, "%s is set: rmake runs recipes with /bin/sh -c", name)
		}
		if name == ".RECIPEPREFIX" {
			v := m.expand(value, m.global())
			if v == "" {
				m.prefix = '\t'
			} else {
				m.prefix = v[0]
			}
		}
		if org == oFile && m.vars[name] != nil && m.vars[name].origin == oCmdline {
			m.cmdlineAssigned[name] = true
		}
		m.set(name, op, value, org)
		if exported {
			m.exports[name] = true
		}
		return
	}
	if exported {
		// "export NAME ...": mark variables exported.
		for _, n := range splitWords(m.expand(trimmed, m.global())) {
			m.exports[n] = true
		}
		m.inRule = false
		return
	}
	if findTop(stripped, func(s string, i int) bool { return s[i] == ':' }) >= 0 {
		m.rule(stripped)
		return
	}
	// A line of function calls (typically $(eval ...)) that expands to
	// nothing is fine.
	if strings.TrimSpace(m.expand(trimmed, m.global())) == "" {
		return
	}
	m.fail("missing separator")
}

func isDefinePrefix(s string) bool {
	w, rest := firstWord(s)
	for w == "override" || w == "export" || w == "private" {
		w, rest = firstWord(rest)
	}
	return w == "define"
}

// splitAssign recognizes a variable assignment.
func splitAssign(s string) (name, op, value string, ok bool) {
	i := findTop(s, func(s string, i int) bool { return s[i] == '=' || s[i] == ':' })
	if i < 0 {
		return "", "", "", false
	}
	switch {
	case s[i] == ':':
		switch {
		case strings.HasPrefix(s[i:], ":::="):
			op = ":::="
		case strings.HasPrefix(s[i:], "::="):
			op = "::="
		case strings.HasPrefix(s[i:], ":="):
			op = ":="
		default:
			return "", "", "", false // a rule
		}
		name, value = s[:i], s[i+len(op):]
	default:
		op = "="
		j := i
		if i > 0 && strings.ContainsRune("?+!", rune(s[i-1])) {
			op = s[i-1:i] + "="
			j = i - 1
		}
		name, value = s[:j], s[i+1:]
	}
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, " \t") && !strings.Contains(name, "$") {
		return "", "", "", false
	}
	return name, op, strings.TrimLeft(value, " \t"), true
}

// conditional handles ifeq/ifneq/ifdef/ifndef/else/endif.
func (m *Makefile) conditional(word, rest string) {
	switch word {
	case "endif":
		if len(m.conds) == 0 {
			m.fail("extraneous 'endif'")
		}
		m.conds = m.conds[:len(m.conds)-1]
		return
	case "else":
		if len(m.conds) == 0 {
			m.fail("extraneous 'else'")
		}
		c := &m.conds[len(m.conds)-1]
		if c.sawElse {
			m.fail("only one 'else' per conditional")
		}
		if rest == "" {
			c.sawElse = true
			c.active = c.parent && !c.taken
			c.taken = c.taken || c.active
			return
		}
		// else ifeq ...
		w, r := firstWord(rest)
		if w != "ifeq" && w != "ifneq" && w != "ifdef" && w != "ifndef" {
			m.fail("extraneous text after 'else' directive")
		}
		if !c.parent || c.taken {
			c.active = false
			return
		}
		c.active = m.test(w, r)
		c.taken = c.active
		return
	}
	parent := m.active()
	c := cond{parent: parent}
	if parent {
		c.active = m.test(word, rest)
		c.taken = c.active
	}
	m.conds = append(m.conds, c)
}

// test evaluates a condition.
func (m *Makefile) test(word, rest string) bool {
	t := &taint{}
	c := &ctx{taint: t}
	var result bool
	switch word {
	case "ifdef", "ifndef":
		name := strings.TrimSpace(m.expand(rest, c))
		value, _, ok := m.lookup(name, c)
		result = ok && value != ""
		if word == "ifndef" {
			result = !result
		}
	default:
		a, b := m.condArgs(rest)
		result = m.expand(a, c) == m.expand(b, c)
		if word == "ifneq" {
			result = !result
		}
	}
	for name := range t.refs {
		v := m.vars[name]
		if v == nil || v.envable || v.origin == oEnv || v.origin == oCmdline {
			m.note(false, "condition on %s, which the environment or command line can set, was resolved at import time", name)
			break
		}
	}
	return result
}

// condArgs parses "(a,b)" or "'a' 'b'" / "\"a\" \"b\"".
func (m *Makefile) condArgs(s string) (string, string) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		inner := s[1 : len(s)-1]
		i := findTop(inner, func(s string, i int) bool { return s[i] == ',' })
		if i < 0 {
			m.fail("invalid syntax in conditional")
		}
		return strings.TrimSpace(inner[:i]), strings.TrimSpace(inner[i+1:])
	}
	var args []string
	for len(s) > 0 && len(args) < 2 {
		q := s[0]
		if q != '"' && q != '\'' {
			m.fail("invalid syntax in conditional")
		}
		j := strings.IndexByte(s[1:], q)
		if j < 0 {
			m.fail("invalid syntax in conditional")
		}
		args = append(args, s[1:j+1])
		s = strings.TrimSpace(s[j+2:])
	}
	if len(args) != 2 || s != "" {
		m.fail("invalid syntax in conditional")
	}
	return args[0], args[1]
}

// define reads a define ... endef block.
func (m *Makefile) define(s *source, rest string, org origin, exported bool) {
	rest = strings.TrimSpace(rest)
	op := "="
	for _, o := range []string{":::=", "::=", ":=", "+=", "?=", "!=", "="} {
		if strings.HasSuffix(rest, o) {
			op = o
			rest = strings.TrimSpace(strings.TrimSuffix(rest, o))
			break
		}
	}
	name := strings.TrimSpace(m.expand(rest, m.global()))
	body := m.defineBody(s)
	if op == "!=" {
		body = strings.ReplaceAll(m.shell(m.expand(body, m.global())), "$", "$$")
		op = "="
	}
	m.set(name, op, body, org)
	if exported {
		m.exports[name] = true
	}
}

func (m *Makefile) defineBody(s *source) string {
	var lines []string
	depth := 1
	for !s.eof() {
		l := s.lines[s.next]
		s.next++
		w, _ := firstWord(strings.TrimSpace(l))
		switch {
		case isDefinePrefix(strings.TrimSpace(l)):
			depth++
		case w == "endef":
			if depth--; depth == 0 {
				return strings.Join(lines, "\n")
			}
		}
		lines = append(lines, l)
	}
	m.fail("missing 'endef', unterminated 'define'")
	return ""
}

func (m *Makefile) skipDefine(s *source) { m.defineBody(s) }

// include reads included makefiles.
func (m *Makefile) include(optional bool, rest string) {
	files := splitWords(m.expand(rest, m.global()))
	if len(files) == 0 {
		return
	}
	allD := true
	for _, f := range files {
		if !strings.HasSuffix(f, ".d") {
			allD = false
		}
	}
	if allD {
		// Dependency files written by the compiler (-MD/-MMD): rmake
		// reads them itself (deps gcc).
		m.depIncludes = true
		return
	}
	for _, f := range files {
		full := f
		if !filepath.IsAbs(f) {
			full = filepath.Join(m.opts.Dir, f)
		}
		src, err := os.ReadFile(full)
		if errors.Is(err, os.ErrNotExist) {
			if optional {
				continue
			}
			m.fail("include %s: no such file (the importer does not build included makefiles)", f)
		}
		if err != nil {
			m.fail("include %s: %v", f, err)
		}
		if m.depth > 50 {
			m.fail("includes nested too deeply")
		}
		saved, savedRule := m.pos, m.inRule
		m.depth++
		m.inRule = false
		m.read(newSource(f, string(src)))
		m.depth--
		m.pos, m.inRule = saved, savedRule
		m.inRule = false
	}
}

// addRecipe adds a recipe line to the current rule.
func (m *Makefile) addRecipe(text string, pos syntax.Pos) {
	if m.curRec == nil {
		return // after a special target
	}
	m.curRec.lines = append(m.curRec.lines, recipeLine{text, pos})
	m.attachRecipe()
}

// attachRecipe gives the current recipe to the current rule's targets.
func (m *Makefile) attachRecipe() {
	if m.curPat != nil {
		m.curPat.recipe = m.curRec
		return
	}
	for _, t := range m.cur {
		if t.recipe != m.curRec {
			if t.recipe != nil && len(t.recipe.lines) > 0 {
				m.note(false, "recipe for %s overrides the one at %s", t.name, t.recipe.pos)
			}
			t.recipe = m.curRec
		}
	}
}

// getTarget returns the explicit target name, creating it.
func (m *Makefile) getTarget(name string) *target {
	t := m.targets[name]
	if t == nil {
		t = &target{name: name, pos: m.pos, order: m.nextOrder}
		m.nextOrder++
		m.targets[name] = t
	}
	return t
}

// specialTargets are handled (or ignored) by the importer.
var specialTargets = map[string]bool{
	".PHONY": true, ".SUFFIXES": true, ".DEFAULT": true, ".PRECIOUS": true, ".INTERMEDIATE": true,
	".SECONDARY": true, ".NOTINTERMEDIATE": true, ".DELETE_ON_ERROR": true, ".SILENT": true,
	".IGNORE": true, ".LOW_RESOLUTION_TIME": true, ".EXPORT_ALL_VARIABLES": true,
	".NOTPARALLEL": true, ".ONESHELL": true, ".POSIX": true, ".SECONDEXPANSION": true, ".WAIT": true,
}

// rule handles a rule line.
func (m *Makefile) rule(line string) {
	i := findTop(line, func(s string, i int) bool { return s[i] == ':' })
	if i < 0 {
		m.fail("missing separator")
	}
	targetsRaw, rest := line[:i], line[i+1:]
	grouped := false
	if strings.HasSuffix(strings.TrimRight(targetsRaw, " \t"), "&") {
		grouped = true
		targetsRaw = strings.TrimSuffix(strings.TrimRight(targetsRaw, " \t"), "&")
	}
	if strings.HasPrefix(rest, ":") {
		m.fail("double-colon rules are not supported by the importer")
	}
	var inline *string
	if j := findTop(rest, func(s string, i int) bool { return s[i] == ';' }); j >= 0 {
		r := rest[j+1:]
		inline = &r
		rest = rest[:j]
	}
	targets := splitWords(m.expand(targetsRaw, m.global()))
	m.inRule = true
	m.cur, m.curPat = nil, nil
	m.curRec = &recipe{pos: m.pos}
	if len(targets) == 0 {
		m.curRec = nil
		return
	}

	// Target-specific variable?
	if name, op, value, ok := splitAssign(rest); ok {
		m.curRec = nil
		org := oFile
		for {
			w, r := firstWord(name)
			if w == "override" {
				org, name = oOverride, r
			} else if w == "export" || w == "private" {
				name = r
				if w == "export" {
					m.exports[strings.TrimSpace(r)] = true
				}
			} else {
				break
			}
		}
		name = strings.TrimSpace(m.expand(name, m.global()))
		if op == ":=" || op == "::=" {
			value = m.expand(value, m.global())
		}
		tv := tvar{name: name, op: op, value: value, org: org, pos: m.pos}
		for _, t := range targets {
			if strings.Contains(t, "%") {
				m.patternVars = append(m.patternVars, patternVar{t, tv})
			} else {
				tg := m.getTarget(t)
				tg.vars = append(tg.vars, tv)
			}
		}
		return
	}

	// Static pattern rule?
	var targetPattern string
	static := false
	if j := findTop(rest, func(s string, i int) bool { return s[i] == ':' }); j >= 0 {
		static = true
		targetPattern = strings.TrimSpace(m.expand(rest[:j], m.global()))
		rest = rest[j+1:]
	}
	prereqText := m.expand(rest, m.global())
	normal, orderOnly := prereqText, ""
	if k := strings.IndexByte(prereqText, '|'); k >= 0 {
		normal, orderOnly = prereqText[:k], prereqText[k+1:]
	}
	prereqs := filterWait(splitWords(normal))
	oo := filterWait(splitWords(orderOnly))

	if specialTargets[targets[0]] {
		m.special(targets[0], prereqs)
		m.curRec = nil
		return
	}
	if !static && len(targets) == 1 && m.isSuffixRule(targets[0]) && len(prereqs) == 0 {
		m.suffixRules[targets[0]] = m.curRec
		m.suffixPos[targets[0]] = m.pos
		m.curRec = m.suffixRules[targets[0]]
		m.curPat = &patternRule{} // recipe lines go to curRec only
		if inline != nil {
			m.curRec.lines = append(m.curRec.lines, recipeLine{*inline, m.pos})
		}
		return
	}
	if !static && strings.Contains(targets[0], "%") {
		for _, t := range targets {
			if !strings.Contains(t, "%") {
				m.fail("mixed implicit and normal rules")
			}
		}
		pr := &patternRule{targets: targets, prereqs: prereqs, orderOnly: oo, pos: m.pos}
		if inline != nil {
			m.curRec.lines = append(m.curRec.lines, recipeLine{*inline, m.pos})
			pr.recipe = m.curRec
		}
		// Without a recipe it cancels matching rules (see allPatterns).
		m.patterns = append(m.patterns, pr)
		m.curPat = pr
		return
	}

	for _, p := range prereqs {
		m.mentioned[p] = true
	}
	for _, p := range oo {
		m.mentioned[p] = true
	}
	for _, name := range targets {
		if strings.Contains(name, "%") && !static {
			m.fail("mixed implicit and normal rules")
		}
		t := m.getTarget(name)
		m.mentioned[name] = true
		ps, os := prereqs, oo
		if static {
			stem, ok := matchPattern(targetPattern, name)
			if !ok {
				m.note(false, "target %s does not match the target pattern %s", name, targetPattern)
				continue
			}
			t.stem = stem
			ps, os = substAll(prereqs, stem), substAll(oo, stem)
			for _, p := range append(ps, os...) {
				m.mentioned[p] = true
			}
		}
		t.prereqs = append(t.prereqs, ps...)
		t.orderOnly = append(t.orderOnly, os...)
		if grouped {
			t.group = targets
		}
		m.cur = append(m.cur, t)
		if m.firstGoal == "" && !strings.HasPrefix(name, ".") || m.firstGoal == "" && strings.Contains(name, "/") {
			m.firstGoal = name
		}
	}
	if inline != nil {
		m.curRec.lines = append(m.curRec.lines, recipeLine{*inline, m.pos})
		m.attachRecipe()
		if strings.TrimSpace(*inline) == "" {
			m.curRec.lines = m.curRec.lines[:0]
		}
	}
}

func filterWait(ws []string) []string {
	out := ws[:0:0]
	for _, w := range ws {
		if w != ".WAIT" {
			out = append(out, w)
		}
	}
	return out
}

func substAll(pats []string, stem string) []string {
	out := make([]string, len(pats))
	for i, p := range pats {
		out[i] = substPattern(p, stem)
	}
	return out
}

// special handles special targets.
func (m *Makefile) special(name string, prereqs []string) {
	switch name {
	case ".PHONY":
		for _, p := range prereqs {
			m.phony[p] = true
		}
	case ".SUFFIXES":
		if len(prereqs) == 0 {
			m.suffixes = nil
		} else {
			m.suffixes = append(m.suffixes, prereqs...)
		}
	case ".EXPORT_ALL_VARIABLES":
		m.exportAll = true
		m.note(false, ".EXPORT_ALL_VARIABLES: rmake passes only the variables listed with pass")
	case ".DEFAULT":
		m.note(true, ".DEFAULT rules are not supported by the importer")
	case ".IGNORE":
		m.note(true, ".IGNORE is not supported by the importer")
	case ".ONESHELL":
		m.note(true, ".ONESHELL is not supported by the importer (rmake runs each recipe line in its own shell)")
	case ".SECONDEXPANSION":
		m.fail(".SECONDEXPANSION is not supported by the importer")
	case ".NOTPARALLEL":
		m.note(false, ".NOTPARALLEL ignored: rmake orders rules by their inputs")
	}
}

// isSuffixRule reports whether name is ".a.b" or ".a" with known suffixes.
func (m *Makefile) isSuffixRule(name string) bool {
	if !strings.HasPrefix(name, ".") || strings.Contains(name, "/") {
		return false
	}
	known := func(s string) bool {
		for _, x := range m.suffixes {
			if x == s {
				return true
			}
		}
		return false
	}
	if known(name) {
		return true
	}
	for _, s := range m.suffixes {
		if strings.HasPrefix(name, s) && known(name[len(s):]) {
			return true
		}
	}
	return false
}
