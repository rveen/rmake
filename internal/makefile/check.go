package makefile

// Comparing the commands of make -n and rmake -n (README §10.6), for
// -t import -check and the differential tests.

import (
	"regexp"
	"sort"
	"strings"
)

var spaces = regexp.MustCompile(`[ \t]+`)

func normCmd(s string) string {
	s = strings.ReplaceAll(s, "\\\n", "")
	return strings.TrimSpace(spaces.ReplaceAllString(s, " "))
}

// MakeCommands splits make -n output into commands (joining continued
// lines) and drops make's own messages.
func MakeCommands(out string) []string {
	var cmds []string
	var cur []string
	for _, l := range strings.Split(out, "\n") {
		cur = append(cur, l)
		if strings.HasSuffix(l, "\\") {
			continue
		}
		c := normCmd(strings.Join(cur, "\n"))
		cur = nil
		if c == "" || strings.HasPrefix(c, "make: ") || strings.HasPrefix(c, "make[") {
			continue
		}
		cmds = append(cmds, c)
	}
	return cmds
}

// intermediateRm is how make deletes intermediate files.
var intermediateRm = regexp.MustCompile(`^rm( [^-][^ ;|&]*)+$`)

var ignoreErrors = regexp.MustCompile(`^\((.*)\) \|\| true$`)

// RmakeCommands extracts the commands of rmake -n output.
func RmakeCommands(out string) []string {
	var cmds []string
	for _, l := range strings.Split(out, "\n") {
		if l == "" || strings.HasPrefix(l, "# ") || strings.HasPrefix(l, "rmake: ") {
			continue
		}
		c := normCmd(l)
		if m := ignoreErrors.FindStringSubmatch(c); m != nil {
			c = normCmd(m[1])
		}
		cmds = append(cmds, c)
	}
	return cmds
}

// CompareCommands compares multisets of commands (want: make's, got:
// rmake's) and describes the differences ("" if none). make-only "rm FILES" of
// intermediate files, "mkdir" of directory targets and recursive "make"
// commands are accepted: rmake keeps intermediates, creates output
// directories itself and folds recursive makes into one graph.
func CompareCommands(want, got []string) string {
	count := map[string]int{}
	for _, c := range got {
		count[c]++
	}
	var missing, extra []string
	for _, c := range want {
		if count[c] > 0 {
			count[c]--
			continue
		}
		if intermediateRm.MatchString(c) || strings.HasPrefix(c, "mkdir ") || strings.HasPrefix(c, "make ") {
			continue
		}
		missing = append(missing, c)
	}
	for c, n := range count {
		for ; n > 0; n-- {
			extra = append(extra, c)
		}
	}
	sort.Strings(extra)
	if len(missing) == 0 && len(extra) == 0 {
		return ""
	}
	var b strings.Builder
	for _, c := range missing {
		b.WriteString("  make only:  " + c + "\n")
	}
	for _, c := range extra {
		b.WriteString("  rmake only: " + c + "\n")
	}
	return b.String()
}
