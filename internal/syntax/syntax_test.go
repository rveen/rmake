package syntax

import (
	"reflect"
	"strings"
	"testing"
)

const example = `var cc ?= cc
var cflags ?= -O2 -Wall
var srcs = glob src/*.c

if $os == windows
  var exe = .exe
else
  var exe = ""
end

default $builddir/app$exe

rule $builddir/%.o from src/%.c | src/config.h
  desc "cc $stem"
  deps gcc
  run  $cc $cflags -MD -MF $out.d -c $in -o $out

rule $builddir/app$exe from ${srcs:src/%.c=$builddir/%.o}
  run $cc -o $out $in

rule bin/server from
  always
  run    go build -o $out ./cmd/server

task test needs $builddir/app$exe bin/server
  desc "run all tests"
  run  ./$builddir/app$exe --selftest
  run  go test ./...

task release(version) needs test
  run git tag v$version
`

func mustParse(t *testing.T, src string) *File {
	t.Helper()
	f, err := Parse("Rmakefile", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return f
}

func TestParseExample(t *testing.T) {
	f := mustParse(t, example)
	if got := len(f.Stmts); got != 10 {
		t.Fatalf("got %d statements, want 10", got)
	}
	r := f.Stmts[5].(*Rule)
	if len(r.Outs) != 1 || len(r.Ins) != 1 || len(r.Implicit) != 1 || !r.Pipe {
		t.Errorf("rule header: %+v", r)
	}
	if len(r.Attrs) != 3 || r.Attrs[1].Kind != "deps" {
		t.Errorf("rule attrs: %+v", r.Attrs)
	}
	tk := f.Stmts[9].(*Task)
	if tk.Name != "release" || !reflect.DeepEqual(tk.Params, []string{"version"}) || len(tk.Needs) != 1 {
		t.Errorf("task: %+v", tk)
	}
	iff := f.Stmts[3].(*If)
	if iff.Op != "==" || len(iff.Then) != 1 || len(iff.Else) != 1 {
		t.Errorf("if: %+v", iff)
	}
	v := f.Stmts[2].(*Var)
	if !v.Glob || len(v.Values) != 1 {
		t.Errorf("glob var: %+v", v)
	}
}

func TestWordParts(t *testing.T) {
	tests := []struct {
		src  string
		want []Part
	}{
		{`abc`, []Part{{Lit: "abc"}}},
		{`""`, []Part{{Lit: "", Quoted: true}}},
		{`-I$x`, []Part{{Lit: "-I"}, {Var: "x"}}},
		{`$out.d`, []Part{{Var: "out"}, {Lit: ".d"}}},
		{`a$$b`, []Part{{Lit: "a$b"}}},
		{`"a $b c"`, []Part{{Lit: "a ", Quoted: true}, {Var: "b", Quoted: true}, {Lit: " c", Quoted: true}}},
		{`"say \"hi\" \\"`, []Part{{Lit: `say "hi" \`, Quoted: true}}},
		{`${x}y`, []Part{{Var: "x"}, {Lit: "y"}}},
		{`${s:src/%.c=$b/%.o}`, []Part{{Var: "s", Subst: &Subst{
			Pat:  []Part{{Lit: "src/%.c"}},
			Repl: []Part{{Var: "b"}, {Lit: "/%.o"}},
		}}}},
	}
	for _, tt := range tests {
		f := mustParse(t, "default "+tt.src+"\n")
		w := f.Stmts[0].(*Default).Targets[0]
		if w.Raw != tt.src {
			t.Errorf("%s: raw %q", tt.src, w.Raw)
		}
		got := stripPos(w.Parts)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", tt.src, got, tt.want)
		}
	}
}

func stripPos(ps []Part) []Part {
	var out []Part
	for _, p := range ps {
		p.Pos = Pos{}
		if p.Subst != nil {
			p.Subst = &Subst{Pat: stripPos(p.Subst.Pat), Repl: stripPos(p.Subst.Repl)}
		}
		out = append(out, p)
	}
	return out
}

func TestParseErrors(t *testing.T) {
	tests := []struct{ src, want string }{
		{"rule a b\n", "Rmakefile:1:1: rule needs 'from'"},
		{"rule from x\n", "Rmakefile:1:1: rule needs at least one output"},
		{"rule a from b | c | d\n", "Rmakefile:1:19: more than one '|'"},
		{"rule a from b\n  rn x\n", `Rmakefile:2:3: unknown rule attribute "rn"`},
		{"task t\n  deps gcc\n", `Rmakefile:2:3: unknown task attribute "deps"`},
		{"var x = \"abc\n", "Rmakefile:1:9: unterminated quote"},
		{"var x = $\n", "Rmakefile:1:9: '$' at end of line"},
		{"var x = $(y)\n", "Rmakefile:1:9: invalid character after '$'"},
		{"var x = ${y\n", "Rmakefile:1:9: expected '}' or ':'"},
		{"var x = ${y:a b}\n", "Rmakefile:1:14: unexpected ' '"},
		{"var x\n", "Rmakefile:1:5: expected '=', '+=' or '?='"},
		{"var 1x = a\n", `Rmakefile:1:5: invalid variable name "1x"`},
		{"if a == b\nvar x = 1\n", "Rmakefile:1:1: if without end"},
		{"end\n", "Rmakefile:1:1: end without if"},
		{"if a = b\nend\n", "Rmakefile:1:1: expected 'if A == B'"},
		{"frob x\n", `Rmakefile:1:1: unknown statement "frob"`},
		{"rule a from b\n  sh echo hi\n", "Rmakefile:2:3: sh takes exactly one word"},
		{"rule a from b\n  deps clang\n", "Rmakefile:2:3: deps takes 'make' or 'gcc'"},
		{"rule a from b\n  var x ?= 1\n", "Rmakefile:2:9: '?=' is not allowed"},
		{"task t(a b)\n", `Rmakefile:1:6: invalid parameter name "a b"`},
		{"task t foo\n", `Rmakefile:1:8: expected 'needs'`},
		{"if a == b\nelse\nelse\nend\n", "Rmakefile:3:1: expected end, found else"},
	}
	for _, tt := range tests {
		_, err := Parse("Rmakefile", []byte(tt.src))
		if err == nil || !strings.HasPrefix(err.Error(), tt.want) {
			t.Errorf("%q:\n got %v\nwant %s...", tt.src, err, tt.want)
		}
	}
}

func TestIndentation(t *testing.T) {
	// Inside an if, statements may be indented; a rule's attributes must be
	// indented deeper than the rule header.
	src := `if $os == linux
  rule a from b
    run cp $in $out
  var x = 1
end
`
	f := mustParse(t, src)
	body := f.Stmts[0].(*If).Then
	if len(body) != 2 {
		t.Fatalf("got %d statements in if, want 2", len(body))
	}
	if n := len(body[0].(*Rule).Attrs); n != 1 {
		t.Errorf("rule has %d attrs, want 1", n)
	}
}

func TestFormat(t *testing.T) {
	src := `# header comment
var a = 1
var  b   +=   2 3   # trailing

var c ?= glob src/*.c
rule out/x from in/x | dep   # rule
    # about run
    run cp $in $out
	desc "copy"
    always
task t ( a,b ) needs out/x
  run echo $a $b
if defined a
var d = 1
else
  # else comment
var d = 2
end
# trailing comment
`
	want := `# header comment
var a = 1
var b += 2 3 # trailing

var c ?= glob src/*.c

rule out/x from in/x | dep # rule
  # about run
  run    cp $in $out
  desc   "copy"
  always

task t(a, b) needs out/x
  run echo $a $b

if defined a
  var d = 1
else
  # else comment
  var d = 2
end

# trailing comment
`
	got := string(Format(mustParse(t, src)))
	if got != want {
		t.Errorf("format:\n%s\nwant:\n%s", got, want)
	}
	if again := string(Format(mustParse(t, got))); again != got {
		t.Errorf("format not idempotent:\n%s", again)
	}
}

func TestRoundTripExample(t *testing.T) {
	once := Format(mustParse(t, example))
	twice := Format(mustParse(t, string(once)))
	if string(once) != string(twice) {
		t.Errorf("not idempotent:\n%s\n---\n%s", once, twice)
	}
}

func FuzzParse(f *testing.F) {
	f.Add(example)
	f.Add("rule a b from c | d\n  run x \"y z\" ${q:%=a%}\n")
	f.Add("if defined x\n  task t(a, b)\n    sh \"echo $a\"\nend\n")
	f.Fuzz(func(t *testing.T, src string) {
		ast, err := Parse("f", []byte(src))
		if err != nil {
			return
		}
		out := Format(ast)
		ast2, err := Parse("f", out)
		if err != nil {
			t.Fatalf("formatted output does not parse: %v\n%s", err, out)
		}
		if out2 := Format(ast2); string(out2) != string(out) {
			t.Fatalf("format not idempotent:\n%s\n---\n%s", out, out2)
		}
	})
}
