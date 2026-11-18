package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// ccScript is a fake compiler: "sh cc.sh IN OUT" copies IN and the existing
// files it includes (lines "inc FILE") to OUT and writes the depfile OUT.d
// with absolute paths, as gcc -MD does for absolute include directories.
const ccScript = `in=$1; out=$2
echo $out >> ran.log
deps=$(sed -n 's/^inc //p' $in)
abs=""
cat $in > $out
for d in $deps; do
  [ -e $d ] && cat $d >> $out
  abs="$abs $(pwd)/$d"
done
printf '%s: %s \\\n %s\n' $out $in "$abs" > $out.d
`

func TestDepfiles(t *testing.T) {
	p := newProj(t, map[string]string{
		"Rmakefile": `rule build/app from build/a.o build/b.o
  sh "echo $out >> ran.log; cat $in > $out"
rule build/%.o from src/%.c
  deps gcc
  run sh cc.sh $in $out
`,
		"cc.sh":    ccScript,
		"src/a.c":  "a\ninc inc/h1.h\n",
		"src/b.c":  "b\ninc inc/h2.h\n",
		"inc/h1.h": "h1\n",
		"inc/h2.h": "h2\n",
		"inc/x.h":  "unused\n",
	})
	p.expect("first", []string{"build/a.o", "build/app", "build/b.o"})
	if p.exists("build/a.o.d") {
		t.Error("depfile was not deleted")
	}
	_, out, _ := p.build("-t", "query", "build/a.o")
	if !strings.Contains(out, "discovered inputs:\n    inc/h1.h\n  last built") {
		t.Errorf("query (discovered inputs are root-relative, without declared inputs):\n%s", out)
	}
	p.expect("no-op", nil)

	// A header change rebuilds only the objects that included it.
	p.write("inc/h1.h", "h1 changed\n")
	p.expect("header change", []string{"build/a.o", "build/app"})
	_, out, _ = p.build("-t", "why", "build/a.o")
	if !strings.Contains(out, "up to date") {
		t.Errorf("why:\n%s", out)
	}
	p.write("inc/h2.h", "h2 changed\n")
	_, out, _ = p.build("-t", "why", "build/b.o")
	if !strings.Contains(out, "discovered input inc/h2.h changed") {
		t.Errorf("why after header change:\n%s", out)
	}
	p.expect("second header change", []string{"build/app", "build/b.o"})

	// Touching a header changes nothing; an unrelated header is not tracked.
	future := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(p.dir, "inc/h1.h"), future, future)
	p.write("inc/x.h", "changed\n")
	p.expect("touch and unrelated header", nil)

	// A missing discovered input reruns the rule (not an error); then it is
	// recorded as missing and the next build is a no-op.
	os.Remove(filepath.Join(p.dir, "inc/h2.h"))
	p.expect("deleted header", []string{"build/app", "build/b.o"})
	p.expect("deleted header again", nil)

	// The include set changes with the source.
	p.write("src/b.c", "b\ninc inc/h1.h\n")
	p.expect("include change", []string{"build/app", "build/b.o"})
	p.write("inc/h1.h", "h1 again\n")
	p.expect("now both include h1", []string{"build/a.o", "build/app", "build/b.o"})

	// A failed rule's depfile is removed too.
	p.write("src/a.c", "a\ninc inc/h1.h\n")
	p.write("Rmakefile", `rule build/a.o from src/a.c
  deps gcc
  sh "echo $out >> ran.log; echo 'x: y' > $out.d; exit 1"
`)
	if code, out, _ := p.build(); code != 1 || p.exists("build/a.o.d") {
		t.Errorf("failed rule: %d, depfile left: %v\n%s", code, p.exists("build/a.o.d"), out)
	}
	// A broken depfile fails the rule.
	p.write("Rmakefile", `rule build/a.o from src/a.c
  deps gcc
  depfile build/deps.txt
  sh "echo $out >> ran.log; cp $in $out; echo 'no colon here' > build/deps.txt"
`)
	if code, out, _ := p.build(); code != 1 || !strings.Contains(out, "missing ':'") || p.exists("build/a.o") {
		t.Errorf("broken depfile: %d\n%s", code, out)
	}
	// A rule with deps whose command writes no depfile has no discovered
	// inputs.
	p.write("Rmakefile", `rule build/a.o from src/a.c
  deps gcc
  sh "echo $out >> ran.log; cp $in $out"
`)
	p.expect("no depfile", []string{"build/a.o"})
	p.expect("no depfile, no-op", nil)
}

