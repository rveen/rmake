//go:build unix

package hash

import (
	"io/fs"
	"syscall"
)

func inode(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}
