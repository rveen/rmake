package makefile

import (
	"os"
	"path"
	"sort"
	"strings"

	"github.com/rveen/rmake/internal/syntax"
)

type kind int

const (
	kMissing kind = iota // not a target, no rule, does not exist: make fails
	kSource              // exists, no recipe
	kRule                // a file made by a recipe
	kTask                // .PHONY (or looks like it)
	kPseudo              // a target without recipe that does not exist
	kDir                 // a directory target (rmake creates directories itself)
)

// node is a target as the lowering sees it.
type node struct {
	name      string
	kind      kind
	outs      []string // make names of all outputs (grouped rules)
	alias     *node    // another output of a grouped rule
	prereqs   []string
	orderOnly []string
	recipe    *recipe
	stem      string
	order     int
	pos       syntax.Pos
	explicit  bool // an explicit target

	// Filled while lowering.
	vars        map[string]*variable // target- and pattern-specific variables
	cmds        []string             // lowered sh strings
	refs        map[string]bool      // variables the recipe referenced
	empty       bool                 // the recipe expanded to nothing
	explicitIns []string             // rules: explicit inputs ($in)
	depfile     string               // rules: depfile written with -MD/-MMD
	usesTarget  bool                 // the recipe references $@
	fromRule    *patternRule         // the implicit rule that makes it
}

// knownTasks are target names that are tasks even when not marked .PHONY.
var knownTasks = map[string]bool{
	"all": true, "clean": true, "distclean": true, "mostlyclean": true, "maintainer-clean": true,
	"realclean": true, "clobber": true, "install": true, "uninstall": true, "check": true,
	"test": true, "tests": true, "dist": true, "distcheck": true, "help": true, "run": true,
	"format": true, "fmt": true, "lint": true, "tags": true, "TAGS": true, "docs": true, "doc": true,
	"depend": true, "dep": true, "deps": true,
}

type lowering struct {
	m        *Makefile
	nodes    map[string]*node
	list     []*node
	seq      int
	symUsed  map[string]bool
	symInfo  map[string]*symInfo
	owner    map[string]string // rmake name -> make name
	dropped  []string
	notes    []Note
	usesMD   bool
	exported map[*patternRule]bool
}

type symInfo struct {
	rname string
	ok    bool
	refs  map[string]bool
}

func (m *Makefile) lower() *Result {
	m.searchCache = map[string]*match{}
	l := &lowering{m: m, nodes: map[string]*node{}, symUsed: map[string]bool{}, symInfo: map[string]*symInfo{}, owner: map[string]string{}}

	// Roots: explicit targets in definition order, then goals.
	var roots []*target
	for _, t := range m.targets {
		roots = append(roots, t)
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].order < roots[j].order })
	for _, t := range roots {
		if !specialTargets[t.name] {
			l.visit(t.name)
		}
	}
	goal := m.defaultGoal()
	if goal != "" {
		l.visit(goal)
	}
	for _, g := range m.opts.Goals {
		l.visit(g)
	}

	// Recipes, in order.
	for _, n := range l.list {
		if n.alias == nil && (n.kind == kRule || n.kind == kTask) {
			l.lowerRecipe(n)
		}
	}
	l.taskHeuristic()
	l.checkInheritance()
	if m.depIncludes && !l.usesMD {
		m.pos = syntax.Pos{File: m.opts.File, Line: 1, Col: 1}
		m.note(false, "dependency files (.d) are included, but no recipe writes them with -MD or -MMD: header dependencies are lost")
	}
	if m.depIncludes {
		m.pos = syntax.Pos{File: m.opts.File, Line: 1, Col: 1}
		m.note(false, "included .d files dropped: rules compiled with -MD/-MMD use deps gcc instead")
	}

	var patterns string
	if m.opts.Fragment {
		patterns = l.exportPatterns()
	}
	body := patterns + l.emitNodes()
	vars := l.emitVars()

	var b strings.Builder
	b.WriteString("# Imported from " + m.opts.File + " by rmake.\n")
	notes := append(m.notes, l.notes...)
	sort.SliceStable(notes, func(i, j int) bool {
		a, c := notes[i].Pos, notes[j].Pos
		if a.File != c.File {
			return a.File < c.File
		}
		return a.Line < c.Line
	})
	for _, n := range notes {
		b.WriteString("# rmake: " + n.String() + "\n")
	}

	if vars != "" {
		b.WriteString("\n" + vars)
	}
	if goal != "" && !m.opts.NoDefault {
		if n := l.nodes[goal]; n != nil {
			if name, ok := l.ref(n); ok {
				b.WriteString("\ndefault " + name + "\n")
			}
		}
	}
	b.WriteString(body)
	// Parse and print: the result is canonical, and an importer bug shows
	// up here rather than as a confusing error later.
	f, err := syntax.Parse(m.opts.File, []byte(b.String()))
	if err != nil {
		m.pos = syntax.Pos{File: m.opts.File, Line: 1, Col: 1}
		m.fail("internal error: the import is not a valid build file: %v\n%s", err, b.String())
	}
	return &Result{Source: syntax.Format(f), Notes: notes, Dropped: l.dropped}
}

