//go:build darwin

package handoff

import (
	"golang.org/x/sys/unix"
)

func isLocalVolume(path string) bool {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return false
	}
	return stat.Flags&unix.MNT_LOCAL != 0
}
