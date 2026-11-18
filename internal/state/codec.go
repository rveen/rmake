package state

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"sort"
)

// Binary log format (README §8):
//
//	file    = magic { record }
//	record  = uvarint(len(payload)) payload crc32le(payload)
//	payload = kind fields...
//
// Strings are uvarint length + bytes, lists are uvarint count + items, maps
// are uvarint count + sorted key/value strings, integers are varints.

const magic = "rmake-state\x00\x02\n"

const (
	kindFile    byte = 1
	kindRule    byte = 2
	kindDelRule byte = 3
)

type record struct {
	kind byte
	file *File
	rule *Rule
	id   string
}

var crcTable = crc32.MakeTable(crc32.Castagnoli)

type enc struct{ b []byte }

func (e *enc) uint(v uint64) { e.b = binary.AppendUvarint(e.b, v) }
func (e *enc) int(v int64)   { e.b = binary.AppendVarint(e.b, v) }
func (e *enc) str(s string)  { e.uint(uint64(len(s))); e.b = append(e.b, s...) }

func (e *enc) strs(ss []string) {
	e.uint(uint64(len(ss)))
	for _, s := range ss {
		e.str(s)
	}
}

func (e *enc) smap(m map[string]string) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	e.uint(uint64(len(keys)))
	for _, k := range keys {
		e.str(k)
		e.str(m[k])
	}
}

// appendRecord appends the framed encoding of r to b.
func appendRecord(b []byte, r record) []byte {
	e := enc{b: make([]byte, 0, 256)}
	e.b = append(e.b, r.kind)
	switch r.kind {
	case kindFile:
		f := r.file
		e.str(f.Path)
		e.int(f.Size)
		e.int(f.Mtime)
		e.uint(f.Ino)
		e.str(f.Hash)
		e.int(f.Recorded)
	case kindRule:
		x := r.rule
		e.str(x.ID)
		e.strs(x.Outs)
		e.str(x.Key)
		e.strs(x.Cmds)
		e.strs(x.Tools)
		e.smap(x.Env)
		e.smap(x.Inputs)
		e.smap(x.Discovered)
		e.smap(x.OutHashes)
		e.int(x.Duration)
		e.str(x.Reason)
		e.int(x.Time)
	case kindDelRule:
		e.str(r.id)
	}
	b = binary.AppendUvarint(b, uint64(len(e.b)))
	b = append(b, e.b...)
	return binary.LittleEndian.AppendUint32(b, crc32.Checksum(e.b, crcTable))
}

var errCorrupt = errors.New("corrupt record")

// nextRecord decodes the record at the start of b and returns it with its
// encoded length. ok is false if b does not start with a complete, intact
// record (a torn write or damage); the log is valid only up to that point.
func nextRecord(b []byte) (r record, size int, ok bool) {
	n, k := binary.Uvarint(b)
	if k <= 0 || n > uint64(len(b)) {
		return record{}, 0, false
	}
	size = k + int(n) + 4
	if size > len(b) {
		return record{}, 0, false
	}
	payload := b[k : k+int(n)]
	if crc32.Checksum(payload, crcTable) != binary.LittleEndian.Uint32(b[k+int(n):]) {
		return record{}, 0, false
	}
	r, err := decodePayload(payload)
	return r, size, err == nil
}

type dec struct {
	b   []byte
	err error
}

func (d *dec) uint() uint64 {
	if d.err != nil {
		return 0
	}
	v, k := binary.Uvarint(d.b)
	if k <= 0 {
		d.err = errCorrupt
		return 0
	}
	d.b = d.b[k:]
	return v
}

func (d *dec) int() int64 {
	if d.err != nil {
		return 0
	}
	v, k := binary.Varint(d.b)
	if k <= 0 {
		d.err = errCorrupt
		return 0
	}
	d.b = d.b[k:]
	return v
}

func (d *dec) str() string {
	n := d.uint()
	if d.err != nil {
		return ""
	}
	if n > uint64(len(d.b)) {
		d.err = errCorrupt
		return ""
	}
	s := string(d.b[:n])
	d.b = d.b[n:]
	return s
}

func (d *dec) strs() []string {
	n := d.uint()
	if d.err != nil || n > uint64(len(d.b)) {
		d.err = errCorrupt
		return nil
	}
	if n == 0 {
		return nil
	}
	out := make([]string, n)
	for i := range out {
		out[i] = d.str()
	}
	return out
}

func (d *dec) smap() map[string]string {
	n := d.uint()
	if d.err != nil || n > uint64(len(d.b)) {
		d.err = errCorrupt
		return nil
	}
	if n == 0 {
		return nil
	}
	m := make(map[string]string, n)
	for range n {
		k := d.str()
		m[k] = d.str()
	}
	return m
}

func decodePayload(p []byte) (record, error) {
	if len(p) == 0 {
		return record{}, errCorrupt
	}
	d := &dec{b: p[1:]}
	r := record{kind: p[0]}
	switch r.kind {
	case kindFile:
		r.file = &File{Path: d.str(), Size: d.int(), Mtime: d.int(), Ino: d.uint(), Hash: d.str(), Recorded: d.int()}
	case kindRule:
		r.rule = &Rule{
			ID: d.str(), Outs: d.strs(), Key: d.str(), Cmds: d.strs(), Tools: d.strs(),
			Env: d.smap(), Inputs: d.smap(), Discovered: d.smap(), OutHashes: d.smap(),
			Duration: d.int(), Reason: d.str(), Time: d.int(),
		}
	case kindDelRule:
		r.id = d.str()
	default:
		return record{}, errCorrupt
	}
	if d.err == nil && len(d.b) != 0 {
		d.err = errCorrupt
	}
	return r, d.err
}
