// Package state stores what rmake knows between runs (README §8): file hash
// cache entries and rule records.
//
// The store is an append-only binary log, .rmake/log (format in codec.go).
// The last record per key wins. Writes are buffered and appended (with
// fsync) by Flush. The log is valid up to the first incomplete or damaged
// record; anything after it is discarded, which is always safe because lost
// state only causes rebuilds. The log is compacted when it holds more than
// twice as many records as are live.
package state

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// File is a hash cache entry.
type File struct {
	Path     string
	Size     int64
	Mtime    int64 // ns
	Ino      uint64
	Hash     string
	Recorded int64 // ns, when the hash was computed
}

// Rule is the record of the last successful run of a rule. Besides the key,
// it keeps the key's components so rmake can explain why a rule reruns.
type Rule struct {
	ID         string
	Outs       []string
	Key        string
	Cmds       []string
	Tools      []string          // resolved argv[0] per command
	Env        map[string]string // pass'ed variables
	Inputs     map[string]string // declared input -> hash
	Discovered map[string]string // depfile input -> hash
	OutHashes  map[string]string
	Duration   int64 // milliseconds
	Reason     string
	Time       int64 // unix ns, end of the run
}

// Store is the state store. Implementations need not be safe for concurrent
// use.
type Store interface {
	File(path string) (File, bool)
	PutFile(File)
	Rule(id string) (*Rule, bool)
	PutRule(*Rule)
	DeleteRule(id string)
	Rules() []*Rule
	// Flush makes all changes durable.
	Flush() error
	// Close flushes and releases the store's lock.
	Close() error
}

type logStore struct {
	path    string
	files   map[string]File
	rules   map[string]*Rule
	pending []record
	records int // records in the log file
	unlock  func()

	size     int64 // bytes in the log file
	validEnd int64 // end of the valid prefix; the rest is cut off on append
}

// Open opens (creating if needed) the store in dir, normally ".rmake". It
// takes an exclusive lock, so concurrent rmake runs in the same project wait
// for each other.
func Open(dir string) (Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	unlock, err := lock(filepath.Join(dir, "lock"))
	if err != nil {
		return nil, err
	}
	s := &logStore{
		path:   filepath.Join(dir, "log"),
		files:  map[string]File{},
		rules:  map[string]*Rule{},
		unlock: unlock,
	}
	if err := s.load(); err != nil {
		unlock()
		return nil, err
	}
	return s, nil
}

func (s *logStore) load() error {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	s.size = int64(len(data))
	if !bytes.HasPrefix(data, []byte(magic)) {
		if len(data) < len(magic) && bytes.HasPrefix([]byte(magic), data) {
			return nil // torn header: start over
		}
		return fmt.Errorf("%s: unknown state format (delete the .rmake directory to start over)", s.path)
	}
	off := len(magic)
	for off < len(data) {
		r, n, ok := nextRecord(data[off:])
		if !ok {
			break
		}
		s.apply(r)
		s.records++
		off += n
	}
	s.validEnd = int64(off)
	return nil
}

func (s *logStore) apply(r record) {
	switch r.kind {
	case kindFile:
		s.files[r.file.Path] = *r.file
	case kindRule:
		s.rules[r.rule.ID] = r.rule
	case kindDelRule:
		delete(s.rules, r.id)
	}
}

func (s *logStore) File(path string) (File, bool) {
	f, ok := s.files[path]
	return f, ok
}

func (s *logStore) PutFile(f File) {
	if old, ok := s.files[f.Path]; ok && old == f {
		return
	}
	s.files[f.Path] = f
	s.pending = append(s.pending, record{kind: kindFile, file: &f})
}

func (s *logStore) Rule(id string) (*Rule, bool) {
	r, ok := s.rules[id]
	return r, ok
}

func (s *logStore) PutRule(r *Rule) {
	s.rules[r.ID] = r
	s.pending = append(s.pending, record{kind: kindRule, rule: r})
}

func (s *logStore) DeleteRule(id string) {
	if _, ok := s.rules[id]; !ok {
		return
	}
	delete(s.rules, id)
	s.pending = append(s.pending, record{kind: kindDelRule, id: id})
}

func (s *logStore) Rules() []*Rule {
	out := make([]*Rule, 0, len(s.rules))
	for _, r := range s.rules {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *logStore) Flush() error {
	if len(s.pending) == 0 {
		return nil
	}
	total := s.records + len(s.pending)
	if total > 2*(len(s.files)+len(s.rules)) && total > 1000 {
		return s.compact()
	}
	var b []byte
	if s.validEnd == 0 {
		b = append(b, magic...)
	}
	for _, r := range s.pending {
		b = appendRecord(b, r)
	}
	if s.validEnd < s.size {
		if err := os.Truncate(s.path, s.validEnd); err != nil {
			return err
		}
	}
	if err := appendSync(s.path, b); err != nil {
		return err
	}
	s.records = total
	s.pending = nil
	s.size = s.validEnd + int64(len(b))
	s.validEnd = s.size
	return nil
}

func appendSync(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// compact rewrites the log with only live records, atomically.
func (s *logStore) compact() error {
	b := []byte(magic)
	paths := make([]string, 0, len(s.files))
	for p := range s.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		f := s.files[p]
		b = appendRecord(b, record{kind: kindFile, file: &f})
	}
	for _, r := range s.Rules() {
		b = appendRecord(b, record{kind: kindRule, rule: r})
	}
	tmp := s.path + ".tmp"
	os.Remove(tmp)
	if err := appendSync(tmp, b); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.records = len(s.files) + len(s.rules)
	s.pending = nil
	s.size, s.validEnd = int64(len(b)), int64(len(b))
	return nil
}

func (s *logStore) Close() error {
	err := s.Flush()
	if s.unlock != nil {
		s.unlock()
		s.unlock = nil
	}
	return err
}
