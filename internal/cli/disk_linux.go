//go:build linux

package cli

import "syscall"

// freeBytes returns the space available to an unprivileged user on the
// filesystem holding dir, or 0 when it cannot be read. Only the fields the
// kernel guarantees on linux are used, so this stays in the standard library.
func freeBytes(dir string) uint64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return 0
	}

	return stat.Bavail * uint64(stat.Bsize)
}
