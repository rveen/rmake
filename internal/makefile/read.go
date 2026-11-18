package makefile

import (
	"strings"

	"github.com/rveen/rmake/internal/syntax"
)

// source is one makefile being read, as physical lines.
type source struct {
	file  string
	lines []string
	next  int // index of the next physical line
}

func newSource(file string, text string) *source {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return &source{file: file, lines: lines}
}

func (s *source) eof() bool { return s.next >= len(s.lines) }

func (s *source) pos(line int) syntax.Pos {
	return syntax.Pos{File: s.file, Line: line + 1, Col: 1}
}

// continued reports whether a physical line ends with an odd number of
// backslashes (a continuation).
func continued(l string) bool {
	n := 0
	for i := len(l) - 1; i >= 0 && l[i] == '\\'; i-- {
		n++
	}
	return n%2 == 1
}

// logical reads the next logical (non-recipe) line: continuation lines are
// joined, each backslash-newline with the whitespace around it becoming one
// space. It returns the text and the position of its first line.
func (s *source) logical() (string, syntax.Pos) {
	pos := s.pos(s.next)
	l := s.lines[s.next]
	s.next++
	if !continued(l) {
		return l, pos
	}
	var b strings.Builder
	for {
		if continued(l) && !s.eof() {
			b.WriteString(strings.TrimRight(l[:len(l)-1], " \t"))
			b.WriteByte(' ')
			l = strings.TrimLeft(s.lines[s.next], " \t")
			s.next++
			continue
		}
		if continued(l) { // continuation at end of file
			l = l[:len(l)-1]
		}
		b.WriteString(l)
		return b.String(), pos
	}
}

// recipe reads a recipe line (the current line starts with the recipe
// prefix): the prefix is removed, continuations are kept as
// backslash-newline, and a recipe prefix at the start of a continuation line
// is removed (as make does).
func (s *source) recipe(prefix byte) (string, syntax.Pos) {
	pos := s.pos(s.next)
	l := s.lines[s.next][1:]
	s.next++
	var b strings.Builder
	for {
		b.WriteString(l)
		if !continued(l) || s.eof() {
			return b.String(), pos
		}
		b.WriteByte('\n')
		l = s.lines[s.next]
		s.next++
		if len(l) > 0 && l[0] == prefix {
			l = l[1:]
		}
	}
}

// stripComment removes a comment (an unescaped '#') from a non-recipe line
// and turns "\#" into "#".
func stripComment(l string) string {
	if !strings.Contains(l, "#") {
		return l
	}
	var b strings.Builder
	for i := 0; i < len(l); i++ {
		c := l[i]
		if c == '\\' && i+1 < len(l) && l[i+1] == '#' {
			b.WriteByte('#')
			i++
			continue
		}
		if c == '#' {
			break
		}
		b.WriteByte(c)
	}
	return b.String()
}

// firstWord splits off the first whitespace-separated word of s.
func firstWord(s string) (word, rest string) {
	s = strings.TrimLeft(s, " \t")
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimLeft(s[i:], " \t")
}

// findTop returns the index of the first byte of s, outside $(...) and
// ${...} references, for which match is true, or -1.
func findTop(s string, match func(s string, i int) bool) int {
	depth := 0
	var stack []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '$' && i+1 < len(s) && (s[i+1] == '(' || s[i+1] == '{'):
			stack = append(stack, closer(s[i+1]))
			depth++
			i++
			continue
		case c == '$' && i+1 < len(s):
			i++ // $$ or $x
			continue
		case depth > 0 && (c == '(' || c == '{'):
			stack = append(stack, closer(c))
			depth++
			continue
		case depth > 0 && c == stack[len(stack)-1]:
			stack = stack[:len(stack)-1]
			depth--
			continue
		}
		if depth == 0 && match(s, i) {
			return i
		}
	}
	return -1
}

func closer(open byte) byte {
	if open == '(' {
		return ')'
	}
	return '}'
}

// splitWords splits on whitespace.
func splitWords(s string) []string { return strings.Fields(s) }
