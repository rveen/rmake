// Package hash computes file content hashes (SHA-256) with a cache keyed on
// file metadata (README §4.3).
//
// A cached hash is reused when size, mtime and inode match the file on disk,
// unless the entry is "racy": the file's mtime is within 2 s of when the hash
// was recorded, so the file may have changed again within the timestamp's
// granularity. Racy entries are rehashed.
package hash

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/rveen/rmake/internal/state"
)

// RacyWindow is the racy-clean interval.
const RacyWindow = 2 * time.Second

// Cache hashes files relative to a root directory.
type Cache struct {
	root  string
	store state.Store
	now   func() time.Time
	// memo holds hashes known in this run. Within a run only rmake's own
	// rules write files, and they call Forget first, so an entry stays
	// valid until then.
	memo map[string]string
	// stats holds results of Prestat, valid until Forget.
	stats map[string]statResult
	// Hashed counts files actually read (for tests and statistics).
	Hashed int
}

type statResult struct {
	fi  fs.FileInfo
	err error
}

type meta struct {
	size  int64
	mtime int64
	ino   uint64
}

// New returns a cache that persists entries in store.
func New(root string, store state.Store) *Cache {
	return &Cache{root: root, store: store, now: time.Now, memo: map[string]string{}, stats: map[string]statResult{}}
}

func (c *Cache) abs(p string) string {
	if len(p) > 0 && p[0] == '/' {
		return p
	}
	return c.root + "/" + p
}

// Hash returns the hex SHA-256 of the file at root-relative path p. ok is
// false if the file does not exist (or is a directory).
func (c *Cache) Hash(p string) (h string, ok bool, err error) {
	if h, ok := c.memo[p]; ok {
		return h, true, nil
	}
	fi, err := c.stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if fi.IsDir() {
		return "", false, nil
	}
	m := meta{size: fi.Size(), mtime: fi.ModTime().UnixNano(), ino: inode(fi)}
	if e, ok := c.store.File(p); ok && e.Size == m.size && e.Mtime == m.mtime && e.Ino == m.ino &&
		!racy(e.Mtime, e.Recorded) {
		c.memo[p] = e.Hash
		return e.Hash, true, nil
	}
	recorded := c.now().UnixNano()
	h, err = hashFile(c.abs(p))
	if err != nil {
		return "", false, err
	}
	c.Hashed++
	c.memo[p] = h
	c.store.PutFile(state.File{Path: p, Size: m.size, Mtime: m.mtime, Ino: m.ino, Hash: h, Recorded: recorded})
	return h, true, nil
}

// Forget drops what is known about p in this run; call it before p is
// rewritten.
func (c *Cache) Forget(p string) {
	delete(c.memo, p)
	delete(c.stats, p)
}

func (c *Cache) stat(p string) (fs.FileInfo, error) {
	if st, ok := c.stats[p]; ok {
		return st.fi, st.err
	}
	return os.Stat(c.abs(p))
}

// Exists reports whether p exists and is not a directory.
func (c *Cache) Exists(p string) bool {
	if _, ok := c.memo[p]; ok {
		return true
	}
	fi, err := c.stat(p)
	return err == nil && !fi.IsDir()
}

// Prestat stats paths concurrently, so that the following Exists and Hash
// calls do not pay for the system calls one at a time. Results stay valid
// until Forget.
func (c *Cache) Prestat(all []string) {
	var paths []string
	for _, p := range all {
		if _, ok := c.stats[p]; !ok {
			paths = append(paths, p)
		}
	}
	if len(paths) < 64 {
		return
	}
	workers := min(4*runtime.GOMAXPROCS(0), 32)
	results := make([]statResult, len(paths))
	var wg sync.WaitGroup
	next := make(chan int, 256)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				fi, err := os.Stat(c.abs(paths[i]))
				results[i] = statResult{fi, err}
			}
		}()
	}
	for i := range paths {
		next <- i
	}
	close(next)
	wg.Wait()
	for i, p := range paths {
		c.stats[p] = results[i]
	}
}

func racy(mtime, recorded int64) bool {
	d := recorded - mtime
	if d < 0 {
		d = -d
	}
	return d < int64(RacyWindow)
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
