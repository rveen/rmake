package sched

import (
	"strings"
	"testing"
)

func TestProgressPlain(t *testing.T) {
	var b strings.Builder
	p := &progress{w: &b, total: 4, running: map[int]string{}}
	p.skip()
	a := p.start("compile a")
	c := p.start("compile c")
	p.finish(c, "compile c", []byte("warning c"))
	p.finish(a, "compile a", nil)
	d := p.start("link")
	p.finish(d, "link", []byte("linked\n"))
	want := "[1/3] compile a\n[2/3] compile c\nwarning c\n[3/3] link\nlinked\n"
	if b.String() != want {
		t.Errorf("got\n%q\nwant\n%q", b.String(), want)
	}

	// Output of a job whose header is not the last line gets the header
	// again.
	b.Reset()
	p = &progress{w: &b, total: 2, running: map[int]string{}}
	a = p.start("a")
	c = p.start("c")
	p.finish(a, "a", []byte("out a\n"))
	p.finish(c, "c", nil)
	want = "[1/2] a\n[2/2] c\n[1/2] a\nout a\n"
	if b.String() != want {
		t.Errorf("got\n%q\nwant\n%q", b.String(), want)
	}
}

func TestProgressTTY(t *testing.T) {
	var b strings.Builder
	p := &progress{w: &b, tty: true, width: 12, total: 3, running: map[int]string{}}
	a := p.start("compile a long name")
	c := p.start("c")
	p.finish(c, "c", []byte("warning\n"))
	p.finish(a, "compile a long name", nil)
	d := p.start("link")
	p.finish(d, "link", []byte("linked\n"))
	p.end()
	want := "\r[0/3] compi\x1b[K" + // started a (cut to width-1)
		"\r[0/3] c\x1b[K" + // started c
		"\r\x1b[K[2/3] c\nwarning\n" + // c's output above the status line
		"\r[1/3] compi\x1b[K" + // a still runs
		"\r[2/3] compi\x1b[K" + // a done
		"\r[2/3] link\x1b[K" + // link started
		"\r\x1b[K[3/3] link\nlinked\n" // nothing runs any more: no status line
	if b.String() != want {
		t.Errorf("got\n%q\nwant\n%q", b.String(), want)
	}
}
