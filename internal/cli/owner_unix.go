//go:build unix

package cli

import (
	"os"
	"syscall"
)

// ownerUID returns the uid that owns path, and false when it cannot be read.
func ownerUID(path string) (int, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(stat.Uid), true
}