func TestAlways(t *testing.T) {
	p := newProj(t, map[string]string{
		"Rmakefile": `default final.txt
rule stamp.txt from
  always
  sh "echo $out >> ran.log; echo fixed > $out"
rule final.txt from stamp.txt
  sh "echo $out >> ran.log; cat $in > $out"
`,
	})
	p.expect("first", []string{"final.txt", "stamp.txt"})
	// always reruns; the identical output does not rerun dependents.
	p.expect("second", []string{"stamp.txt"})
	_, out, _ := p.build("-n")
	if !strings.Contains(out, "# stamp.txt: always") {
		t.Errorf("-n:\n%s", out)
	}
}

const genRoot = `default all.txt
include build/gen.rmk
rule build/gen.rmk from gen.in
  sh "echo $out >> ran.log; cp $in $out"
rule all.txt from build/msg.txt
  sh "echo $out >> ran.log; cat $in > $out"
`

func genTemplate(word string) string {
	return "rule build/msg.txt from\n  sh \"echo $out >> ran.log; echo " + word + " > $out\"\n"
}

func TestGeneratedInclude(t *testing.T) {
	p := newProj(t, map[string]string{"Rmakefile": genRoot, "gen.in": genTemplate("hello")})
	// The fragment does not exist yet: it is built, then the build file is
	// reloaded and its rule used.
	p.expect("first", []string{"all.txt", "build/gen.rmk", "build/msg.txt"})
	if got := p.read("all.txt"); got != "hello\n" {
		t.Errorf("all.txt = %q", got)
	}
	out := p.expect("no-op", nil)
	if !strings.Contains(out, "no work to do") {
		t.Errorf("no-op:\n%s", out)
	}

	// A changed fragment is rebuilt before the graph is used, even with -n
	// (as make does) ...
	p.write("gen.in", genTemplate("bye"))
	code, out, ran := p.build("-n")
	if code != 0 || !reflect.DeepEqual(ran, []string{"build/gen.rmk"}) ||
		!strings.Contains(out, "echo bye > build/msg.txt") {
		t.Fatalf("-n: %d %q\n%s", code, ran, out)
	}
	p.expect("changed fragment", []string{"all.txt", "build/msg.txt"})
	if got := p.read("all.txt"); got != "bye\n" {
		t.Errorf("all.txt = %q", got)
	}

	// The tools see the generated rules.
	os.RemoveAll(filepath.Join(p.dir, "build"))
	_, out, errs := runIn(t, p.dir, "-t", "targets")
	if out != "all.txt\nbuild/gen.rmk\nbuild/msg.txt\n" || !strings.Contains(errs, "cp gen.in build/gen.rmk") {
		t.Errorf("targets:\n%s\nstderr:\n%s", out, errs)
	}

	// A generated include that is not produced by a rule and missing.
	p.write("Rmakefile", "include build/nothing.rmk\nrule x from\n  sh \"touch x\"\n")
	if code, out, _ := p.build(); code != 2 || !strings.Contains(out, "Rmakefile:1:1: include build/nothing.rmk: no such file") {
		t.Errorf("missing include: %d\n%s", code, out)
	}
}

func TestGeneratedIncludeChain(t *testing.T) {
	// The fragment generates another fragment: one reload is not enough.
	p := newProj(t, map[string]string{
		"Rmakefile": genRoot,
		"gen.in": `include build/gen2.rmk
rule build/gen2.rmk from ../gen2.in
  sh "cp $in $out"
` + genTemplate("hello"),
		"gen2.in": "rule other from\n  sh \"touch other\"\n",
	})
	code, out, _ := p.build()
	if code != 2 || !strings.Contains(out, "generated include build/gen2.rmk changed after the build file was reloaded") {
		t.Errorf("chain: %d\n%s", code, out)
	}
	// The next run has both fragments and works.
	p.expect("second", []string{"all.txt", "build/msg.txt"})
}

func TestDepfilesRealCC(t *testing.T) {
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no cc")
	}
	p := newProj(t, map[string]string{
		"Rmakefile": `rule build/app from build/main.o build/util.o
  sh "echo $out >> ran.log; cc -o $out $in"
rule build/%.o from src/%.c
  deps gcc
  sh "echo $out >> ran.log; cc -Iinclude -MD -MF $out.d -c $in -o $out"
`,
		"src/main.c":       "#include \"util.h\"\nint main(void) { return util(); }\n",
		"src/util.c":       "#include \"config.h\"\nint util(void) { return VALUE; }\n",
		"include/util.h":   "int util(void);\n",
		"include/config.h": "#define VALUE 0\n",
	})
	p.expect("first", []string{"build/app", "build/main.o", "build/util.o"})
	p.write("include/config.h", "#define VALUE 0 /* comment only */\n")
	// util.o is rebuilt; comments do not change the object, so the link is
	// cut off early.
	p.expect("config.h", []string{"build/util.o"})
	p.write("include/util.h", "int util(void); /* changed */\n")
	p.expect("util.h", []string{"build/main.o"})
}
