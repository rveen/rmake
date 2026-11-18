package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain lets tests run rmake as a child process: the test binary itself
// acts as rmake when RMAKE_TEST_MAIN is set.
func TestMain(m *testing.M) {
	if os.Getenv("RMAKE_TEST_MAIN") != "" {
		os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// diamond is a wide build: 20 sources, compiled, then combined in two
// layers.
func diamond(t *testing.T) *proj {
	files := map[string]string{"Rmakefile": `default build/all.txt
rule build/%.o from src/%.c
  sh "echo $out >> ran.log; sleep 0.01; tr a-z A-Z < $in > $out"
rule build/even.txt from build/*0.o build/*2.o build/*4.o build/*6.o build/*8.o
  sh "echo $out >> ran.log; cat $in > $out"
rule build/odd.txt from build/*1.o build/*3.o build/*5.o build/*7.o build/*9.o
  sh "echo $out >> ran.log; cat $in > $out"
rule build/all.txt from build/even.txt build/odd.txt
  sh "echo $out >> ran.log; cat $in > $out"
`}
	for i := range 20 {
		files[fmt.Sprintf("src/f%02d.c", i)] = fmt.Sprintf("file %d\n", i)
	}
	return newProj(t, files)
}

// snapshot returns the contents of every file under dir/build.
func snapshot(dir string) map[string]string {
	m := map[string]string{}
	filepath.Walk(filepath.Join(dir, "build"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(dir, p)
			m[rel] = string(b)
		}
		return nil
	})
	return m
}

func TestParallelSameResult(t *testing.T) {
	var snaps []map[string]string
	var rans [][]string
	for _, j := range []string{"-j1", "-j16"} {
		p := diamond(t)
		code, out, ran := p.build(j)
		if code != 0 {
			t.Fatalf("%s: exit %d\n%s", j, code, out)
		}
		// 20 objects + even + odd + all.
		if got := strings.Count(out, "/23] "); got != 23 {
			t.Errorf("%s: %d progress lines, want 23:\n%s", j, got, out)
		}
		snaps = append(snaps, snapshot(p.dir))
		rans = append(rans, ran)
		p.expect(j+" no-op", nil, j)
		// A change in one source reruns exactly its chain, and the
		// progress total counts only the steps that run.
		p.write("src/f03.c", "changed\n")
		out = p.expect(j+" one change", []string{"build/all.txt", "build/f03.o", "build/odd.txt"}, j)
		if !strings.Contains(out, "[1/3] ") || !strings.Contains(out, "[3/3] ") {
			t.Errorf("%s: progress:\n%s", j, out)
		}
	}
	if !reflect.DeepEqual(snaps[0], snaps[1]) {
		t.Errorf("-j1 and -j16 differ:\n%v\n%v", snaps[0], snaps[1])
	}
	if !reflect.DeepEqual(rans[0], rans[1]) {
		t.Errorf("-j1 and -j16 ran different rules:\n%v\n%v", rans[0], rans[1])
	}
}

func TestParallelRuns(t *testing.T) {
	// Each rule waits until the other has started; this succeeds only if
	// both run at the same time.
	wait := func(me, other string) string {
		return fmt.Sprintf("  sh \"touch %s.started; i=0; while [ ! -e %s.started ]; do i=$$((i+1)); [ $$i -gt 500 ] && exit 1; sleep 0.01; done; echo ok > $out\"\n", me, other)
	}
	p := newProj(t, map[string]string{
		"Rmakefile": "default a.txt b.txt\nrule a.txt from\n" + wait("a", "b") + "rule b.txt from\n" + wait("b", "a"),
	})
	code, out, _ := p.build("-j2")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestParallelFailure(t *testing.T) {
	// slow.txt is running when bad.txt fails: without -k rmake waits for it
	// (and records it) but starts nothing else.
	p := newProj(t, map[string]string{
		"Rmakefile": `default all.txt later.txt
rule bad.txt from
  sh "echo $out >> ran.log; exit 1"
rule slow.txt from
  sh "echo $out >> ran.log; sleep 0.3; echo slow > $out"
rule all.txt from bad.txt slow.txt
  sh "echo $out >> ran.log; cat $in > $out"
rule later.txt from slow.txt
  sh "echo $out >> ran.log; cat $in > $out"
`,
	})
	code, out, ran := p.build("-j2")
	if code != 1 || !reflect.DeepEqual(ran, []string{"bad.txt", "slow.txt"}) {
		t.Fatalf("exit %d, ran %q\n%s", code, ran, out)
	}
	if !p.exists("slow.txt") {
		t.Error("running job was not waited for")
	}
	// The finished job was recorded; -k now builds what does not depend
	// on bad.txt.
	code, out, ran = p.build("-j2", "-k")
	if code != 1 || !reflect.DeepEqual(ran, []string{"bad.txt", "later.txt"}) {
		t.Fatalf("-k: exit %d, ran %q\n%s", code, ran, out)
	}
}

func TestInterrupt(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a child rmake")
	}
	p := newProj(t, map[string]string{
		"Rmakefile": `default a.txt b.txt
rule a.txt from
  sh "echo partial > $out; touch a.started; sleep 30; echo done >> $out"
rule b.txt from
  sh "echo partial > $out; touch b.started; sleep 30; echo done >> $out"
`,
	})
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-C", p.dir, "-j2")
	cmd.Env = append(os.Environ(), "RMAKE_TEST_MAIN=1")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !p.exists("a.started") || !p.exists("b.started") {
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			cmd.Wait()
			t.Fatalf("jobs did not start\n%s", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	cmd.Process.Signal(syscall.SIGINT)
	err = cmd.Wait()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 130 {
		t.Fatalf("exit: %v\n%s", err, out.String())
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("interrupt took %v; signal not forwarded?", d)
	}
	if p.exists("a.txt") || p.exists("b.txt") {
		t.Errorf("interrupted rules left partial outputs")
	}
	if !strings.Contains(out.String(), "rmk: interrupted") {
		t.Errorf("output:\n%s", out.String())
	}
	// The state was flushed and is usable; the rules run again.
	p.write("Rmakefile", `default a.txt b.txt
rule a.txt from
  sh "echo $out >> ran.log; echo ok > $out"
rule b.txt from
  sh "echo $out >> ran.log; echo ok > $out"
`)
	p.expect("after interrupt", []string{"a.txt", "b.txt"})
}