func (m *Makefile) defaultGoal() string {
	if v, ok := m.vars[".DEFAULT_GOAL"]; ok {
		if g := strings.TrimSpace(m.valueOf(".DEFAULT_GOAL", m.global())); g != "" {
			_ = v
			return g
		}
	}
	return m.firstGoal
}

// visit analyses a target and everything it depends on.
func (l *lowering) visit(name string) *node {
	if n, ok := l.nodes[name]; ok {
		return n
	}
	m := l.m
	n := &node{name: name, outs: []string{name}, order: 1 << 30}
	l.nodes[name] = n
	l.seq++
	n.order += l.seq
	t := m.targets[name]
	if t != nil {
		n.order, n.pos, n.explicit = t.order, t.pos, true
		n.prereqs, n.orderOnly = t.prereqs, t.orderOnly
	}
	switch {
	case m.phony[name]:
		n.kind = kTask
		if t != nil {
			n.recipe = t.recipe
		}
	case t != nil && t.recipe != nil:
		n.kind = kRule
		n.recipe, n.stem = t.recipe, t.stem
		if t.stem == "" {
			n.stem = m.suffixStem(name)
		}
		if len(t.group) > 1 {
			n.outs = t.group
		}
		if t.recipe.pos.Line > 0 {
			n.pos = t.recipe.pos
		}
	default:
		if mt := m.search(name, false, nil); mt != nil {
			n.kind = kRule
			n.fromRule = mt.rule
			n.recipe = mt.rule.recipe
			n.stem = mt.stem
			n.prereqs = append(append([]string(nil), mt.prereqs...), n.prereqs...)
			n.orderOnly = append(append([]string(nil), mt.orderOnly...), n.orderOnly...)
			if len(mt.targets) > 1 {
				n.outs = mt.targets
			}
			if mt.rule.pos.Line > 0 && t == nil {
				n.pos = mt.rule.pos
			}
		} else if m.exist(name) {
			n.kind = kSource
		} else if t != nil {
			n.kind = kPseudo
		} else {
			n.kind = kMissing
		}
	}
	if n.kind == kRule && (strings.HasSuffix(name, "/") || m.isDir(name) || isMkdirRecipe(n.recipe)) {
		n.kind = kDir
		for _, rl := range n.recipe.lines {
			l.dropped = append(l.dropped, rl.text)
		}
	}
	if n.kind == kRule && knownTasks[name] && !m.exist(name) {
		n.kind = kTask
		m.pos = n.pos
		m.note(false, "%s is not marked .PHONY; imported as a task", name)
	}
	// Other outputs of a grouped rule share this node.
	for _, o := range n.outs {
		if o != name {
			if _, ok := l.nodes[o]; !ok {
				l.nodes[o] = &node{name: o, alias: n, kind: n.kind}
			}
		}
	}
	l.list = append(l.list, n)
	for _, p := range n.prereqs {
		l.visit(p)
	}
	for _, p := range n.orderOnly {
		l.visit(p)
	}
	return n
}

var statFn = os.Stat

func (m *Makefile) isDir(name string) bool {
	p := name
	if !path.IsAbs(p) {
		p = path.Join(m.absRoot, p)
	}
	fi, err := statFn(p)
	return err == nil && fi.IsDir()
}

// isMkdirRecipe reports whether every recipe line is a mkdir.
func isMkdirRecipe(r *recipe) bool {
	if r == nil || len(r.lines) == 0 {
		return false
	}
	for _, rl := range r.lines {
		s := strings.TrimLeft(rl.text, " \t@-+")
		if !strings.HasPrefix(s, "mkdir ") && !strings.HasPrefix(s, "$(MKDIR") && !strings.HasPrefix(s, "${MKDIR") {
			return false
		}
	}
	return true
}

// suffixStem is $* of an explicit rule: the target without a known suffix.
func (m *Makefile) suffixStem(name string) string {
	for _, s := range m.suffixes {
		if strings.HasSuffix(name, s) && len(name) > len(s) {
			return strings.TrimSuffix(name, s)
		}
	}
	return ""
}

func clean(p string) string {
	if p == "" {
		return p
	}
	return path.Clean(p)
}

