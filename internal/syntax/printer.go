package syntax

import (
	"strings"
)

// Format prints f in canonical form (README §6): statements inside if are
// indented two spaces per level, attributes two spaces deeper than their
// header with keywords aligned, rule/task/if blocks separated by one blank
// line, and comments preserved.
func Format(f *File) []byte {
	pr := &printer{}
	pr.stmts(f.Stmts, 0)
	if len(f.Trailing) > 0 {
		if pr.b.Len() > 0 {
			pr.b.WriteByte('\n')
		}
		for _, c := range f.Trailing {
			pr.line(0, c)
		}
	}
	return []byte(pr.b.String())
}

type printer struct {
	b strings.Builder
}

func (pr *printer) line(depth int, text string) {
	pr.b.WriteString(strings.Repeat("  ", depth))
	pr.b.WriteString(text)
	pr.b.WriteByte('\n')
}

func isBlock(s Stmt) bool {
	switch s.(type) {
	case *Rule, *Task, *If:
		return true
	}
	return false
}

func (pr *printer) stmts(ss []Stmt, depth int) {
	for i, s := range ss {
		n := s.N()
		if i > 0 && (n.BlankBefore || isBlock(s) || isBlock(ss[i-1])) {
			pr.b.WriteByte('\n')
		}
		pr.comments(depth, n.Comments)
		switch s := s.(type) {
		case *Var:
			pr.line(depth, withComment(varText(s), n))
		case *Include:
			pr.line(depth, withComment("include "+s.Path.Raw, n))
		case *Default:
			pr.line(depth, withComment("default "+words(s.Targets), n))
		case *Rule:
			h := "rule " + words(s.Outs) + " from"
			if len(s.Ins) > 0 {
				h += " " + words(s.Ins)
			}
			if s.Pipe {
				h += " |"
				if len(s.Implicit) > 0 {
					h += " " + words(s.Implicit)
				}
			}
			pr.line(depth, withComment(h, n))
			pr.attrs(s.Attrs, depth+1)
		case *Task:
			h := "task " + s.Name
			if s.Params != nil {
				h += "(" + strings.Join(s.Params, ", ") + ")"
			}
			if len(s.Needs) > 0 {
				h += " needs " + words(s.Needs)
			}
			pr.line(depth, withComment(h, n))
			pr.attrs(s.Attrs, depth+1)
		case *If:
			h := "if defined " + s.Defined
			if s.Op != "defined" {
				h = "if " + s.Left.Raw + " " + s.Op + " " + s.Right.Raw
			}
			pr.line(depth, withComment(h, n))
			pr.stmts(s.Then, depth+1)
			if s.HasElse {
				pr.comments(depth, s.ElseN.Comments)
				pr.line(depth, withComment("else", &s.ElseN))
				pr.stmts(s.Else, depth+1)
			}
			pr.comments(depth, s.EndN.Comments)
			pr.line(depth, withComment("end", &s.EndN))
		}
	}
}

func (pr *printer) comments(depth int, cs []string) {
	for _, c := range cs {
		pr.line(depth, c)
	}
}

func (pr *printer) attrs(as []*Attr, depth int) {
	width := 0
	for _, a := range as {
		width = max(width, len(a.Kind))
	}
	for _, a := range as {
		pr.comments(depth, a.Comments)
		text := a.Kind
		switch {
		case a.Var != nil:
			text += strings.Repeat(" ", width-len(a.Kind)+1) + varText(a.Var)[len("var "):]
		case len(a.Args) > 0:
			text += strings.Repeat(" ", width-len(a.Kind)+1) + words(a.Args)
		}
		pr.line(depth, withComment(text, &a.Node))
	}
}

func varText(v *Var) string {
	s := "var " + v.Name + " " + v.Op
	if v.Glob {
		s += " glob"
	}
	if len(v.Values) > 0 {
		s += " " + words(v.Values)
	}
	return s
}

func words(ws []Word) string {
	parts := make([]string, len(ws))
	for i, w := range ws {
		parts[i] = w.Raw
	}
	return strings.Join(parts, " ")
}

func withComment(s string, n *Node) string {
	if n.LineComment != "" {
		return s + " " + n.LineComment
	}
	return s
}
