package main

import (
	"os/exec"
	"strings"
	"testing"
)

const cMakefile = `CFLAGS ?= -O2
CFLAGS += -MMD
SRCS := $(wildcard src/*.c)
OBJS := $(SRCS:src/%.c=build/%.o)

app: $(OBJS)
	$(CC) -o $@ $^

build/%.o: src/%.c | build
	@echo $@ >> ran.log
	$(CC) $(CFLAGS) -Iinclude -c $< -o $@

build:
	mkdir -p $@

-include $(OBJS:.o=.d)

.PHONY: clean
clean:
	rm -rf build app
`

func TestTransparentBuild(t *testing.T) {
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no cc")
	}
	p := newProj(t, map[string]string{
		"Makefile":         cMakefile,
		"src/main.c":       "#include \"util.h\"\nint main(void) { return util(); }\n",
		"src/util.c":       "#include \"config.h\"\nint util(void) { return VALUE; }\n",
		"include/util.h":   "int util(void);\n",
		"include/config.h": "#define VALUE 0\n",
	})
	p.expect("first", []string{"build/main.o", "build/util.o"})
	if !p.exists("app") {
		t.Fatal("app not linked")
	}
	p.expect("no-op", nil)
	// Header dependencies come from the -MMD depfiles (deps gcc).
	p.write("include/config.h", "#define VALUE 0 /* changed */\n")
	p.expect("config.h", []string{"build/util.o"})
	// Command-line variables work as with make.
	out := p.expect("CFLAGS", []string{"build/main.o", "build/util.o"}, "-v", "CFLAGS=-O0")
	if !strings.Contains(out, "-O0") {
		t.Errorf("CFLAGS=-O0 not used:\n%s", out)
	}
	// Tasks from .PHONY.
	p.expect("clean", nil, "clean")
	if p.exists("app") || p.exists("build") {
		t.Error("clean did not clean")
	}
}

func TestTransparentRefusesApproximations(t *testing.T) {
	p := newProj(t, map[string]string{
		"Makefile": "vpath %.c src\nall: x\nx:\n\ttouch $@\n",
	})
	code, out, _ := p.build()
	for _, want := range []string{"Makefile cannot be imported exactly", "Makefile:1:1: vpath is not supported", "rmake -t import Makefile"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if code != exitUsage {
		t.Errorf("exit %d", code)
	}
	// -t import converts it anyway and reports the approximation.
	code, stdout, stderr := runIn(t, p.dir, "-t", "import")
	if code != 0 || !strings.Contains(stdout, "# rmake: Makefile:1:1: vpath is not supported") ||
		!strings.Contains(stderr, "1 approximation(s)") {
		t.Errorf("-t import: %d\n%s\n%s", code, stdout, stderr)
	}
}

func TestImportTool(t *testing.T) {
	p := newProj(t, map[string]string{
		"build.mk": "V := $(shell echo 1.2)\nall: out.txt\nout.txt:\n\techo $(V) > $@\n",
	})
	// $(shell) needs -allow-shell.
	code, _, stderr := runIn(t, p.dir, "-t", "import", "build.mk")
	if code != exitUsage || !strings.Contains(stderr, "needs -allow-shell") {
		t.Errorf("without -allow-shell: %d %s", code, stderr)
	}
	code, stdout, _ := runIn(t, p.dir, "-t", "import", "-allow-shell", "build.mk")
	if code != 0 || !strings.Contains(stdout, "var V = 1.2\n") || !strings.Contains(stdout, `sh "echo $V > $out"`) {
		t.Errorf("import: %d\n%s", code, stdout)
	}
	// The output is a valid, canonical build file.
	p.write("Rmakefile", stdout)
	p.write("ran.log", "")
	code, out, _ := p.build("out.txt")
	if code != 0 || p.read("out.txt") != "1.2\n" {
		t.Errorf("imported build: %d\n%s", code, out)
	}
	_, formatted, _ := runIn(t, p.dir, "-t", "fmt")
	if formatted != stdout {
		t.Errorf("import output is not in canonical form:\n%s\nfmt:\n%s", stdout, formatted)
	}
}

func TestIncludeMakefileFromRmakefile(t *testing.T) {
	// An included .mk is imported; its paths are relative to the root, as
	// in make; it does not set the default target.
	p := newProj(t, map[string]string{
		"Rmakefile": "include mk/gen.mk\nrule all.txt from gen/a.txt\n  sh \"echo all.txt >> ran.log; cp $in $out\"\n",
		"mk/gen.mk": "gen/%.txt: data/%.in\n\techo $@ >> ran.log; cp $< $@\nhelper:\n\ttrue\n",
		"data/a.in": "a\n",
	})
	p.expect("build", []string{"all.txt", "gen/a.txt"})
	if p.read("all.txt") != "a\n" {
		t.Error("wrong content")
	}
}

func TestDirAttribute(t *testing.T) {
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no cc")
	}
	// Commands run in dir; depfile paths written there are relative to it.
	p := newProj(t, map[string]string{
		"Rmakefile": `rule lib/x.o from lib/x.c
  dir lib
  deps gcc
  depfile lib/x.d
  sh "echo x.o >> ../ran.log; cc -MMD -Iinc -c x.c -o x.o"
task show
  dir lib
  sh "pwd > ../pwd.txt"
`,
		"lib/x.c":     "#include \"x.h\"\nint x(void) { return X; }\n",
		"lib/inc/x.h": "#define X 1\n",
	})
	p.expect("first", []string{"x.o"})
	p.expect("no-op", nil)
	_, out, _ := p.build("-t", "query", "lib/x.o")
	if !strings.Contains(out, "runs in: lib") || !strings.Contains(out, "lib/inc/x.h") {
		t.Errorf("query:\n%s", out)
	}
	p.write("lib/inc/x.h", "#define X 2\n")
	p.expect("header", []string{"x.o"})
	p.expect("task", nil, "show")
	if !strings.HasSuffix(strings.TrimSpace(p.read("pwd.txt")), "/lib") {
		t.Errorf("task dir: %q", p.read("pwd.txt"))
	}
	// dir in an included file is relative to that file's directory.
	p.write("Rmakefile", "include lib/Rmakefile\n")
	p.write("lib/Rmakefile", "task where\n  dir .\n  sh \"pwd > ../pwd2.txt\"\n")
	p.expect("included", nil, "where")
	if !strings.HasSuffix(strings.TrimSpace(p.read("pwd2.txt")), "/lib") {
		t.Errorf("included dir: %q", p.read("pwd2.txt"))
	}
}
