//go:build !unix

package hash

import "io/fs"

// inode is not available; Windows file indexes are deferred (README §2).
func inode(fs.FileInfo) uint64 { return 0 }
