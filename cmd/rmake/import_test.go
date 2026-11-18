package main

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// corpus is the importer's differential test corpus (README §10.6): each
// directory holds a project with a makefile; an optional file "goals" lists
// one argument list per line (default: the default goal), an optional file
// "rundir" names the subdirectory make runs in.
const corpus = "../../testdata/makefiles"

// testEnv is the environment of both make and rmake in the differential
// tests.
func testEnv(home string) []string {
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "LANG=C"}
}

func TestImportDifferential(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("GNU make not installed")
	}
	entries, err := os.ReadDir(corpus)
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			copyTree(t, filepath.Join(corpus, name), dir)
			run := dir
			if b, err := os.ReadFile(filepath.Join(dir, "rundir")); err == nil {
				run = filepath.Join(dir, strings.TrimSpace(string(b)))
			}
			goals := []string{""}
			if b, err := os.ReadFile(filepath.Join(dir, "goals")); err == nil {
				goals = strings.Split(strings.TrimRight(string(b), "\n"), "\n")
			}
			for _, g := range goals {
				args := strings.Fields(g)
				mk := exec.Command("make", append([]string{"-n", "-B"}, args...)...)
				mk.Dir, mk.Env = run, testEnv(dir)
				var mout, merr bytes.Buffer
				mk.Stdout, mk.Stderr = &mout, &merr
				if err := mk.Run(); err != nil {
					t.Fatalf("make -n -B %s: %v\n%s%s", g, err, mout.String(), merr.String())
				}
				rm := exec.Command(self, append([]string{"-n", "-B"}, args...)...)
				rm.Dir, rm.Env = run, append(testEnv(dir), "RMAKE_TEST_MAIN=1")
				var rout, rerr bytes.Buffer
				rm.Stdout, rm.Stderr = &rout, &rerr
				if err := rm.Run(); err != nil {
					imp := exec.Command(self, "-t", "import", "-allow-shell")
					imp.Dir, imp.Env = run, append(testEnv(dir), "RMAKE_TEST_MAIN=1")
					src, _ := imp.CombinedOutput()
					t.Fatalf("rmake -n -B %s: %v\n%s%s\nimport:\n%s", g, err, rout.String(), rerr.String(), src)
				}
				want, got := makeCommands(mout.String()), rmakeCommands(rout.String())
				if diff := compareCommands(want, got); diff != "" {
					imp := exec.Command(self, "-t", "import", "-allow-shell")
					imp.Dir, imp.Env = run, append(testEnv(dir), "RMAKE_TEST_MAIN=1")
					src, _ := imp.CombinedOutput()
					t.Errorf("goal %q: commands differ:\n%s\nimport:\n%s", g, diff, src)
				}
			}
		})
	}
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

var spaces = regexp.MustCompile(`[ \t]+`)

func normCmd(s string) string {
	s = strings.ReplaceAll(s, "\\\n", "")
	return strings.TrimSpace(spaces.ReplaceAllString(s, " "))
}

// makeCommands splits make -n output into commands (joining continued
// lines) and drops make's own messages.
func makeCommands(out string) []string {
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

// rmakeCommands extracts the commands of rmake -n output.
func rmakeCommands(out string) []string {
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

// compareCommands compares multisets of commands. make-only "rm FILES" of
// intermediate files and "mkdir" of directory targets are accepted: rmake
// keeps intermediates and creates output directories itself.
func compareCommands(want, got []string) string {
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
		if intermediateRm.MatchString(c) || strings.HasPrefix(c, "mkdir ") {
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
