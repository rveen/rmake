package hash

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rveen/rmake/internal/state"
)

func open(t *testing.T) state.Store {
	t.Helper()
	s, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestHashAndCache(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "f")
	os.WriteFile(p, []byte("hello"), 0o644)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(p, old, old)

	st := open(t)
	c := New(root, st)
	h1, ok, err := c.Hash("f")
	if err != nil || !ok || h1 != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("hash: %s %v %v", h1, ok, err)
	}
	// A new cache over the same store reuses the entry without reading.
	c2 := New(root, st)
	if h, _, _ := c2.Hash("f"); h != h1 || c2.Hashed != 0 {
		t.Errorf("cache not reused: hashed=%d", c2.Hashed)
	}
	// Same size and mtime but different content, while the entry is racy:
	// must be detected.
	now := time.Now()
	os.WriteFile(p, []byte("HELLO"), 0o644)
	os.Chtimes(p, now, now)
	c3 := New(root, st)
	h3, _, _ := c3.Hash("f")
	os.WriteFile(p, []byte("hullo"), 0o644)
	os.Chtimes(p, now, now)
	c4 := New(root, st)
	h4, _, _ := c4.Hash("f")
	if h3 == h1 || h4 == h3 {
		t.Errorf("racy change not detected: %s %s %s", h1, h3, h4)
	}

	if _, ok, err := c.Hash("missing"); ok || err != nil {
		t.Errorf("missing: %v %v", ok, err)
	}
}

func TestComputePut(t *testing.T) {
	root := t.TempDir()
	st := open(t)
	c := New(root, st)
	os.WriteFile(filepath.Join(root, "out"), []byte("one"), 0o644)
	h1, _, _ := c.Hash("out")
	// Rewritten behind the memo; Compute does not touch the cache ...
	os.WriteFile(filepath.Join(root, "out"), []byte("two"), 0o644)
	e, err := c.Compute("out")
	if err != nil {
		t.Fatal(err)
	}
	if h, _, _ := c.Hash("out"); h != h1 {
		t.Error("Compute changed the cache")
	}
	// ... Put replaces the entry.
	h2, ok := c.Put(e)
	if !ok || h2 == h1 {
		t.Errorf("put: %s %v", h2, ok)
	}
	if h, _, _ := c.Hash("out"); h != h2 {
		t.Error("Put did not update the memo")
	}
	if f, ok := st.File("out"); !ok || f.Hash != h2 {
		t.Error("Put did not update the store")
	}
	e, err = c.Compute("missing")
	if _, ok := c.Put(e); err != nil || ok {
		t.Errorf("missing: %v %v", ok, err)
	}
}
