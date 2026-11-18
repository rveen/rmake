//go:build !unix

package state

// lock is a no-op where advisory locks are not implemented yet (Windows is
// deferred, README §2).
func lock(string) (func(), error) { return func() {}, nil }
