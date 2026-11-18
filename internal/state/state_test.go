package state

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func mustOpen(t *testing.T, dir string) Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir)
	full := &Rule{
		ID: "r1", Outs: []string{"x", "y"}, Key: "k1", Cmds: []string{`["cc"]`}, Tools: []string{"/bin/cc"},
		Env: map[string]string{"a": "1"}, Inputs: map[string]string{"in": "h"},
		Discovered: map[string]string{"d.h": "h2"}, OutHashes: map[string]string{"x": "h3"},
		Duration: 12, Reason: "no build record", Time: -5,
	}
	s.PutFile(File{Path: "a", Size: 1, Mtime: 2, Ino: 3, Hash: "h1", Recorded: 4})
	s.PutRule(&Rule{ID: "r1", Key: "old"})
	s.PutRule(&Rule{ID: "r2", Key: "k2"})
	s.PutRule(full)
	s.DeleteRule("r2")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = mustOpen(t, dir)
	defer s.Close()
	if f, ok := s.File("a"); !ok || f != (File{Path: "a", Size: 1, Mtime: 2, Ino: 3, Hash: "h1", Recorded: 4}) {
		t.Errorf("file: %+v %v", f, ok)
	}
	if r, ok := s.Rule("r1"); !ok || !reflect.DeepEqual(r, full) {
		t.Errorf("r1: %+v", r)
	}
	if _, ok := s.Rule("r2"); ok {
		t.Error("r2 should be deleted")
	}
}

func TestDamagedLog(t *testing.T) {
	for _, tc := range []struct {
		name   string
		damage func([]byte) []byte
		keep   []string // rules that survive
	}{
		{"torn tail", func(b []byte) []byte { return b[:len(b)-3] }, []string{"r1"}},
		{"flipped byte in last record", func(b []byte) []byte { b[len(b)-6] ^= 0xff; return b }, []string{"r1"}},
		{"garbage appended", func(b []byte) []byte { return append(b, 0x05, 0x01) }, []string{"r1", "r2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s := mustOpen(t, dir)
			s.PutRule(&Rule{ID: "r1"})
			s.Flush()
			s.PutRule(&Rule{ID: "r2"})
			s.Close()
			p := filepath.Join(dir, "log")
			b, _ := os.ReadFile(p)
			os.WriteFile(p, tc.damage(b), 0o644)

			s = mustOpen(t, dir)
			var got []string
			for _, r := range s.Rules() {
				got = append(got, r.ID)
			}
			if !reflect.DeepEqual(got, tc.keep) {
				t.Errorf("after damage: %v, want %v", got, tc.keep)
			}
			// New records appended after the damage must survive a reload.
			s.PutRule(&Rule{ID: "r3"})
			s.Close()
			s = mustOpen(t, dir)
			defer s.Close()
			if _, ok := s.Rule("r3"); !ok {
				t.Error("record appended after damage was lost")
			}
		})
	}
}

func TestUnknownFormat(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "log"), []byte(`{"rmake_state":1}`+"\n"), 0o644)
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "unknown state format") {
		t.Errorf("got %v", err)
	}
}

func TestCompaction(t *testing.T) {
	dir := t.TempDir()
	for i := range 5 {
		s := mustOpen(t, dir)
		for j := range 500 {
			s.PutRule(&Rule{ID: "same", Key: strings.Repeat("k", i+1)})
			s.PutFile(File{Path: "f", Size: int64(j)})
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	fi, _ := os.Stat(filepath.Join(dir, "log"))
	if fi.Size() > 200 {
		t.Errorf("log not compacted: %d bytes", fi.Size())
	}
	s := mustOpen(t, dir)
	defer s.Close()
	if r, _ := s.Rule("same"); r == nil || r.Key != "kkkkk" {
		t.Errorf("after compaction: %+v", r)
	}
}

func FuzzDecode(f *testing.F) {
	f.Add(appendRecord(nil, record{kind: kindRule, rule: &Rule{ID: "x", Outs: []string{"a"}, Inputs: map[string]string{"b": "c"}}}))
	f.Add(appendRecord(nil, record{kind: kindFile, file: &File{Path: "p"}}))
	f.Fuzz(func(t *testing.T, b []byte) {
		for len(b) > 0 {
			_, n, ok := nextRecord(b)
			if !ok {
				return
			}
			b = b[n:]
		}
	})
}
