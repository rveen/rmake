package syntax

import (
	"strings"
	"unicode/utf8"
)

// line is one physical source line, split into words.
type line struct {
	pos     Pos // position of the first word (or of the comment)
	indent  string
	words   []Word
	comment string // trailing or full-line comment, including '#'
	blank   bool   // nothing but whitespace
}

func (l *line) code() bool { return len(l.words) > 0 }

// lexLines splits src into lines of words.
func lexLines(file string, src []byte) ([]line, error) {
	if !utf8.Valid(src) {
		return nil, Errorf(Pos{File: file, Line: 1, Col: 1}, "file is not valid UTF-8")
	}
	text := strings.ReplaceAll(string(src), "\r\n", "\n")
	raw := strings.Split(text, "\n")
	if len(raw) > 0 && raw[len(raw)-1] == "" {
		raw = raw[:len(raw)-1]
	}
	lines := make([]line, 0, len(raw))
	for i, s := range raw {
		l, err := lexLine(file, i+1, s)
		if err != nil {
			return nil, err
		}
		lines = append(lines, l)
	}
	return lines, nil
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\v' || c == '\f' }

func lexLine(file string, n int, s string) (line, error) {
	var l line
	i := 0
	for i < len(s) && isSpace(s[i]) {
		i++
	}
	l.indent = s[:i]
	l.pos = Pos{File: file, Line: n, Col: i + 1}
	for {
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		if i >= len(s) {
			break
		}
		if s[i] == '#' {
			l.comment = strings.TrimRight(s[i:], " \t\r\v\f")
			break
		}
		w, end, err := lexWord(file, n, s, i)
		if err != nil {
			return l, err
		}
		l.words = append(l.words, w)
		i = end
	}
	l.blank = len(l.words) == 0 && l.comment == ""
	return l, nil
}

// lexWord lexes the word starting at s[i]. It returns the word and the index
// after it.
func lexWord(file string, n int, s string, i int) (Word, int, error) {
	start := i
	w := Word{Pos: Pos{File: file, Line: n, Col: i + 1}}
	quoted := false
	var quotePos Pos
	lit := func(text string, pos Pos) {
		if k := len(w.Parts) - 1; k >= 0 && w.Parts[k].Var == "" && w.Parts[k].Quoted == quoted {
			w.Parts[k].Lit += text
			return
		}
		w.Parts = append(w.Parts, Part{Pos: pos, Lit: text, Quoted: quoted})
	}
	for i < len(s) {
		c := s[i]
		pos := Pos{File: file, Line: n, Col: i + 1}
		if !quoted && isSpace(c) {
			break
		}
		switch {
		case c == '"':
			quoted = !quoted
			if quoted {
				quotePos = pos
				lit("", pos) // a "" word has one empty literal part
			}
			i++
		case quoted && c == '\\' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\'):
			lit(s[i+1:i+2], pos)
			i += 2
		case c == '$':
			p, end, err := lexRef(file, n, s, i)
			if err != nil {
				return w, 0, err
			}
			if p.Var == "" {
				lit(p.Lit, pos)
			} else {
				p.Quoted = quoted
				w.Parts = append(w.Parts, p)
			}
			i = end
		default:
			lit(s[i:i+1], pos)
			i++
		}
	}
	if quoted {
		return w, 0, Errorf(quotePos, "unterminated quote")
	}
	w.Raw = s[start:i]
	return w, i, nil
}

func isNameStart(c byte) bool {
	return c == '_' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

func isNameChar(c byte) bool { return isNameStart(c) || ('0' <= c && c <= '9') }

// IsName reports whether s is a valid variable, parameter or task name.
func IsName(s string) bool {
	if s == "" || !isNameStart(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isNameChar(s[i]) {
			return false
		}
	}
	return true
}

// lexRef lexes a reference starting at s[i] == '$'. `$$` yields a literal
// part.
func lexRef(file string, n int, s string, i int) (Part, int, error) {
	pos := Pos{File: file, Line: n, Col: i + 1}
	if i+1 >= len(s) {
		return Part{}, 0, Errorf(pos, "'$' at end of line (use $$ for a literal $)")
	}
	c := s[i+1]
	switch {
	case c == '$':
		return Part{Pos: pos, Lit: "$"}, i + 2, nil
	case isNameStart(c):
		j := i + 1
		for j < len(s) && isNameChar(s[j]) {
			j++
		}
		return Part{Pos: pos, Var: s[i+1 : j]}, j, nil
	case c == '{':
		j := i + 2
		for j < len(s) && isNameChar(s[j]) {
			j++
		}
		name := s[i+2 : j]
		if !IsName(name) {
			return Part{}, 0, Errorf(pos, "invalid variable name in ${...}")
		}
		p := Part{Pos: pos, Var: name}
		if j < len(s) && s[j] == '}' {
			return p, j + 1, nil
		}
		if j >= len(s) || s[j] != ':' {
			return Part{}, 0, Errorf(pos, "expected '}' or ':' in ${...}")
		}
		pat, j, err := lexSubstParts(file, n, s, j+1, '=')
		if err != nil {
			return Part{}, 0, err
		}
		repl, j, err := lexSubstParts(file, n, s, j+1, '}')
		if err != nil {
			return Part{}, 0, err
		}
		p.Subst = &Subst{Pat: pat, Repl: repl}
		return p, j + 1, nil
	default:
		return Part{}, 0, Errorf(pos, "invalid character after '$' (use $$ for a literal $)")
	}
}

// lexSubstParts lexes the pattern or replacement of ${x:pat=repl} up to the
// terminator, returning the index of the terminator.
func lexSubstParts(file string, n int, s string, i int, term byte) ([]Part, int, error) {
	var parts []Part
	for i < len(s) {
		c := s[i]
		pos := Pos{File: file, Line: n, Col: i + 1}
		switch {
		case c == term:
			return parts, i, nil
		case isSpace(c) || c == '"' || c == '}' || (term == '=' && c == '='):
			return nil, 0, Errorf(pos, "unexpected %q in ${...}", c)
		case c == '$':
			p, end, err := lexRef(file, n, s, i)
			if err != nil {
				return nil, 0, err
			}
			parts = append(parts, p)
			i = end
		default:
			if k := len(parts) - 1; k >= 0 && parts[k].Var == "" {
				parts[k].Lit += string(c)
			} else {
				parts = append(parts, Part{Pos: pos, Lit: string(c)})
			}
			i++
		}
	}
	return nil, 0, Errorf(Pos{File: file, Line: n, Col: i + 1}, "unterminated ${...}")
}
