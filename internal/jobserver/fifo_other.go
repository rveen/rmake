//go:build !unix

package jobserver

import (
	"errors"
	"os"
)

// The Windows semaphore jobserver is deferred (M8).
func openFIFO(path string) (*os.File, error) {
	return nil, errors.New("jobserver: not supported on this platform")
}
