package makefile

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func imp(t *testing.T, files map[string]string, opts Options) (*Result, error) {
	t.Helper()
	dir := t.TempDir()
	for name, c := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	opts.Dir = dir
	if opts.File == "" {
		opts.File = "Makefile"
	}
	return Import(opts)
}

func mustImport(t *testing.T, mk string, opts Options) string {
	t.Helper()
	res, err := imp(t, map[string]string{"Makefile": mk}, opts)
	if err != nil {
		t.Fatal(err)
	}
	return string(res.Source)
}

// has reports whether src contains want, ignoring runs of spaces (the
// output is aligned by the canonical printer).
func has(src, want string) bool {
	sp := regexp.MustCompile(` +`)
	return strings.Contains(sp.ReplaceAllString(src, " "), sp.ReplaceAllString(want, " "))
}

func TestErrors(t *testing.T) {
	for _, tc := range []struct{ mk, want string }{
		{"ifeq (a,b)\n", "Makefile:1:1: missing 'endif'"},
		{"endif\n", "Makefile:1:1: extraneous 'endif'"},
		{"a:: b\n\ttrue\n", "Makefile:1:1: double-colon rules are not supported"},
		{"X = $(shell echo hi)\nall:\n\techo $(X)\n", "Makefile:3:1: $(shell echo hi) needs -allow-shell"},
		{"$(error stop here)\n", "Makefile:1:1: stop here"},
		{"x := $(foo\n", "unterminated variable reference"},
		{"oops\n", "Makefile:1:1: missing separator"},
		{"include nope.mk\n", "include nope.mk: no such file"},
		{".SECONDEXPANSION:\n", ".SECONDEXPANSION is not supported"},
		{"define X\nabc\n", "missing 'endef'"},
		{"all:\n\techo $(word 0,a)\n", "first argument to 'word' function must be greater than 0"},
	} {
		_, err := imp(t, map[string]string{"Makefile": tc.mk}, Options{})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: got %v, want %q", tc.mk, err, tc.want)
		}
	}
}

func TestApproximations(t *testing.T) {
	for _, tc := range []struct{ mk, want string }{
		{"vpath %.c src\nall:\n\ttrue\n", "vpath is not supported"},
		{"VPATH = src\nall:\n\ttrue\n", "VPATH is not supported"},
		{"SHELL = /bin/bash\nall:\n\ttrue\n", "SHELL is set"},
		{".ONESHELL:\nall:\n\ttrue\n", ".ONESHELL is not supported"},
		{"app: gen\n\tcp x $@\n.PHONY: gen\ngen:\n\techo gen\n", "depends on the task gen, which has a recipe"},
		{"all: a\nall: X = 1\na:\n\techo $(X) > $@\n", "target-specific variable X of all is inherited by a"},
		{"all:\n\techo 'a \\\n\tb' > $@\n", "continues inside single quotes"},
		{".PHONY: docs/html\ndocs/html:\n\ttrue\n", "docs/html cannot be a task name"},
	} {
		res, err := imp(t, map[string]string{"Makefile": tc.mk}, Options{})
		if err != nil {
			t.Errorf("%q: %v", tc.mk, err)
			continue
		}
		found := false
		for _, n := range res.Notes {
			if n.Approx && strings.Contains(n.Msg, tc.want) {
				found = true
			}
		}
		if !found {
			t.Errorf("%q: no approximation %q in %v", tc.mk, tc.want, res.Notes)
		}
	}
	// Exact imports have no approximations.
	res, err := imp(t, map[string]string{"Makefile": "all: x\nx:\n\ttouch $@\n"}, Options{})
	if err != nil || res.Approx() {
		t.Errorf("exact import: %v %v", err, res.Notes)
	}
}

