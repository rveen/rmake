package sched

import (
	"fmt"
	"io"
	"unicode/utf8"
)

// progress prints what the build does (README §7.3). It is used only by the
// scheduler goroutine.
//
// The total counts the planned steps not yet known to be up to date: a step
// decided up to date, or skipped after a failure, leaves the total. Once the
// build ends, the total is the number of steps that ran.
//
// Not on a terminal, every job prints "[n/N] desc" when it starts, and its
// captured output when it finishes (repeating the header first if other
// lines came in between). On a terminal, one status line "[done/N] desc"
// is overwritten in place, and only jobs with output print a header and
// their output, above the status line.
type progress struct {
	w       io.Writer
	tty     bool
	width   int
	total   int
	started int
	done    int
	last    int            // not TTY: number of the job whose header was printed last
	running map[int]string // TTY: descs of running jobs by number
	latest  string         // TTY: desc on the status line
	shown   bool           // TTY: a status line is on the screen
}

func (p *progress) header(n int, desc string) {
	fmt.Fprintf(p.w, "[%d/%d] %s\n", n, p.total, desc)
	p.last = n
}

// start announces a job and returns its number.
func (p *progress) start(desc string) int {
	p.started++
	n := p.started
	if p.tty {
		p.running[n] = desc
		p.latest = desc
		p.draw()
	} else {
		p.header(n, desc)
	}
	return n
}

// finish reports the end of job n with its output.
func (p *progress) finish(n int, desc string, out []byte) {
	p.done++
	if p.tty {
		delete(p.running, n)
		// Show the most recently started job still running.
		best := 0
		for k, d := range p.running {
			if k > best {
				best, p.latest = k, d
			}
		}
		if len(out) > 0 {
			p.clear()
			p.header(n, desc)
			p.write(out)
			if len(p.running) == 0 {
				return // the header just printed says it all
			}
		}
		p.draw()
		return
	}
	if len(out) > 0 {
		if p.last != n {
			p.header(n, desc)
		}
		p.write(out)
	}
}

// skip removes a step that will not run from the total.
func (p *progress) skip() { p.total-- }

// message prints a message that belongs to no job.
func (p *progress) message(s string) {
	p.clear()
	p.write([]byte(s))
	p.last = 0
	if p.started > p.done {
		p.draw()
	}
}

func (p *progress) write(b []byte) {
	p.w.Write(b)
	if len(b) > 0 && b[len(b)-1] != '\n' {
		io.WriteString(p.w, "\n")
	}
}

func (p *progress) draw() {
	if !p.tty {
		return
	}
	line := fmt.Sprintf("[%d/%d] %s", p.done, p.total, p.latest)
	if p.width > 1 && utf8.RuneCountInString(line) > p.width-1 {
		r := []rune(line)
		line = string(r[:p.width-1])
	}
	fmt.Fprintf(p.w, "\r%s\x1b[K", line)
	p.shown = true
}

func (p *progress) clear() {
	if p.shown {
		io.WriteString(p.w, "\r\x1b[K")
		p.shown = false
	}
}

// end leaves the last status line on the screen.
func (p *progress) end() {
	if p.shown {
		io.WriteString(p.w, "\n")
		p.shown = false
	}
}
