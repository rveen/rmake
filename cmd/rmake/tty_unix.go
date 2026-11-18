//go:build unix

package main

import (
	"syscall"
	"unsafe"
)

// termWidth returns the width of the terminal on fd, or 0.
func termWidth(fd uintptr) int {
	var ws struct{ row, col, x, y uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if errno != 0 {
		return 0
	}
	return int(ws.col)
}