// targetVars builds the target- and pattern-specific variables of a target.
func (m *Makefile) targetVars(name string) map[string]*variable {
	tvs := m.patternVarsFor(name)
	if t := m.targets[name]; t != nil {
		tvs = append(tvs, t.vars...)
	}
	if len(tvs) == 0 {
		return nil
	}
	vars := map[string]*variable{}
	for _, tv := range tvs {
		cur := vars[tv.name]
		if cur == nil {
			cur = m.vars[tv.name]
		}
		if cur != nil && cur.origin == oCmdline && tv.org != oOverride {
			continue
		}
		switch tv.op {
		case "=":
			vars[tv.name] = &variable{name: tv.name, value: tv.value, flavor: recursive, origin: oFile}
		case ":=", "::=":
			vars[tv.name] = &variable{name: tv.name, value: tv.value, flavor: simple, origin: oFile}
		case "?=":
			if cur == nil {
				vars[tv.name] = &variable{name: tv.name, value: tv.value, flavor: recursive, origin: oFile}
			}
		case "+=":
			if cur == nil {
				vars[tv.name] = &variable{name: tv.name, value: tv.value, flavor: recursive, origin: oFile}
				continue
			}
			nv := *cur
			add := tv.value
			if nv.flavor == simple {
				add = m.expand(add, m.global())
			}
			if nv.value == "" {
				nv.value = add
			} else {
				nv.value += " " + add
			}
			vars[tv.name] = &nv
		default:
			m.pos = tv.pos
			m.fail("target-specific %s assignments are not supported", tv.op)
		}
	}
	return vars
}

// inputs maps a rule's prerequisites to rmake inputs. kept are the normal
// prerequisites kept as inputs (cleaned, without duplicates, in order);
// extra are inputs from flattened prerequisites and order-only files.
func (l *lowering) inputs(n *node) (kept, extra []string, always bool) {
	seen := map[string]bool{}
	var flatten func(p *node, depth int)
	add := func(list *[]string, p string) {
		c := clean(p)
		if !seen[c] {
			seen[c] = true
			*list = append(*list, c)
		}
	}
	flatten = func(p *node, depth int) {
		if depth > 50 {
			return
		}
		for _, q := range append(append([]string(nil), p.prereqs...), p.orderOnly...) {
			qn := l.nodes[q]
			switch l.kindOf(qn) {
			case kDir:
			case kTask, kPseudo:
				always = true
				flatten(qn, depth+1)
			case kSource:
				add(&extra, q)
				flatten(qn, depth+1)
			default:
				add(&extra, q)
			}
		}
	}
	for _, p := range n.prereqs {
		pn := l.nodes[p]
		switch l.kindOf(pn) {
		case kDir:
			l.m.pos = n.pos
			l.m.note(false, "directory prerequisite %s of %s dropped (rmake creates output directories)", p, n.name)
		case kTask:
			if pn.recipe != nil && len(pn.recipe.lines) > 0 {
				l.m.pos = n.pos
				l.m.note(true, "%s depends on the task %s, which has a recipe; rmake rules cannot run tasks first", n.name, p)
			}
			always = true
			flatten(pn, 0)
		case kPseudo:
			always = true
			flatten(pn, 0)
		case kSource:
			add(&kept, p)
			flatten(pn, 0)
		default:
			add(&kept, p)
		}
	}
	for _, p := range n.orderOnly {
		pn := l.nodes[p]
		switch l.kindOf(pn) {
		case kDir:
		case kTask, kPseudo:
			flatten(pn, 0)
		default:
			add(&extra, p)
		}
	}
	// Explicit inputs listed again among the extra ones.
	k := map[string]bool{}
	for _, p := range kept {
		k[p] = true
	}
	out := extra[:0]
	for _, p := range extra {
		if !k[p] {
			out = append(out, p)
		}
	}
	return kept, out, always
}

func (l *lowering) kindOf(n *node) kind {
	if n == nil {
		return kMissing
	}
	if n.alias != nil {
		n = n.alias
	}
	if (n.kind == kRule || n.kind == kTask) && n.empty {
		if n.kind == kRule && l.m.exist(n.name) {
			return kSource
		}
		if n.kind == kRule {
			return kPseudo
		}
	}
	return n.kind
}