func TestEnvironmentDefaults(t *testing.T) {
	mk := "CFLAGS += -Wall\nOPT ?= -O2\nall: x\nx:\n\t$(CC) $(CFLAGS) $(OPT) $(HOME) -o $@\n"
	// The environment's values are what rmake will read at run time, so
	// the emitted defaults are the values without the environment.
	src := mustImport(t, mk, Options{Environ: []string{"CC=clang", "CFLAGS=-g", "OPT=-O0", "HOME=/h"}})
	for _, want := range []string{
		"var CC ?= cc\n", "var CFLAGS ?=\nvar CFLAGS += -Wall\n", "var OPT ?= -O2\n", "var HOME ?=\n",
		`sh "$CC $CFLAGS $OPT $HOME -o $out"`,
	} {
		if !has(src, want) {
			t.Errorf("missing %q in\n%s", want, src)
		}
	}
	// Command-line values are concrete; assignments in the file do not
	// read the environment.
	src = mustImport(t, "CC = gcc\nall: x\nx:\n\t$(CC) $(CFLAGS) -o $@\n", Options{
		Environ: []string{"CC=clang"}, Overrides: map[string]string{"CFLAGS": "-O3 -g"}})
	for _, want := range []string{"var CC = gcc\n", "var CFLAGS = -O3 -g\n"} {
		if !has(src, want) {
			t.Errorf("missing %q in\n%s", want, src)
		}
	}
}

func TestSymbolic(t *testing.T) {
	src := mustImport(t, `INC = a b
OUT = $@.tmp
COMPILE.x = cc -I$(INC)
X.Y-z = v
all: f
f: g h
	$(COMPILE.x) $(OUT) $(X.Y-z) $(INC)x $(foreach i,$(INC),-D$(i)) $< $^
g h:
	touch $@
`, Options{})
	for _, want := range []string{
		// Not a whole word: expanded in the variable value.
		"var COMPILE_x = cc -Ia b\n",
		// '.' and '-' become '_'.
		"var X_Y_z = v\n",
	} {
		if !has(src, want) {
			t.Errorf("missing %q in\n%s", want, src)
		}
	}
	// $@ in OUT makes it concrete; $INC stays symbolic inside the sh
	// string (text substitution); the function result is concrete; $< and
	// $^ differ, so $< is concrete and $^ is $in.
	want := `sh "$COMPILE_x $out.tmp $X_Y_z ${INC}x -Da -Db g $in"`
	if !has(src, want) {
		t.Errorf("missing %q in\n%s", want, src)
	}
}

func TestDepfileDetection(t *testing.T) {
	src := mustImport(t, `all: a.o b.o c.o
a.o: a.c
	cc -MMD -c $< -o $@
b.o: b.c
	cc -MD -MF deps/b.dep -c $< -o $@
c.o: c.c
	cc -c $< -o $@
`, Options{})
	for _, want := range []string{
		"rule a.o from a.c\n  deps gcc\n  depfile a.d\n",
		"rule b.o from b.c\n  deps gcc\n  depfile deps/b.dep\n",
		"rule c.o from c.c\n  sh",
	} {
		if !has(src, want) {
			t.Errorf("missing %q in\n%s", want, src)
		}
	}
}

func TestTargetsAndTasks(t *testing.T) {
	src := mustImport(t, `.PHONY: test-unit
all: prog
prog: main.c
	cc -o $@ $<
test-unit: prog
	./prog --test
install:
	cp prog /usr/bin
upload: prog
	scp prog host:
.DEFAULT_GOAL := prog
`, Options{})
	for _, want := range []string{
		"default prog\n",
		"task all needs prog\n",
		"task test-unit needs prog\n",
		"task install\n",           // a known task name
		"task upload needs prog\n", // the recipe does not create it
	} {
		if !has(src, want) {
			t.Errorf("missing %q in\n%s", want, src)
		}
	}
}

func FuzzImport(f *testing.F) {
	for _, s := range []string{
		"all: a b\n\techo $@ $< $^\na: ; touch $@\n",
		"X = $(Y)\nY := $(patsubst %.c,%.o,a.c b.c)\nifeq ($(X),)\nZ += 1\nelse\nZ = 2\nendif\n",
		"define T\n$(1): ; echo $$@\nendef\n$(foreach p,a b,$(eval $(call T,$(p))))\n",
		"%.o: %.c\n\tcc -c $<\nprog: prog.o\n.PHONY: all\nall: prog\n",
		"a b &: c\n\tgen\nOBJS = x.o\n$(OBJS): %.o: %.c h\n\tcc $< -o $@\n",
		".SUFFIXES: .x .y\n.x.y:\n\tconv $< $@\nt: CFLAGS += -g\n",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, mk string) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte(mk), 0o644); err != nil {
			t.Fatal(err)
		}
		// Errors are fine; panics are not, and a successful import is a
		// valid build file (Import parses its own output).
		res, err := Import(Options{File: "Makefile", Dir: dir})
		if err == nil && len(res.Source) == 0 {
			t.Fatal("empty import")
		}
	})
}
