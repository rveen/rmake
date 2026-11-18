package syntax

import (
	"strings"
)

// Parse parses a build file. name is used in positions.
func Parse(name string, src []byte) (*File, error) {
	lines, err := lexLines(name, src)
	if err != nil {
		return nil, err
	}
	p := &parser{lines: lines}
	f := &File{Name: name}
	stmts, term, trail, err := p.stmts(nil)
	if err != nil {
		return nil, err
	}
	if term != nil {
		return nil, Errorf(term.pos, "%s without if", term.words[0].Raw)
	}
	f.Stmts = stmts
	f.Trailing = trail
	return f, nil
}

type parser struct {
	lines []line
	i     int

	// comments and blank flag seen before the last else/end line
	pendingComments []string
	pendingBlank    bool
}

// trivia consumes blank and comment-only lines and returns the comments and
// whether a blank line was seen.
func (p *parser) trivia() (comments []string, blank bool) {
	for p.i < len(p.lines) {
		l := &p.lines[p.i]
		switch {
		case l.blank:
			blank = true
		case !l.code():
			comments = append(comments, l.comment)
		default:
			return
		}
		p.i++
	}
	return
}

func node(l *line, comments []string, blank bool) Node {
	return Node{Pos: l.pos, Comments: comments, LineComment: l.comment, BlankBefore: blank}
}

// stmts parses statements up to EOF or, inside an if, up to else/end. It
// returns the terminating line (nil at EOF) and, at EOF, trailing comments.
// Comments before a terminator are left in p.pendingComments. ifLine is the
// enclosing if, or nil at top level.
func (p *parser) stmts(ifLine *line) ([]Stmt, *line, []string, error) {
	var out []Stmt
	for {
		comments, blank := p.trivia()
		if p.i >= len(p.lines) {
			if ifLine != nil {
				return nil, nil, nil, Errorf(ifLine.pos, "if without end")
			}
			return out, nil, comments, nil
		}
		l := &p.lines[p.i]
		kw := l.words[0].Raw
		switch kw {
		case "else", "end":
			if len(l.words) > 1 {
				return nil, nil, nil, Errorf(l.words[1].Pos, "unexpected %q after %s", l.words[1].Raw, kw)
			}
			p.pendingComments, p.pendingBlank = comments, blank
			p.i++
			return out, l, nil, nil
		}
		n := node(l, comments, blank)
		p.i++
		var s Stmt
		var err error
		switch kw {
		case "var":
			s, err = parseVar(n, l.words[1:], true)
		case "include":
			s, err = p.parseInclude(n, l)
		case "default":
			if len(l.words) < 2 {
				return nil, nil, nil, Errorf(l.pos, "default needs at least one target")
			}
			s = &Default{Node: n, Targets: l.words[1:]}
		case "rule":
			s, err = p.parseRule(n, l)
		case "task":
			s, err = p.parseTask(n, l)
		case "if":
			s, err = p.parseIf(n, l)
		default:
			return nil, nil, nil, Errorf(l.pos, "unknown statement %q", kw)
		}
		if err != nil {
			return nil, nil, nil, err
		}
		out = append(out, s)
	}
}

func parseVar(n Node, ws []Word, top bool) (*Var, error) {
	if len(ws) == 0 {
		return nil, Errorf(n.Pos, "var needs a name")
	}
	name := ws[0].Raw
	if !IsName(name) {
		return nil, Errorf(ws[0].Pos, "invalid variable name %q", name)
	}
	if len(ws) < 2 {
		return nil, Errorf(ws[0].Pos, "expected '=', '+=' or '?=' after %s", name)
	}
	op := ws[1].Raw
	switch op {
	case "=", "+=":
	case "?=":
		if !top {
			return nil, Errorf(ws[1].Pos, "'?=' is not allowed in a rule or task")
		}
	default:
		return nil, Errorf(ws[1].Pos, "expected '=', '+=' or '?=' after %s", name)
	}
	v := &Var{Node: n, Name: name, Op: op, Values: ws[2:]}
	if len(v.Values) > 0 && v.Values[0].Raw == "glob" {
		v.Glob = true
		v.Values = v.Values[1:]
		if len(v.Values) == 0 {
			return nil, Errorf(ws[2].Pos, "glob needs at least one pattern")
		}
	}
	return v, nil
}

func (p *parser) parseInclude(n Node, l *line) (Stmt, error) {
	if len(l.words) != 2 {
		return nil, Errorf(l.pos, "include takes exactly one path")
	}
	return &Include{Node: n, Path: l.words[1]}, nil
}

func (p *parser) parseRule(n Node, l *line) (Stmt, error) {
	r := &Rule{Node: n}
	ws := l.words[1:]
	from := -1
	for i, w := range ws {
		if w.Raw == "from" {
			from = i
			break
		}
	}
	if from < 0 {
		return nil, Errorf(l.pos, "rule needs 'from' (use 'from' with nothing after it for no inputs)")
	}
	if from == 0 {
		return nil, Errorf(l.pos, "rule needs at least one output")
	}
	r.Outs = ws[:from]
	for _, w := range r.Outs {
		if w.Raw == "|" {
			return nil, Errorf(w.Pos, "'|' is only allowed after 'from'")
		}
	}
	for _, w := range ws[from+1:] {
		if w.Raw == "|" {
			if r.Pipe {
				return nil, Errorf(w.Pos, "more than one '|'")
			}
			r.Pipe = true
			continue
		}
		if r.Pipe {
			r.Implicit = append(r.Implicit, w)
		} else {
			r.Ins = append(r.Ins, w)
		}
	}
	attrs, err := p.attrs(l, ruleAttrs)
	r.Attrs = attrs
	return r, err
}