func dedup(ws []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range ws {
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ruleSym decides symbolic references in one rule's recipe.
type ruleSym struct {
	l        *lowering
	n        *node
	explicit []string // the rule's explicit inputs (cleaned)
	task     bool
}

func (s *ruleSym) automatic(name string) (string, bool) {
	if s.task {
		return "", false
	}
	n := s.n
	switch name {
	case "@":
		if len(n.outs) == 1 && clean(n.outs[0]) == n.outs[0] {
			return "out", true
		}
	case "<":
		if len(n.prereqs) > 0 && len(s.explicit) == 1 && s.explicit[0] == n.prereqs[0] {
			return "in", true
		}
	case "^", "?":
		if equal(s.explicit, dedup(n.prereqs)) {
			return "in", true
		}
	case "+":
		if equal(s.explicit, n.prereqs) {
			return "in", true
		}
	}
	return "", false
}

func (s *ruleSym) variable(name string, c *ctx) (string, bool) {
	r, ok := s.l.symbolic(name, c)
	if ok {
		s.l.symUsed[name] = true
	}
	return r, ok
}

// symbolic reports whether a reference to make variable name may stay
// symbolic in context c, and returns its rmake name.
func (l *lowering) symbolic(name string, c *ctx) (string, bool) {
	if _, ok := c.target[name]; ok {
		return "", false
	}
	info := l.info(name)
	if !info.ok {
		return "", false
	}
	for r := range info.refs {
		if _, ok := c.target[r]; ok {
			return "", false
		}
	}
	return info.rname, true
}

var reservedNames = map[string]bool{"os": true, "arch": true, "builddir": true, "in": true, "out": true, "stem": true}

// info computes whether a variable can be emitted as an rmake variable.
func (l *lowering) info(name string) *symInfo {
	if si, ok := l.symInfo[name]; ok {
		return si
	}
	si := &symInfo{}
	l.symInfo[name] = si
	if name == "" || strings.HasPrefix(name, ".") || name == "MAKE" || name == "MAKEFLAGS" ||
		name == "MAKECMDGOALS" || name == "MFLAGS" || name == "SHELL" || name == "MAKEFILE_LIST" {
		return si
	}
	r := rmakeName(name)
	if r == "" || reservedNames[r] {
		return si
	}
	if o, ok := l.owner[r]; ok && o != name {
		return si
	}
	t := &taint{}
	val := l.m.valueOf(name, &ctx{taint: t})
	if t.auto || t.side || strings.Contains(val, "\n") {
		return si
	}
	l.owner[r] = name
	si.rname, si.ok, si.refs = r, true, t.refs
	return si
}

// rmakeName maps a make variable name to an rmake name ("" if impossible).
func rmakeName(name string) string {
	b := []byte(name)
	for i, c := range b {
		if !(c == '_' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9') {
			if c == '.' || c == '-' {
				b[i] = '_'
				continue
			}
			return ""
		}
	}
	s := string(b)
	if s[0] >= '0' && s[0] <= '9' {
		s = "_" + s
	}
	return s
}

// lowerRecipe expands a rule's or task's recipe into sh strings.
func (l *lowering) lowerRecipe(n *node) {
	m := l.m
	n.vars = m.targetVars(n.name)
	prereqs := dedup(n.prereqs)
	first := ""
	if len(n.prereqs) > 0 {
		first = n.prereqs[0]
	}
	auto := map[string]string{
		"@": n.outs[0], "<": first, "^": strings.Join(prereqs, " "), "+": strings.Join(n.prereqs, " "),
		"?": strings.Join(prereqs, " "), "|": strings.Join(dedup(n.orderOnly), " "), "*": n.stem, "%": "",
	}
	if n.kind == kTask {
		auto["@"] = n.name
	}
	if n.recipe == nil {
		n.empty = true
		return
	}
	// Probe: which automatic variables does the recipe use, and does it
	// expand to anything?
	probe := &taint{}
	pc := &ctx{auto: auto, target: n.vars, taint: probe}
	nonEmpty := false
	for _, rl := range n.recipe.lines {
		m.pos = rl.pos
		text := strings.TrimSpace(m.expand(rl.text, pc))
		text = strings.TrimLeft(text, "@-+ \t")
		if text != "" {
			nonEmpty = true
		}
	}
	n.refs = probe.refs
	if !nonEmpty {
		n.empty = true
		return
	}
	uses := l.autosUsed(n, auto)
	n.usesTarget = uses["@"]
	var explicit []string
	if n.kind == kRule {
		kept, _, _ := l.inputs(n)
		if uses["^"] || uses["+"] || uses["?"] {
			explicit = kept
		} else if len(kept) > 0 && len(n.prereqs) > 0 && kept[0] == n.prereqs[0] {
			explicit = kept[:1]
		}
		if uses["?"] {
			m.pos = n.recipe.pos
			m.note(false, "$? in the recipe of %s is replaced by all inputs ($in): rmake has no \"newer than the target\" set", n.name)
		}
	}
	n.explicitIns = explicit
	sym := &ruleSym{l: l, n: n, explicit: explicit, task: n.kind == kTask}
	c := &ctx{auto: auto, target: n.vars}
	for _, rl := range n.recipe.lines {
		m.pos = rl.pos
		l.lowerLine(n, rl, c, sym)
	}
}

// autosUsed returns the automatic variables a recipe references.
func (l *lowering) autosUsed(n *node, auto map[string]string) map[string]bool {
	uses := map[string]bool{}
	for k := range auto {
		probe := map[string]string{}
		for kk, v := range auto {
			probe[kk] = v
		}
		probe[k] = "\x00MARK\x00"
		c := &ctx{auto: probe, target: n.vars}
		for _, rl := range n.recipe.lines {
			if strings.Contains(l.m.expand(rl.text, c), "\x00MARK\x00") {
				uses[k] = true
				break
			}
		}
	}
	return uses
}

// lowerLine lowers one recipe line (which may expand to several commands).
func (l *lowering) lowerLine(n *node, rl recipeLine, c *ctx, sym symbolicer) {
	m := l.m
	t := &taint{}
	tc := *c
	tc.taint = t
	tm := m.template(rl.text, &tc, sym)
	if t.refs["MAKE"] {
		m.note(true, "recursive make in the recipe of %s is not supported by the importer yet", n.name)
	}
	conc := concrete(tm)
	if strings.Contains(strings.ReplaceAll(conc, "\\\n", ""), "\n") {
		// A multi-line value (define): each line is a command; no
		// symbolic references.
		for _, line := range strings.Split(conc, "\n") {
			l.addCmd(n, []seg{{lit: line}})
		}
		return
	}
	l.addCmd(n, tm)
}

// addCmd turns a template into an sh command: prefixes, continuations,
// ignore-errors, -MD detection.
func (l *lowering) addCmd(n *node, tm []seg) {
	m := l.m
	conc := concrete(tm)
	// Prefix characters.
	trimmed := strings.TrimLeft(conc, " \t@-+")
	prefix := conc[:len(conc)-len(trimmed)]
	if prefix != "" {
		if len(tm) == 0 || tm[0].ref != "" || !strings.HasPrefix(tm[0].lit, prefix) {
			tm = []seg{{lit: conc}}
		}
		tm[0].lit = tm[0].lit[len(prefix):]
	}
	ignore := strings.Contains(prefix, "-")
	// Backslash-newline: removed, as the shell does outside single quotes.
	if strings.Contains(conc, "\\\n") {
		if quotedContinuation(conc) {
			m.note(true, "a recipe line of %s continues inside single quotes; the importer joins the lines", n.name)
		}
		for i := range tm {
			if tm[i].ref == "" {
				tm[i].lit = strings.ReplaceAll(tm[i].lit, "\\\n", "")
			}
		}
	}
	text := concrete(tm)
	if strings.TrimSpace(text) == "" {
		return
	}
	if n.kind == kRule {
		l.depfile(n, text)
	}
	s := shString(tm)
	if ignore {
		s = "(" + s + ") || true"
	}
	n.cmds = append(n.cmds, s)
}

// quotedContinuation reports whether a backslash-newline occurs inside
// single quotes.
func quotedContinuation(s string) bool {
	single, double := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case single:
			if c == '\'' {
				single = false
			} else if c == '\\' && i+1 < len(s) && s[i+1] == '\n' {
				return true
			}
		case c == '\\':
			i++
		case c == '\'' && !double:
			single = true
		case c == '"':
			double = !double
		}
	}
	return false
}

