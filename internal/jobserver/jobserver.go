// Package jobserver implements the server side of GNU make's jobserver
// protocol, FIFO style (make 4.4, README §7.2): a named pipe holds one byte
// per job slot beyond the first. Every client (rmake's own scheduler,
// nested make, cargo, ninja) takes a byte before starting an extra job and
// writes it back when the job is done, so all of them share one -j budget.
package jobserver

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrClosed is returned by Acquire after Close.
var ErrClosed = errors.New("jobserver closed")

// Server is a jobserver for jobs slots.
type Server struct {
	path string
	jobs int
	f    *os.File
}

// New creates the FIFO dir/jobserver (dir must exist) holding jobs-1
// tokens.
func New(dir string, jobs int) (*Server, error) {
	if jobs < 2 {
		return nil, fmt.Errorf("jobserver: needs at least 2 jobs")
	}
	abs, err := filepath.Abs(filepath.Join(dir, "jobserver"))
	if err != nil {
		return nil, err
	}
	f, err := openFIFO(abs)
	if err != nil {
		return nil, err
	}
	s := &Server{path: abs, jobs: jobs, f: f}
	tokens := make([]byte, jobs-1)
	for i := range tokens {
		tokens[i] = '+'
	}
	if _, err := f.Write(tokens); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// MakeFlags is the MAKEFLAGS value that makes GNU make a client.
func (s *Server) MakeFlags() string {
	return fmt.Sprintf(" -j%d --jobserver-auth=fifo:%s", s.jobs, s.path)
}

// Acquire takes a token, blocking until one is free. It returns ErrClosed
// once the server is closed.
func (s *Server) Acquire() (byte, error) {
	var b [1]byte
	for {
		n, err := s.f.Read(b[:])
		if n == 1 {
			return b[0], nil
		}
		if err != nil {
			return 0, ErrClosed
		}
	}
}

// Release returns a token.
func (s *Server) Release(b byte) {
	s.f.Write([]byte{b})
}

// Close closes and removes the FIFO; a blocked Acquire returns ErrClosed.
func (s *Server) Close() error {
	err := s.f.Close()
	os.Remove(s.path)
	return err
}
