package eval

import (
	"strings"

	"github.com/rveen/rmake/internal/syntax"
)

// scope is a chain of variable maps. An included file and a rule get a child
// scope; their definitions do not leak to the parent.
type scope struct {
	vars   map[string][]string
	parent *scope
}

func newScope(parent *scope) *scope {
	return &scope{vars: map[string][]string{}, parent: parent}
}

func (s *scope) lookup(name string) ([]string, bool) {
	for ; s != nil; s = s.parent {
		if v, ok := s.vars[name]; ok {
			return v, true
		}
	}
	return nil, false
}

// snapshot returns a flat copy of everything visible from s, so later
// definitions do not affect deferred expansion (tasks).
func (s *scope) snapshot() *scope {
	out := newScope(nil)
	var chain []*scope
	for c := s; c != nil; c = c.parent {
		chain = append(chain, c)
	}
	for i := len(chain) - 1; i >= 0; i-- {
		for k, v := range chain[i].vars {
			out.vars[k] = v
		}
	}
	return out
}

// expandWords expands each word and concatenates the results.
func expandWords(ws []syntax.Word, sc *scope) ([]string, error) {
	var out []string
	for _, w := range ws {
		v, err := expandWord(w, sc)
		if err != nil {
			return nil, err
		}
		out = append(out, v...)
	}
	return out, nil
}

// expandJoined expands words and joins the result with single spaces.
func expandJoined(ws []syntax.Word, sc *scope) (string, error) {
	v, err := expandWords(ws, sc)
	return strings.Join(v, " "), err
}

// expandWord expands one word to a list (README §5.3): standalone list
// variables yield several words, concatenation distributes, concatenation
// with the empty list yields the empty list, and inside quotes a list is
// joined with spaces.
func expandWord(w syntax.Word, sc *scope) ([]string, error) {
	return expandParts(w.Parts, sc)
}

func expandParts(parts []syntax.Part, sc *scope) ([]string, error) {
	acc := []string{""}
	empty := false
	for _, p := range parts {
		var vals []string
		if p.Var == "" {
			vals = []string{p.Lit}
		} else {
			v, ok := sc.lookup(p.Var)
			if !ok {
				return nil, undefined(p)
			}
			if p.Subst != nil {
				pat, err := expandSingle(p.Subst.Pat, sc, p.Pos)
				if err != nil {
					return nil, err
				}
				repl, err := expandSingle(p.Subst.Repl, sc, p.Pos)
				if err != nil {
					return nil, err
				}
				v = substitute(v, pat, repl)
			}
			if p.Quoted {
				vals = []string{strings.Join(v, " ")}
			} else {
				vals = v
			}
		}
		if empty {
			continue // keep evaluating to report errors
		}
		switch {
		case len(vals) == 0:
			empty = true
		case len(vals) == 1:
			for i := range acc {
				acc[i] += vals[0]
			}
		case len(acc) == 1:
			pre := acc[0]
			acc = make([]string, len(vals))
			for i, v := range vals {
				acc[i] = pre + v
			}
		default:
			return nil, syntax.Errorf(p.Pos, "cannot concatenate a list of %d words with a list of %d words", len(acc), len(vals))
		}
	}
	if empty {
		return nil, nil
	}
	return acc, nil
}

func undefined(p syntax.Part) error {
	switch p.Var {
	case "in", "out":
		return syntax.Errorf(p.Pos, "$%s is only defined inside rules", p.Var)
	case "stem":
		return syntax.Errorf(p.Pos, "$stem is only defined inside pattern rules")
	}
	return syntax.Errorf(p.Pos, "undefined variable %s", p.Var)
}

// expandSingle expands the pattern or replacement of a substitution
// reference, which must be at most one word.
func expandSingle(parts []syntax.Part, sc *scope, pos syntax.Pos) (string, error) {
	v, err := expandParts(parts, sc)
	if err != nil {
		return "", err
	}
	if len(v) > 1 {
		return "", syntax.Errorf(pos, "substitution pattern expands to %d words, want one", len(v))
	}
	if len(v) == 0 {
		return "", nil
	}
	return v[0], nil
}

// substitute applies ${x:pat=repl} to each word. With a '%' in pat, the '%'
// matches any string and the first '%' in repl is replaced by it; without,
// pat is matched as a suffix. Non-matching words are kept unchanged.
func substitute(vals []string, pat, repl string) []string {
	out := make([]string, len(vals))
	if i := strings.IndexByte(pat, '%'); i >= 0 {
		pre, suf := pat[:i], pat[i+1:]
		for k, v := range vals {
			if len(v) >= len(pre)+len(suf) && strings.HasPrefix(v, pre) && strings.HasSuffix(v, suf) {
				out[k] = strings.Replace(repl, "%", v[len(pre):len(v)-len(suf)], 1)
			} else {
				out[k] = v
			}
		}
		return out
	}
	for k, v := range vals {
		if pat != "" && strings.HasSuffix(v, pat) {
			out[k] = v[:len(v)-len(pat)] + repl
		} else {
			out[k] = v
		}
	}
	return out
}
