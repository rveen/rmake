// Package glob implements path globbing with `**` (README §5.4).
//
// Patterns use '/' separators. Within a segment, `*`, `?` and `[...]` have
// their path.Match meaning. A segment that is exactly `**` matches zero or
// more segments.
package glob

import (
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
)

// HasMeta reports whether s contains glob metacharacters.
func HasMeta(s string) bool { return strings.ContainsAny(s, "*?[") }

// Valid reports whether pattern is syntactically valid.
func Valid(pattern string) bool {
	for _, seg := range strings.Split(pattern, "/") {
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, ""); err != nil {
			return false
		}
	}
	return true
}

// Match reports whether name matches pattern. An invalid pattern matches
// nothing.
func Match(pattern, name string) bool {
	return matchSegs(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegs(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// Collapse repeated ** and try every split point.
			for len(pat) > 0 && pat[0] == "**" {
				pat = pat[1:]
			}
			if len(pat) == 0 {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if matchSegs(pat, name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		ok, err := path.Match(pat[0], name[0])
		if err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// Disk expands pattern against files below root. Results are paths relative
// to root, sorted, and only name non-directories. Directories whose names
// start with '.' are not entered by wildcard segments, and paths for which
// skip returns true (e.g. the build directory) are not entered at all.
// An invalid pattern returns nil.
func Disk(root, pattern string, skip func(rel string) bool) []string {
	if !Valid(pattern) {
		return nil
	}
	if path.IsAbs(pattern) {
		out := Disk("/", strings.TrimLeft(pattern, "/"), nil)
		for i := range out {
			out[i] = "/" + out[i]
		}
		return out
	}
	if !HasMeta(pattern) {
		if fi, err := os.Stat(path.Join(root, pattern)); err == nil && !fi.IsDir() {
			return []string{path.Clean(pattern)}
		}
		return nil
	}
	g := &walker{root: root, skip: skip, seen: map[string]bool{}}
	segs := strings.Split(path.Clean(pattern), "/")
	g.walk("", segs)
	sort.Strings(g.out)
	return g.out
}

type walker struct {
	root string
	skip func(string) bool
	seen map[string]bool
	out  []string
}

func join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func (g *walker) abs(rel string) string {
	if rel == "" {
		return g.root
	}
	return path.Join(g.root, rel)
}

func (g *walker) emit(rel string) {
	if !g.seen[rel] {
		g.seen[rel] = true
		g.out = append(g.out, rel)
	}
}

// walk matches segs against the directory dir (relative to root).
func (g *walker) walk(dir string, segs []string) {
	seg := segs[0]
	last := len(segs) == 1
	switch {
	case seg == "**":
		if last {
			g.walkAll(dir)
			return
		}
		g.walk(dir, segs[1:]) // zero segments
		for _, e := range g.readDir(dir) {
			if e.IsDir() && !hidden(e.Name()) {
				g.walk(join(dir, e.Name()), segs) // one or more
			}
		}
	case !HasMeta(seg):
		p := join(dir, seg)
		if seg == ".." || seg == "." {
			p = path.Clean(p)
		}
		if g.skip != nil && g.skip(p) {
			return
		}
		fi, err := os.Stat(g.abs(p))
		if err != nil {
			return
		}
		if last {
			if !fi.IsDir() {
				g.emit(p)
			}
		} else if fi.IsDir() {
			g.walk(p, segs[1:])
		}
	default:
		for _, e := range g.readDir(dir) {
			name := e.Name()
			if hidden(name) && !strings.HasPrefix(seg, ".") {
				continue
			}
			if ok, _ := path.Match(seg, name); !ok {
				continue
			}
			p := join(dir, name)
			isDir := g.isDir(p, e)
			if last {
				if !isDir {
					g.emit(p)
				}
			} else if isDir {
				g.walk(p, segs[1:])
			}
		}
	}
}

// walkAll emits every file below dir (for a trailing **).
func (g *walker) walkAll(dir string) {
	for _, e := range g.readDir(dir) {
		if hidden(e.Name()) {
			continue
		}
		p := join(dir, e.Name())
		if e.IsDir() { // symlinked directories are not followed by **
			g.walkAll(p)
		} else {
			g.emit(p)
		}
	}
}

func (g *walker) readDir(dir string) []fs.DirEntry {
	if g.skip != nil && dir != "" && g.skip(dir) {
		return nil
	}
	es, err := os.ReadDir(g.abs(dir))
	if err != nil {
		return nil
	}
	var out []fs.DirEntry
	for _, e := range es {
		if g.skip != nil && e.IsDir() && g.skip(join(dir, e.Name())) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// isDir follows symlinks.
func (g *walker) isDir(rel string, e fs.DirEntry) bool {
	if e.Type()&fs.ModeSymlink == 0 {
		return e.IsDir()
	}
	fi, err := os.Stat(g.abs(rel))
	return err == nil && fi.IsDir()
}

func hidden(name string) bool { return strings.HasPrefix(name, ".") }