// depfile detects gcc -MD/-MMD and records the depfile.
func (l *lowering) depfile(n *node, cmd string) {
	ws := strings.Fields(cmd)
	md := false
	for _, w := range ws {
		if w == "-MD" || w == "-MMD" {
			md = true
		}
	}
	if !md {
		return
	}
	l.usesMD = true
	df := ""
	for i, w := range ws {
		switch {
		case w == "-MF" && i+1 < len(ws):
			df = ws[i+1]
		case strings.HasPrefix(w, "-MF") && len(w) > 3:
			df = w[3:]
		}
	}
	if df == "" {
		for i, w := range ws {
			if w == "-o" && i+1 < len(ws) {
				o := ws[i+1]
				if ext := path.Ext(o); ext != "" {
					o = strings.TrimSuffix(o, ext)
				}
				df = o + ".d"
			}
		}
	}
	if df == "" {
		return
	}
	n.depfile = clean(df)
}

// shString renders a template as the text of an sh "..." string.
func shString(tm []seg) string {
	var b strings.Builder
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `$$`)
	for i, s := range tm {
		if s.ref == "" {
			b.WriteString(esc.Replace(s.lit))
			continue
		}
		if i+1 < len(tm) && tm[i+1].ref == "" && tm[i+1].lit != "" && isNameByte(tm[i+1].lit[0]) {
			b.WriteString("${" + s.ref + "}")
		} else if i+1 < len(tm) && tm[i+1].ref != "" {
			b.WriteString("${" + s.ref + "}")
		} else {
			b.WriteString("$" + s.ref)
		}
	}
	return b.String()
}

