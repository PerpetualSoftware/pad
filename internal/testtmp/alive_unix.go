//go:build !windows

package testtmp

import (
	"errors"
	"os"
	"syscall"
)

// processAlive reports whether pid names a running process. EPERM means it
// exists under another user, which counts as alive.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// ownedByMe reports whether this user owns the directory, so a sweep never
// touches another account's files in a shared temp dir.
func ownedByMe(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
