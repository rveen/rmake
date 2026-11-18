package makefile

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

var sprintf = fmt.Sprintf

// defaultSuffixes is GNU make's default .SUFFIXES list.
var defaultSuffixes = strings.Fields(".out .a .ln .o .c .cc .C .cpp .p .f .F .m .r .y .l .ym .yl .s .S .mod .sym .def .h .info .dvi .tex .texinfo .texi .txinfo .w .ch .web .sh .elc .el")

// builtinVars are GNU make's default variables for the supported built-in
// rules (C, C++, assembler, linking, lex, yacc, ar).
var builtinVars = [][2]string{
	{"AR", "ar"}, {"ARFLAGS", "-rv"}, {"AS", "as"}, {"CC", "cc"}, {"CXX", "g++"},
	{"CPP", "$(CC) -E"}, {"LEX", "lex"}, {"YACC", "yacc"}, {"RM", "rm -f"}, {"MAKE", "make"},
	{"SHELL", "/bin/sh"},
	{"OUTPUT_OPTION", "-o $@"},
	{"COMPILE.c", "$(CC) $(CFLAGS) $(CPPFLAGS) $(TARGET_ARCH) -c"},
	{"COMPILE.cc", "$(CXX) $(CXXFLAGS) $(CPPFLAGS) $(TARGET_ARCH) -c"},
	{"COMPILE.C", "$(COMPILE.cc)"},
	{"COMPILE.cpp", "$(COMPILE.cc)"},
	{"COMPILE.s", "$(AS) $(ASFLAGS) $(TARGET_MACH)"},
	{"COMPILE.S", "$(CC) $(ASFLAGS) $(CPPFLAGS) $(TARGET_MACH) -c"},
	{"PREPROCESS.S", "$(CPP) $(CPPFLAGS)"},
	{"LINK.o", "$(CC) $(LDFLAGS) $(TARGET_ARCH)"},
	{"LINK.c", "$(CC) $(CFLAGS) $(CPPFLAGS) $(LDFLAGS) $(TARGET_ARCH)"},
	{"LINK.cc", "$(CXX) $(CXXFLAGS) $(CPPFLAGS) $(LDFLAGS) $(TARGET_ARCH)"},
	{"LINK.C", "$(LINK.cc)"},
	{"LINK.cpp", "$(LINK.cc)"},
	{"LINK.s", "$(CC) $(ASFLAGS) $(LDFLAGS) $(TARGET_MACH)"},
	{"LINK.S", "$(CC) $(ASFLAGS) $(CPPFLAGS) $(LDFLAGS) $(TARGET_MACH)"},
	{"YACC.y", "$(YACC) $(YFLAGS)"},
	{"LEX.l", "$(LEX) $(LFLAGS) -t"},
}

// builtinSuffixRules are GNU make's built-in suffix rules for the supported
// languages, by suffix rule name.
var builtinSuffixRules = map[string][]string{
	".c.o":   {"$(COMPILE.c) $(OUTPUT_OPTION) $<"},
	".cc.o":  {"$(COMPILE.cc) $(OUTPUT_OPTION) $<"},
	".C.o":   {"$(COMPILE.C) $(OUTPUT_OPTION) $<"},
	".cpp.o": {"$(COMPILE.cpp) $(OUTPUT_OPTION) $<"},
	".s.o":   {"$(COMPILE.s) -o $@ $<"},
	".S.o":   {"$(COMPILE.S) -o $@ $<"},
	".S.s":   {"$(PREPROCESS.S) $< > $@"},
	".y.c":   {"$(YACC.y) $<", "mv -f y.tab.c $@"},
	".l.c":   {"@$(RM) $@", "$(LEX.l) $< > $@"},
	".o":     {"$(LINK.o) $^ $(LOADLIBES) $(LDLIBS) -o $@"},
	".c":     {"$(LINK.c) $^ $(LOADLIBES) $(LDLIBS) -o $@"},
	".cc":    {"$(LINK.cc) $^ $(LOADLIBES) $(LDLIBS) -o $@"},
	".C":     {"$(LINK.C) $^ $(LOADLIBES) $(LDLIBS) -o $@"},
	".cpp":   {"$(LINK.cpp) $^ $(LOADLIBES) $(LDLIBS) -o $@"},
	".s":     {"$(LINK.s) $^ $(LOADLIBES) $(LDLIBS) -o $@"},
	".S":     {"$(LINK.S) $^ $(LOADLIBES) $(LDLIBS) -o $@"},
	".sh":    {"cat $< >$@", "chmod a+x $@"},
}

