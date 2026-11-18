package glob

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestMatch(t *testing.T) {
	tests := []struct {
		pat, name string
		want      bool
	}{
		{"*.c", "a.c", true},
		{"*.c", "src/a.c", false},
		{"src/*.c", "src/a.c", true},
		{"**/*.c", "a.c", true},
		{"**/*.c", "x/y/a.c", true},
		{"src/**", "src/a/b", true},
		{"src/**/b", "src/b", true},
		{"src/**/b", "src/x/y/b", true},
		{"src/**/b", "src/x/y/c", false},
		{"a/**/**/b", "a/b", true},
		{"?.go", "ab.go", false},
		{"[ab].go", "b.go", true},
		{"[", "[", false},
		{"build/*.o", "build/a.o", true},
	}
	for _, tt := range tests {
		if got := Match(tt.pat, tt.name); got != tt.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tt.pat, tt.name, got, tt.want)
		}
	}
}

func touch(t *testing.T, root string, files ...string) {
	t.Helper()
	for _, f := range files {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDisk(t *testing.T) {
	root := t.TempDir()
	touch(t, root, "a.c", "b.h", "src/x.c", "src/sub/y.c", ".git/z.c", "src/.hid/w.c",
		"build/stale.c", ".hidden.c")
	skip := func(rel string) bool { return rel == "build" }
	tests := []struct {
		pat  string
		want []string
	}{
		{"*.c", []string{"a.c"}},
		{".*.c", []string{".hidden.c"}},
		{"src/*.c", []string{"src/x.c"}},
		{"**/*.c", []string{"a.c", "src/sub/y.c", "src/x.c"}},
		{"src/**", []string{"src/sub/y.c", "src/x.c"}},
		{"a.c", []string{"a.c"}},
		{"nope.c", nil},
		{"src", nil}, // directories are not matched
		{"*/*/*.c", []string{"src/sub/y.c"}},
		{"build/*.c", nil},
	}
	for _, tt := range tests {
		got := Disk(root, tt.pat, skip)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("Disk(%q) = %q, want %q", tt.pat, got, tt.want)
		}
	}
}

func FuzzMatch(f *testing.F) {
	f.Add("**/*.c", "a/b/c.c")
	f.Add("[a-", "x")
	f.Fuzz(func(t *testing.T, pat, name string) {
		Match(pat, name)
	})
}
