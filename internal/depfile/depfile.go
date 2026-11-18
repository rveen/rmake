// Package depfile parses make-format dependency files (README §4.4), as
// written by gcc/clang -MD -MF, rustc --emit=dep-info and many generators.
package depfile

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// Parse returns the union of the prerequisites of all rules in data, in
// order of first appearance and without duplicates. Targets are dropped.
//
// Syntax: "targets: prerequisites" lines; backslash-newline continues a line;
// "\ " is a space and "\#" a '#' inside a name; "$$" is '$'; any other
// backslash is literal; a '#' at the start of a word starts a comment. The
// colon that ends the targets must be followed by white space or the end of
// the line (so "c:\x" is a name). Lines without such a colon must be empty.
func Parse(data []byte) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	s := string(data)
	line := 1
	inTargets := true // before the colon of the current rule
	sawTarget := false
	var word strings.Builder
	inWord := false

	flush := func() error {
		if !inWord {
			return nil
		}
		w := word.String()
		word.Reset()
		inWord = false
		if inTargets {
			sawTarget = true
			return nil
		}
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
		return nil
	}
	endLine := func() error {
		if err := flush(); err != nil {
			return err
		}
		if inTargets && sawTarget {
			return fmt.Errorf("depfile line %d: missing ':'", line)
		}
		inTargets, sawTarget = true, false
		line++
		return nil
	}

	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && (s[i+1] == '\n' || s[i+1] == '\r' && i+2 < len(s) && s[i+2] == '\n'):
			// Continuation: acts as a separator.
			if err := flush(); err != nil {
				return nil, err
			}
			if s[i+1] == '\r' {
				i++
			}
			i++
			line++
		case c == '\\' && i+1 < len(s) && (s[i+1] == ' ' || s[i+1] == '#'):
			word.WriteByte(s[i+1])
			inWord = true
			i++
		case c == '$' && i+1 < len(s) && s[i+1] == '$':
			word.WriteByte('$')
			inWord = true
			i++
		case c == '#' && !inWord:
			for i < len(s) && s[i] != '\n' {
				i++
			}
			i-- // the newline ends the line
		case c == '\n':
			if err := endLine(); err != nil {
				return nil, err
			}
		case c == ' ' || c == '\t' || c == '\r':
			if err := flush(); err != nil {
				return nil, err
			}
		case c == ':' && inTargets && (i+1 == len(s) || isSpace(s[i+1])):
			// The colon ends the targets (a colon followed by anything else,
			// as in c:\x, is part of a name).
			if err := flush(); err != nil {
				return nil, err
			}
			if !sawTarget {
				return nil, fmt.Errorf("depfile line %d: no target before ':'", line)
			}
			inTargets = false
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	if err := endLine(); err != nil {
		return nil, err
	}
	return out, nil
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\\' }

// Normalize cleans p and makes it relative to root (an absolute, clean
// directory) if it is absolute and inside root. Other absolute paths stay
// absolute; relative paths are relative to root already (commands run there).
func Normalize(p, root string) string {
	if !path.IsAbs(p) {
		return path.Clean(p)
	}
	p = path.Clean(p)
	if rel, err := filepath.Rel(root, p); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
		return filepath.ToSlash(rel)
	}
	return p
}
