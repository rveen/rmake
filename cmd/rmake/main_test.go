package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runIn(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb strings.Builder
	code := run(append([]string{"-C", dir}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

func TestCLI(t *testing.T) {
	wd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(wd) })
	dir := t.TempDir()
	write := func(name, content string) {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Rmakefile", `var cflags ?= -O2
rule build/%.o from src/%.c
  run cc $cflags -c $in -o $out
rule build/app from build/*.o
  run cc -o $out $in
task release(version) needs build/app
  desc "tag a release"
  run git tag v$version
`)
	write("src/a.c", "")

	code, out, errs := runIn(t, dir, "-n")
	if code != 0 || !strings.Contains(out, "cc -O2 -c src/a.c -o build/a.o\n") || !strings.Contains(out, "cc -o build/app build/a.o\n") {
		t.Errorf("-n: %d %q %q", code, out, errs)
	}
	code, out, _ = runIn(t, dir, "cflags=-O0 -g", "-n", "release", "1.0")
	if code != 0 || !strings.Contains(out, "cc -O0 -g -c src/a.c") || !strings.HasSuffix(out, "git tag v1.0\n") {
		t.Errorf("override + task: %d %q", code, out)
	}
	code, _, errs = runIn(t, dir, "-n", "release")
	if code != 2 || !strings.Contains(errs, "takes 1 argument(s) (version), got 0") {
		t.Errorf("missing task arg: %d %q", code, errs)
	}
	code, out, _ = runIn(t, dir, "-t", "tasks")
	if code != 0 || !strings.Contains(out, "release(version)") || !strings.Contains(out, "tag a release") {
		t.Errorf("tasks: %q", out)
	}
	code, out, _ = runIn(t, dir, "-t", "targets")
	if code != 0 || out != "build/a.o\nbuild/app\n" {
		t.Errorf("targets: %q", out)
	}
	code, _, _ = runIn(t, dir)
	if code != 1 {
		t.Errorf("build without -n should fail until M2, got %d", code)
	}

	write("bad/Rmakefile", "rule a from b\n  rn x\n")
	code, _, errs = runIn(t, filepath.Join(dir, "bad"), "-n")
	if code != 2 || !strings.Contains(errs, "Rmakefile:2:3: unknown rule attribute") {
		t.Errorf("bad file: %d %q", code, errs)
	}

	write("fmt/Rmakefile", "rule a from b\n      run   x\n")
	code, out, _ = runIn(t, filepath.Join(dir, "fmt"), "-t", "fmt")
	if code != 0 || out != "rule a from b\n  run x\n" {
		t.Errorf("fmt: %q", out)
	}
}