func isNameByte(c byte) bool {
	return c == '_' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
}

// taskHeuristic imports a rule as a task when it is plainly not a file: its
// recipe never names the target ($@ or the name itself), the file does not
// exist, and no rule depends on it.
func (l *lowering) taskHeuristic() {
	needed := map[string]bool{}
	for _, n := range l.list {
		if n.kind == kRule && n.alias == nil {
			for _, p := range append(append([]string(nil), n.prereqs...), n.orderOnly...) {
				needed[p] = true
			}
		}
	}
	for _, n := range l.list {
		if n.kind != kRule || n.alias != nil || n.empty || needed[n.name] || l.m.exist(n.name) || n.usesTarget {
			continue
		}
		mentions := false
		for _, c := range n.cmds {
			if strings.Contains(c, n.name) || strings.Contains(c, "$out") {
				mentions = true
			}
		}
		if !mentions {
			n.kind = kTask
			l.m.pos = n.pos
			l.m.note(false, "%s is not marked .PHONY and its recipe does not create it; imported as a task", n.name)
		}
	}
}

// exportPatterns renders the makefile's own pattern rules as rmake pattern
// rules, for makefiles included from an Rmakefile (whose targets the
// importer does not know).
func (l *lowering) exportPatterns() string {
	m := l.m
	l.exported = map[*patternRule]bool{}
	var b strings.Builder
	for _, p := range m.allPatterns() {
		if p.builtin {
			continue
		}
		m.pos = p.pos
		if len(p.targets) != 1 || len(p.prereqs) == 0 || strings.Count(p.prereqs[0], "%") != 1 {
			m.note(true, "pattern rule %s cannot be exported as an rmake pattern rule (one target pattern and a first prerequisite with %% are needed)", strings.Join(p.targets, " "))
			continue
		}
		const mark = "\x00"
		auto := map[string]string{}
		for _, a := range []string{"@", "<", "^", "+", "*", "?", "|", "%"} {
			auto[a] = mark + a + mark
		}
		n := &node{name: p.targets[0], outs: p.targets, prereqs: p.prereqs, orderOnly: p.orderOnly, recipe: p.recipe, kind: kRule}
		uses := l.autosUsed(n, auto)
		explicit := []string{p.prereqs[0]}
		if uses["^"] || uses["+"] || uses["?"] {
			explicit = dedup(p.prereqs)
		}
		sym := &patternSym{ruleSym: ruleSym{l: l, n: n, explicit: explicit}}
		c := &ctx{auto: auto}
		ok := true
		for _, rl := range p.recipe.lines {
			m.pos = rl.pos
			l.lowerLine(n, rl, c, sym)
		}
		for _, cmd := range n.cmds {
			if strings.Contains(cmd, mark) {
				ok = false
			}
		}
		if !ok || len(n.cmds) == 0 {
			m.pos = p.pos
			m.note(true, "pattern rule %s uses automatic variables rmake pattern rules cannot express", p.targets[0])
			continue
		}
		l.exported[p] = true
		m.pos = p.pos
		m.note(false, "pattern rule %s exported as an rmake pattern rule: it is instantiated for every existing %s, and %% does not match '/'", p.targets[0], p.prereqs[0])
		k := map[string]bool{}
		b.WriteString("\nrule " + p.targets[0] + " from")
		for _, x := range explicit {
			k[x] = true
			b.WriteString(" " + quoteWord(x))
		}
		var implicit []string
		for _, x := range append(dedup(p.prereqs), p.orderOnly...) {
			if !k[x] {
				implicit = append(implicit, quoteWord(x))
			}
		}
		if len(implicit) > 0 {
			b.WriteString(" | " + strings.Join(implicit, " "))
		}
		b.WriteString("\n")
		if n.depfile != "" {
			b.WriteString("  deps gcc\n")
			if n.depfile != p.targets[0]+".d" {
				b.WriteString("  depfile " + quoteWord(n.depfile) + "\n")
			}
		}
		for _, cmd := range n.cmds {
			b.WriteString("  sh \"" + cmd + "\"\n")
		}
	}
	return b.String()
}

// patternSym is ruleSym for exported pattern rules: $* is $stem.
type patternSym struct{ ruleSym }

func (s *patternSym) automatic(name string) (string, bool) {
	if name == "*" {
		return "stem", true
	}
	if name == "@" {
		return "out", true
	}
	return s.ruleSym.automatic(name)
}