func (p *parser) parseTask(n Node, l *line) (Stmt, error) {
	t := &Task{Node: n}
	ws := l.words[1:]
	if len(ws) == 0 {
		return nil, Errorf(l.pos, "task needs a name")
	}
	// The name and parameter list may be split over several words:
	// release(a, b) lexes as "release(a," "b)".
	head := ws[0].Raw
	k := 1
	if !strings.Contains(head, "(") && len(ws) > 1 && strings.HasPrefix(ws[1].Raw, "(") {
		head += " " + ws[1].Raw
		k++
	}
	if strings.Contains(head, "(") {
		for !strings.Contains(head, ")") && k < len(ws) {
			head += " " + ws[k].Raw
			k++
		}
		open := strings.Index(head, "(")
		closeIdx := strings.Index(head, ")")
		if closeIdx < 0 || closeIdx != len(head)-1 {
			return nil, Errorf(ws[0].Pos, "malformed parameter list")
		}
		t.Name = strings.TrimSpace(head[:open])
		params := strings.TrimSpace(head[open+1 : closeIdx])
		if params != "" {
			for _, s := range strings.Split(params, ",") {
				s = strings.TrimSpace(s)
				if !IsName(s) {
					return nil, Errorf(ws[0].Pos, "invalid parameter name %q", s)
				}
				t.Params = append(t.Params, s)
			}
		}
	} else {
		t.Name = head
	}
	if !IsName(t.Name) {
		return nil, Errorf(ws[0].Pos, "invalid task name %q", t.Name)
	}
	rest := ws[k:]
	if len(rest) > 0 {
		if rest[0].Raw != "needs" {
			return nil, Errorf(rest[0].Pos, "expected 'needs', found %q", rest[0].Raw)
		}
		if len(rest) == 1 {
			return nil, Errorf(rest[0].Pos, "needs requires at least one target")
		}
		t.Needs = rest[1:]
	}
	attrs, err := p.attrs(l, taskAttrs)
	t.Attrs = attrs
	return t, err
}

var (
	ruleAttrs = map[string]bool{"var": true, "run": true, "sh": true, "deps": true, "depfile": true, "pass": true, "always": true, "desc": true}
	taskAttrs = map[string]bool{"var": true, "run": true, "sh": true, "pass": true, "desc": true}
)

// attrs parses the attribute lines that follow header line h: lines indented
// deeper than h.
func (p *parser) attrs(h *line, allowed map[string]bool) ([]*Attr, error) {
	var out []*Attr
	for {
		save := p.i
		comments, blank := p.trivia()
		if p.i >= len(p.lines) || len(p.lines[p.i].indent) <= len(h.indent) {
			p.i = save
			return out, nil
		}
		l := &p.lines[p.i]
		p.i++
		a := &Attr{Node: node(l, comments, blank), Kind: l.words[0].Raw, Args: l.words[1:]}
		if !allowed[a.Kind] {
			what := "rule"
			if h.words[0].Raw == "task" {
				what = "task"
			}
			return nil, Errorf(l.pos, "unknown %s attribute %q", what, a.Kind)
		}
		if err := checkAttr(a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
}

func checkAttr(a *Attr) error {
	n := len(a.Args)
	switch a.Kind {
	case "var":
		v, err := parseVar(a.Node, a.Args, false)
		if err != nil {
			return err
		}
		a.Var = v
	case "run", "pass":
		if n == 0 {
			return Errorf(a.Pos, "%s needs at least one argument", a.Kind)
		}
		if a.Kind == "pass" {
			for _, w := range a.Args {
				if !IsName(w.Raw) {
					return Errorf(w.Pos, "invalid variable name %q", w.Raw)
				}
			}
		}
	case "sh", "desc", "depfile":
		if n != 1 {
			return Errorf(a.Pos, "%s takes exactly one word (quote it)", a.Kind)
		}
	case "deps":
		if n != 1 || (a.Args[0].Raw != "make" && a.Args[0].Raw != "gcc") {
			return Errorf(a.Pos, "deps takes 'make' or 'gcc'")
		}
	case "always":
		if n != 0 {
			return Errorf(a.Args[0].Pos, "always takes no arguments")
		}
	}
	return nil
}

func (p *parser) parseIf(n Node, l *line) (Stmt, error) {
	s := &If{Node: n}
	ws := l.words[1:]
	switch {
	case len(ws) == 2 && ws[0].Raw == "defined":
		if !IsName(ws[1].Raw) {
			return nil, Errorf(ws[1].Pos, "invalid variable name %q", ws[1].Raw)
		}
		s.Op, s.Defined = "defined", ws[1].Raw
	case len(ws) == 3 && (ws[1].Raw == "==" || ws[1].Raw == "!="):
		s.Left, s.Op, s.Right = ws[0], ws[1].Raw, ws[2]
	default:
		return nil, Errorf(l.pos, "expected 'if A == B', 'if A != B' or 'if defined NAME'")
	}
	body, term, _, err := p.stmts(l)
	if err != nil {
		return nil, err
	}
	s.Then = body
	if term.words[0].Raw == "else" {
		s.HasElse = true
		s.ElseN = node(term, p.pendingComments, p.pendingBlank)
		body, term, _, err = p.stmts(l)
		if err != nil {
			return nil, err
		}
		if term.words[0].Raw != "end" {
			return nil, Errorf(term.pos, "expected end, found else")
		}
		s.Else = body
	}
	s.EndN = node(term, p.pendingComments, p.pendingBlank)
	return s, nil
}
