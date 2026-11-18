package depfile

import (
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"\n\n", nil},
		{"a.o: a.c a.h\n", []string{"a.c", "a.h"}},
		{"a.o: a.c", []string{"a.c"}},
		{"a.o:\n", nil},
		// gcc -MD output with continuations and -MP phony targets.
		{"build/a.o: src/a.c src/a.h \\\n  /usr/include/stdio.h\nsrc/a.h:\n/usr/include/stdio.h:\n",
			[]string{"src/a.c", "src/a.h", "/usr/include/stdio.h"}},
		// CRLF.
		{"a.o: a.c \\\r\n b.h\r\n", []string{"a.c", "b.h"}},
		// Escapes.
		{`a.o: my\ file.h dollar$$.h hash\#.h back\slash.h`, []string{"my file.h", "dollar$.h", "hash#.h", `back\slash.h`}},
		// Multiple targets and rules: union, first appearance order.
		{"a.o b.o: x.h y.h\nc.o: y.h z.h\n", []string{"x.h", "y.h", "z.h"}},
		// A colon not followed by space is part of a name.
		{"a.o: c:/x.h\n", []string{"c:/x.h"}},
		// Colon right before a continuation.
		{"a.o:\\\n a.c\n", []string{"a.c"}},
		// Comments.
		{"# generated\na.o: a.c # trailing\n", []string{"a.c"}},
		// Tabs.
		{"a.o:\ta.c\tb.h\n", []string{"a.c", "b.h"}},
	} {
		got, err := Parse([]byte(tc.in))
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Parse(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"a.o a.c\n", ": a.c\n", "a.o\nb.o: x\n"} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("Parse(%q): no error", bad)
		}
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"src/../src/a.h":       "src/a.h",
		"./a.h":                "a.h",
		"/proj/src/a.h":        "src/a.h",
		"/proj/./x/../a.h":     "a.h",
		"/usr/include/stdio.h": "/usr/include/stdio.h",
		"/project/a.h":         "/project/a.h",
		"/proj":                ".",
	} {
		if got := Normalize(in, "/proj"); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{"a.o: a.c \\\n b.h\n", `x: a\ b $$ \# c:\d`, "# c\n\n:", "a:\\\r\n"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, err := Parse([]byte(s))
		if err != nil {
			return
		}
		seen := map[string]bool{}
		for _, p := range got {
			if p == "" || seen[p] || strings.ContainsAny(p, "\n") {
				t.Fatalf("Parse(%q) = %q", s, got)
			}
			seen[p] = true
		}
	})
}