// checkInheritance notes target-specific variables that make would pass on
// to the prerequisites of a target.
func (l *lowering) checkInheritance() {
	for _, n := range l.list {
		t := l.m.targets[n.name]
		if t == nil || len(t.vars) == 0 {
			continue
		}
		names := map[string]bool{}
		for _, tv := range t.vars {
			names[tv.name] = true
		}
		seen := map[string]bool{}
		var walk func(p string, depth int)
		walk = func(p string, depth int) {
			pn := l.nodes[p]
			if pn == nil || seen[p] || depth > 100 {
				return
			}
			seen[p] = true
			if pn.alias != nil {
				pn = pn.alias
			}
			for r := range pn.refs {
				if names[r] && (pn.vars == nil || pn.vars[r] == nil) {
					l.m.pos = t.pos
					l.m.note(true, "target-specific variable %s of %s is inherited by %s in make; the importer does not pass it on", r, n.name, pn.name)
					return
				}
			}
			for _, q := range append(append([]string(nil), pn.prereqs...), pn.orderOnly...) {
				walk(q, depth+1)
			}
		}
		for _, q := range append(append([]string(nil), n.prereqs...), n.orderOnly...) {
			walk(q, 0)
		}
	}
}

// ref returns the rmake name of a node for needs/default (a task name or
// a path).
func (l *lowering) ref(n *node) (string, bool) {
	switch l.kindOf(n) {
	case kTask, kPseudo:
		if syntax.IsTaskName(n.name) {
			return n.name, true
		}
		return "", false
	case kDir:
		return "", false
	}
	return quoteWord(clean(n.name)), true
}

// emitNodes renders rules and tasks in definition order.
func (l *lowering) emitNodes() string {
	m := l.m
	nodes := append([]*node(nil), l.list...)
	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].order < nodes[j].order })
	// Targets that tasks need or that are goals.
	wanted := map[string]bool{m.defaultGoal(): true}
	for _, g := range m.opts.Goals {
		wanted[g] = true
	}
	for _, n := range nodes {
		if k := l.kindOf(n); k == kTask || k == kPseudo {
			for _, p := range append(append([]string(nil), n.prereqs...), n.orderOnly...) {
				wanted[p] = true
			}
		}
	}
	var b strings.Builder
	for _, n := range nodes {
		if n.alias != nil {
			continue
		}
		switch l.kindOf(n) {
		case kRule:
			if n.fromRule != nil && l.exported[n.fromRule] {
				continue // made by the exported pattern rule
			}
			kept, extra, always := l.inputs(n)
			explicit := n.explicitIns
			k := map[string]bool{}
			for _, p := range explicit {
				k[p] = true
			}
			var implicit []string
			for _, p := range kept {
				if !k[p] {
					implicit = append(implicit, p)
				}
			}
			implicit = append(implicit, extra...)
			b.WriteString("\nrule")
			for _, o := range n.outs {
				b.WriteString(" " + quoteWord(clean(o)))
			}
			b.WriteString(" from")
			for _, p := range explicit {
				b.WriteString(" " + quoteWord(p))
			}
			if len(implicit) > 0 {
				b.WriteString(" |")
				for _, p := range implicit {
					b.WriteString(" " + quoteWord(p))
				}
			}
			b.WriteString("\n")
			if always {
				b.WriteString("  always\n")
			}
			if n.depfile != "" {
				b.WriteString("  deps gcc\n")
				if n.depfile != clean(n.outs[0])+".d" {
					b.WriteString("  depfile " + quoteWord(n.depfile) + "\n")
				}
			}
			l.writePass(&b)
			for _, c := range n.cmds {
				b.WriteString("  sh \"" + c + "\"\n")
			}
		case kTask, kPseudo:
			if l.kindOf(n) == kPseudo && len(n.prereqs)+len(n.orderOnly) == 0 && !wanted[n.name] {
				continue // FORCE-like: folded into always where used
			}
			if !syntax.IsTaskName(n.name) {
				m.pos = n.pos
				m.note(true, "%s cannot be a task name; it is left out", n.name)
				continue
			}
			b.WriteString("\ntask " + n.name)
			var needs []string
			for _, p := range dedup(append(append([]string(nil), n.prereqs...), n.orderOnly...)) {
				pn := l.nodes[p]
				if pn == nil {
					continue
				}
				if r, ok := l.ref(pn); ok {
					needs = append(needs, r)
				}
			}
			if len(needs) > 0 {
				b.WriteString(" needs " + strings.Join(needs, " "))
			}
			b.WriteString("\n")
			if len(n.cmds) > 0 {
				l.writePass(&b)
			}
			for _, c := range n.cmds {
				b.WriteString("  sh \"" + c + "\"\n")
			}
		}
	}
	return b.String()
}

