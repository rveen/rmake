//go:build unix

package jobserver

import (
	"strings"
	"testing"
	"time"
)

func TestServer(t *testing.T) {
	s, err := New(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if f := s.MakeFlags(); !strings.HasPrefix(f, " -j3 --jobserver-auth=fifo:/") {
		t.Errorf("MakeFlags = %q", f)
	}
	// Two tokens, then Acquire blocks until one is released.
	a, _ := s.Acquire()
	b, _ := s.Acquire()
	got := make(chan byte)
	go func() {
		c, err := s.Acquire()
		if err == nil {
			got <- c
		}
	}()
	select {
	case <-got:
		t.Fatal("a third token")
	case <-time.After(50 * time.Millisecond):
	}
	s.Release(a)
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("released token not acquired")
	}
	s.Release(b)
	// Close interrupts a blocked Acquire.
	s.Acquire()
	done := make(chan error)
	go func() {
		_, err := s.Acquire()
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	s.Close()
	select {
	case err := <-done:
		if err != ErrClosed {
			t.Errorf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not interrupt Acquire")
	}
}
