package makefile

import (
	"strings"
	"testing"
)

func TestParseSubmake(t *testing.T) {
	for _, tc := range []struct {
		line string
		ok   bool
		want string // dir|file|targets|vars
	}{
		{"make -C lib", true, "lib||[]|map[]"},
		{"@make -s -C src/lib all CFLAGS=-g", true, "src/lib||[all]|map[CFLAGS:-g]"},
		{"cd lib && make install", true, "lib||[install]|map[]"},
		{"cd a && make -C b -f other.mk x y", true, "a/b|other.mk|[x y]|map[]"},
		{"make --directory=x -j4 --no-print-directory", true, "x||[]|map[]"},
		{"make -C lib; make -C app", false, ""},
		{"for d in a b; do make -C $d; done", false, ""},
		{"make -n -C lib", false, ""},
		{"make -C lib > log", false, ""},
		{"gmake -C lib", false, ""},
	} {
		inv, ok := parseSubmake(tc.line, "make")
		if ok != tc.ok {
			t.Errorf("%q: ok = %v", tc.line, ok)
			continue
		}
		if !ok {
			continue
		}
		got := inv.dir + "|" + inv.file + "|" + sprintf("%v|%v", inv.targets, inv.vars)
		if inv.targets == nil {
			got = inv.dir + "|" + inv.file + "|[]|" + sprintf("%v", inv.vars)
		}
		if got != tc.want {
			t.Errorf("%q: got %s, want %s", tc.line, got, tc.want)
		}
	}
}

const libMk = `CFLAGS = -O2
liblib.a: a.o
	$(AR) rcs $@ $^
clean:
	rm -f *.o liblib.a
.PHONY: clean
`

func TestRecursiveMake(t *testing.T) {
	res, err := imp(t, map[string]string{
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
		"lib/Makefile": libMk,
		"lib/a.c":      "",
		"app/Makefile": "app: main.o ../lib/liblib.a\n\t$(CC) -o $@ main.o -L../lib -llib\nclean:\n\trm -f app *.o\n",
		"app/main.c":   "",
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	src := string(res.Source)
	for _, want := range []string{
		"task lib needs lib/liblib.a\n",
		"task app needs lib app/app\n",
		"task clean needs lib.clean app.clean\n",
		"rule lib/liblib.a from lib/a.o\n  dir lib\n  sh \"ar rcs liblib.a a.o\"\n",
		"rule lib/a.o from lib/a.c\n  dir lib\n  sh \"cc -O2 -c -o a.o a.c\"\n",
		"rule app/app from app/main.o | lib/liblib.a\n  dir app\n",
		"task lib.clean\n  dir lib\n  sh \"rm -f *.o liblib.a\"\n",
	} {
		if !has(src, want) {
			t.Errorf("missing %q in\n%s", want, src)
		}
	}
	if res.Approx() {
		t.Errorf("approximations: %v", res.Notes)
	}
}

func TestRecursiveMakeOrder(t *testing.T) {
	// app links -llib without depending on lib's file: make's order is
	// lost in rmake.
	res, err := imp(t, map[string]string{
		"Makefile":     "all:\n\t$(MAKE) -C lib\n\t$(MAKE) -C app\n.PHONY: all\n",
		"lib/Makefile": libMk,
		"lib/a.c":      "",
		"app/Makefile": "app: main.o\n\t$(CC) -o $@ main.o -L../lib -llib\n",
		"app/main.c":   "",
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range res.Notes {
		if n.Approx && strings.Contains(n.Msg, "rmake would run them in parallel") {
			found = true
		}
	}
	if !found {
		t.Errorf("no ordering approximation: %v\n%s", res.Notes, res.Source)
	}
}

func TestDelegation(t *testing.T) {
	res, err := imp(t, map[string]string{
		"Makefile": `.PHONY: all loop odd
all: lib/liblib.a loop odd
lib/liblib.a:
	$(MAKE) -C lib
loop:
	for d in lib; do $(MAKE) -C $$d; done
odd:
	$(MAKE) -C weird
`,
		"lib/Makefile":   libMk,
		"lib/a.c":        "",
		"weird/Makefile": "vpath %.c src\nx:\n\ttouch x\n",
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	src := string(res.Source)
	for _, want := range []string{
		// A file rule: delegated, always.
		"rule lib/liblib.a from\n  always\n  sh \"make -C lib\"\n",
		"task loop\n  sh \"for d in lib; do make -C $$d; done\"\n",
		// The sub-make needs an approximation: delegated, exact.
		"task odd\n  sh \"make -C weird\"\n",
	} {
		if !has(src, want) {
			t.Errorf("missing %q in\n%s", want, src)
		}
	}
	if res.Approx() {
		t.Errorf("delegation is exact: %v", res.Notes)
	}
	notes := 0
	for _, n := range res.Notes {
		if strings.Contains(n.Msg, "delegated to make") {
			notes++
		}
	}
	if notes != 3 {
		t.Errorf("delegation notes: %v", res.Notes)
	}
}

func TestSubmakeVariables(t *testing.T) {
	// Command-line variables of the recursive make and exported variables
	// reach the sub-make; cd dir && $(MAKE) works.
	res, err := imp(t, map[string]string{
		"Makefile":     "export MODE = fast\nall:\n\tcd sub && $(MAKE) OPT=-O3\n.PHONY: all\n",
		"sub/Makefile": "out.txt:\n\techo $(OPT) $(MODE) > $@\n",
	}, Options{Overrides: map[string]string{"EXTRA": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if !has(string(res.Source), "rule sub/out.txt from\n  dir sub\n  sh \"echo -O3 fast > out.txt\"\n") {
		t.Errorf("got\n%s", res.Source)
	}
}
