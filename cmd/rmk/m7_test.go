package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// subMake is a makefile with 4 independent jobs that log when they start
// and end.
const subMake = `all: j1 j2 j3 j4
j1 j2 j3 j4:
	@echo start >> ../events; sleep 0.3; echo end >> ../events
.PHONY: all j1 j2 j3 j4
`

// maxConcurrency replays start/end events.
func maxConcurrency(events string) int {
	n, max := 0, 0
	for _, e := range strings.Fields(events) {
		if e == "start" {
			n++
		} else {
			n--
		}
		if n > max {
			max = n
		}
	}
	return max
}

func TestJobserverBudget(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("GNU make not installed")
	}
	p := newProj(t, map[string]string{
		"Rmakefile":  "task all needs a b\ntask a\n  sh \"make -s -C a\"\ntask b\n  sh \"make -s -C b\"\n",
		"a/Makefile": subMake,
		"b/Makefile": subMake,
	})
	code, out, _ := p.build("-j3")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	events := p.read("events")
	if strings.Count(events, "start") != 8 {
		t.Fatalf("events:\n%s", events)
	}
	// Two nested makes, eight jobs, one budget of 3: without the jobserver
	// each make runs its jobs one by one (2 at a time); with it they share
	// the 3 slots.
	if m := maxConcurrency(events); m != 3 {
		t.Errorf("max concurrency %d, want 3\n%s", m, out)
	}
	if p.exists(".rmake/jobserver") {
		t.Error("jobserver FIFO left behind")
	}
}

func TestRecursiveTransparentBuild(t *testing.T) {
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no cc")
	}
	p := newProj(t, map[string]string{
		"Makefile": `SUBDIRS = lib app
.PHONY: all clean $(SUBDIRS)
all: $(SUBDIRS)
$(SUBDIRS):
	$(MAKE) -C $@
app: lib
clean:
	$(MAKE) -C lib clean
	$(MAKE) -C app clean
`,
		"lib/Makefile": `CFLAGS = -O2 -MMD
liblib.a: util.o
	@echo lib/$@ >> ../ran.log
	$(AR) rcs $@ $^
%.o: %.c
	@echo lib/$@ >> ../ran.log
	$(CC) $(CFLAGS) -c $< -o $@
clean:
	rm -f *.o *.d liblib.a
-include *.d
.PHONY: clean
`,
		"lib/util.c":   "#include \"util.h\"\nint util(void) { return VALUE; }\n",
		"lib/util.h":   "#define VALUE 0\n",
		"app/Makefile": "app: main.o ../lib/liblib.a\n\t@echo app/$@ >> ../ran.log\n\t$(CC) -o $@ main.o -L../lib -llib\nclean:\n\trm -f app *.o\n",
		"app/main.c":   "int util(void);\nint main(void) { return util(); }\n",
	})
	// One graph: lib and app build in one rmake run; app's objects may
	// compile in parallel with lib, the link waits for liblib.a.
	p.expect("first", []string{"app/app", "lib/liblib.a", "lib/util.o"})
	if !p.exists("app/app") {
		t.Fatal("app not built")
	}
	p.expect("no-op", nil)
	// The depfile written in lib/ (-MMD) names util.h relative to lib/.
	p.write("lib/util.h", "#define VALUE 0 /* changed */\n")
	p.expect("header", []string{"lib/util.o"})
	p.write("lib/util.c", "#include \"util.h\"\nint util(void) { return VALUE + 1; }\n")
	p.expect("source", []string{"app/app", "lib/liblib.a", "lib/util.o"})
	_, out, _ := p.build("-t", "query", "lib/util.o")
	if !strings.Contains(out, "runs in: lib") || !strings.Contains(out, "lib/util.h") {
		t.Errorf("query:\n%s", out)
	}
	p.expect("clean", nil, "clean")
	if p.exists("app/app") || p.exists("lib/liblib.a") {
		t.Error("clean did not clean")
	}
}

func TestImportCheck(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("GNU make not installed")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	check := func(dir string, args ...string) (int, string) {
		cmd := exec.Command(self, append([]string{"-t", "import", "-check"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "RMAKE_TEST_MAIN=1")
		out, _ := cmd.CombinedOutput()
		code := 0
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		return code, string(out)
	}
	p := newProj(t, map[string]string{
		"Makefile": "all: x\nx: x.c\n\t$(CC) -o $@ $<\nclean:\n\trm -f x\n",
		"x.c":      "",
	})
	if code, out := check(p.dir); code != 0 || !strings.Contains(out, "runs the same 1 command(s)") {
		t.Errorf("exact: %d\n%s", code, out)
	}
	if code, out := check(p.dir, "clean"); code != 0 || !strings.Contains(out, "runs the same 1 command(s)") {
		t.Errorf("goal: %d\n%s", code, out)
	}
	// An approximation that changes a command: make passes all's
	// target-specific variable on to a.
	p.write("Makefile", "all: X = 1\nall: a\na:\n\techo [$(X)] > $@\n")
	code, out := check(p.dir)
	if code != 1 || !strings.Contains(out, "make only:  echo [1] > a") || !strings.Contains(out, "rmake only: echo [] > a") {
		t.Errorf("approximation: %d\n%s", code, out)
	}
}