// suffixPattern converts a suffix rule name to a pattern rule.
func (m *Makefile) suffixPattern(name string, rec *recipe, builtin bool) *patternRule {
	for _, s := range m.suffixes {
		if !strings.HasPrefix(name, s) {
			continue
		}
		t := name[len(s):]
		if t == "" {
			return &patternRule{targets: []string{"%"}, prereqs: []string{"%" + s}, recipe: rec, builtin: builtin, suffixes: []string{s}}
		}
		for _, x := range m.suffixes {
			if x == t {
				return &patternRule{targets: []string{"%" + t}, prereqs: []string{"%" + s}, recipe: rec, builtin: builtin, suffixes: []string{s, t}}
			}
		}
	}
	return nil
}

// allPatterns returns the pattern rules in search order: the makefile's
// pattern rules, then its suffix rules, then the built-in rules not
// cancelled or replaced; suffix rules in the order of .SUFFIXES.
func (m *Makefile) allPatterns() []*patternRule {
	if m.ruleCache != nil {
		return m.ruleCache
	}
	same := func(a, b []string) bool {
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
	// A pattern rule replaces an earlier one with the same targets and
	// prerequisites; without a recipe it cancels it.
	var user []*patternRule
	var cancelled []*patternRule
	for _, p := range m.patterns {
		kept := user[:0]
		for _, q := range user {
			if !(same(q.targets, p.targets) && same(q.prereqs, p.prereqs)) {
				kept = append(kept, q)
			}
		}
		user = kept
		if p.recipe == nil {
			cancelled = append(cancelled, p)
			continue
		}
		user = append(user, p)
	}
	isCancelled := func(p *patternRule) bool {
		for _, c := range cancelled {
			if same(c.targets, p.targets) && same(c.prereqs, p.prereqs) {
				return true
			}
		}
		for _, u := range user {
			if same(u.targets, p.targets) && same(u.prereqs, p.prereqs) {
				return true // replaced
			}
		}
		return false
	}
	rules := user
	// Suffix rules in .SUFFIXES order (source suffix, then target suffix);
	// the makefile's own replace built-in ones.
	names := func() []string {
		var out []string
		for _, s := range m.suffixes {
			out = append(out, s)
			for _, t := range m.suffixes {
				out = append(out, s+t)
			}
		}
		return out
	}()
	seen := map[string]bool{}
	for _, builtin := range []bool{false, true} {
		for _, n := range names {
			if seen[n] {
				continue
			}
			var rec *recipe
			if builtin {
				lines, ok := builtinSuffixRules[n]
				if !ok {
					continue
				}
				rec = &recipe{}
				for _, l := range lines {
					rec.lines = append(rec.lines, recipeLine{text: l})
				}
			} else {
				r, ok := m.suffixRules[n]
				if !ok {
					continue
				}
				rec = r
			}
			p := m.suffixPattern(n, rec, builtin)
			if p == nil || isCancelled(p) {
				continue
			}
			if !builtin {
				p.pos = m.suffixPos[n]
			}
			seen[n] = true
			rules = append(rules, p)
		}
	}
	m.ruleCache = rules
	return rules
}

// exist reports whether a file exists (relative to the make directory).
func (m *Makefile) exist(name string) bool {
	if e, ok := m.exists[name]; ok {
		return e
	}
	p := name
	if !filepath.IsAbs(p) {
		p = filepath.Join(m.opts.Dir, p)
	}
	_, err := os.Stat(p)
	m.exists[name] = err == nil
	return err == nil
}

// match is an applicable pattern rule for a target.
type match struct {
	rule      *patternRule
	stem      string   // $*
	targets   []string // all targets the rule makes (multi-target patterns)
	prereqs   []string
	orderOnly []string
}

// instantiate applies rule p, through its target pattern tp, to file t.
func instantiate(p *patternRule, tp, t string) (*match, bool) {
	dir, name := "", t
	if !strings.Contains(tp, "/") {
		if i := strings.LastIndexByte(t, '/'); i >= 0 {
			dir, name = t[:i+1], t[i+1:]
		}
	}
	s, ok := matchPattern(tp, name)
	if !ok {
		return nil, false
	}
	mt := &match{rule: p, stem: dir + s}
	subst := func(pats []string) []string {
		var out []string
		for _, pp := range pats {
			if strings.Contains(pp, "%") {
				x := substPattern(pp, s)
				if !strings.Contains(pp, "/") {
					x = dir + x
				}
				out = append(out, x)
			} else {
				out = append(out, pp)
			}
		}
		return out
	}
	mt.prereqs = subst(p.prereqs)
	mt.orderOnly = subst(p.orderOnly)
	for _, tt := range p.targets {
		x := substPattern(tt, s)
		if !strings.Contains(tt, "/") {
			x = dir + x
		}
		mt.targets = append(mt.targets, x)
	}
	return mt, true
}

// search finds the implicit rule for t (GNU make's implicit rule search).
// chained is true for a prerequisite of another implicit rule.
func (m *Makefile) search(t string, chained bool, busy map[*patternRule]bool) *match {
	key := t
	if chained {
		key = "\x00" + t
	}
	if r, ok := m.searchCache[key]; ok {
		return r
	}
	type cand struct {
		mt  *match
		any bool // match-anything
	}
	var cands []cand
	specific := false
	for _, p := range m.allPatterns() {
		if busy[p] {
			continue // a rule may appear only once in a chain
		}
		for _, tp := range p.targets {
			mt, ok := instantiate(p, tp, t)
			if !ok {
				continue
			}
			any := tp == "%"
			if !any {
				specific = true
			}
			cands = append(cands, cand{mt, any})
			break
		}
	}
	var list []*match
	for _, c := range cands {
		if c.any && (specific || chained) {
			continue
		}
		list = append(list, c.mt)
	}
	sort.SliceStable(list, func(i, j int) bool { return len(list[i].stem) < len(list[j].stem) })

	ought := func(p string) bool { return m.mentioned[p] || m.exist(p) }
	var found *match
	for _, mt := range list {
		ok := true
		for _, p := range append(mt.prereqs, mt.orderOnly...) {
			if !ought(p) {
				ok = false
				break
			}
		}
		if ok {
			found = mt
			break
		}
	}
	if found == nil {
		for _, mt := range list {
			nb := map[*patternRule]bool{mt.rule: true}
			for r := range busy {
				nb[r] = true
			}
			ok := true
			for _, p := range append(mt.prereqs, mt.orderOnly...) {
				if ought(p) {
					continue
				}
				if m.search(p, true, nb) == nil {
					ok = false
					break
				}
			}
			if ok {
				found = mt
				break
			}
		}
	}
	if len(busy) == 0 {
		m.searchCache[key] = found
	}
	return found
}

// patternVarsFor returns the pattern-specific variables that apply to t,
// in definition order.
func (m *Makefile) patternVarsFor(t string) []tvar {
	var out []tvar
	for _, pv := range m.patternVars {
		if _, ok := matchPattern(pv.pattern, t); ok {
			out = append(out, pv.tv)
		} else if !strings.Contains(pv.pattern, "/") {
			if _, ok := matchPattern(pv.pattern, path.Base(t)); ok {
				out = append(out, pv.tv)
			}
		}
	}
	return out
}
