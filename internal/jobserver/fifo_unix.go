//go:build unix

package jobserver

import (
	"os"
	"syscall"
)

// openFIFO (re)creates a FIFO and opens it for reading and writing, so
// that it never sees EOF and writes never block on a missing reader. The
// descriptor is non-blocking (os.OpenFile puts FIFOs in the poller), so
// Close interrupts a blocked Read.
func openFIFO(path string) (*os.File, error) {
	os.Remove(path)
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_RDWR, 0)
}