// writePass writes a pass attribute for exported variables.
func (l *lowering) writePass(b *strings.Builder) {
	if len(l.m.exports) == 0 {
		return
	}
	var names []string
	for e := range l.m.exports {
		si := l.info(e)
		if !si.ok {
			continue
		}
		l.symUsed[e] = true
		names = append(names, si.rname)
	}
	sort.Strings(names)
	if len(names) > 0 {
		b.WriteString("  pass " + strings.Join(names, " ") + "\n")
	}
}

// emitVars renders the symbolic variables used, dependencies first.
func (l *lowering) emitVars() string {
	m := l.m
	lines := map[string][]string{}
	deps := map[string][]string{}
	// The closure: emitting a variable can make others symbolic.
	for {
		var todo []string
		for name := range l.symUsed {
			if _, ok := lines[name]; !ok {
				todo = append(todo, name)
			}
		}
		if len(todo) == 0 {
			break
		}
		sort.Strings(todo)
		for _, name := range todo {
			lines[name], deps[name] = l.varLines(name)
		}
	}
	var names []string
	for name := range lines {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	done := map[string]bool{}
	var emit func(name string)
	emit = func(name string) {
		if done[name] {
			return
		}
		done[name] = true
		for _, d := range deps[name] {
			emit(d)
		}
		for _, line := range lines[name] {
			b.WriteString(line + "\n")
		}
	}
	for _, name := range names {
		emit(name)
	}
	_ = m
	return b.String()
}

// varLines renders the definition of one variable and lists the variables
// it references.
func (l *lowering) varLines(name string) ([]string, []string) {
	m := l.m
	r := l.info(name).rname
	var deps []string
	tmpl := func(text string, fl flavor) string {
		if fl == simple {
			return words(text)
		}
		s, d := l.varTemplate(text)
		deps = append(deps, d...)
		return s
	}
	line := func(op, value string) string {
		if value == "" {
			return "var " + r + " " + op
		}
		return "var " + r + " " + op + " " + value
	}
	v := m.vars[name]
	switch {
	case v == nil:
		return []string{line("?=", "")}, nil
	case v.origin == oCmdline || v.origin == oOverride:
		return []string{line("=", words(m.valueOf(name, m.global())))}, nil
	case v.envable:
		out := []string{line("?=", tmpl(v.base.value, v.base.flavor))}
		for _, p := range v.appends {
			out = append(out, line("+=", tmpl(p.value, p.flavor)))
		}
		return out, deps
	default:
		return []string{line("=", tmpl(v.value, v.flavor))}, deps
	}
}

// varSym keeps whole-word references symbolic in variable values.
type varSym struct{ l *lowering }

func (s varSym) automatic(string) (string, bool) { return "", false }
func (s varSym) variable(name string, c *ctx) (string, bool) {
	return s.l.symbolic(name, c)
}

// varTemplate renders a recursive variable value as rmake words, keeping
// references that are whole words symbolic.
func (l *lowering) varTemplate(text string) (string, []string) {
	tm := l.m.template(text, l.m.global(), varSym{l})
	// References that are not whole words become literal.
	var fixed []seg
	for i, s := range tm {
		if s.ref != "" {
			before := i == 0 || tm[i-1].ref == "" && endsSpace(tm[i-1].lit)
			after := i == len(tm)-1 || tm[i+1].ref == "" && startsSpace(tm[i+1].lit)
			if !before || !after {
				s = seg{lit: s.val}
			}
		}
		if k := len(fixed) - 1; s.ref == "" && k >= 0 && fixed[k].ref == "" {
			fixed[k].lit += s.lit
			continue
		}
		fixed = append(fixed, s)
	}
	var out []string
	var deps []string
	for _, s := range fixed {
		if s.ref != "" {
			out = append(out, "$"+s.ref)
			name := l.owner[s.ref]
			l.symUsed[name] = true
			deps = append(deps, name)
			continue
		}
		for _, w := range strings.Fields(s.lit) {
			out = append(out, quoteWord(w))
		}
	}
	return strings.Join(out, " "), deps
}

func endsSpace(s string) bool   { return s != "" && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') }
func startsSpace(s string) bool { return s != "" && (s[0] == ' ' || s[0] == '\t') }

// words renders text as rmake words.
func words(text string) string {
	var out []string
	for _, w := range strings.Fields(text) {
		out = append(out, quoteWord(w))
	}
	return strings.Join(out, " ")
}

// quoteWord renders one word, quoting it if needed.
func quoteWord(w string) string {
	safe := w != ""
	for i := 0; i < len(w); i++ {
		c := w[i]
		if !(isNameByte(c) || strings.IndexByte("./-+,:=@^~%", c) >= 0) {
			safe = false
			break
		}
	}
	if safe {
		return w
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `$$`).Replace(w) + `"`
}
