//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package cli

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// touchCredentialsFile sets the save's open file's mtime to now (BUG-3838),
// through its descriptor, so a path replaced or a symlink retargeted since the
// open cannot take the stamp instead (codex r3). Linux's futimes resolves
// /proc/self/fd; without /proc it falls back to the path, as before.
func touchCredentialsFile(f *os.File, path string) error {
	now := time.Now()
	tv := []unix.Timeval{unix.NsecToTimeval(now.UnixNano()), unix.NsecToTimeval(now.UnixNano())}
	rc, err := f.SyscallConn()
	if err == nil {
		var ferr error
		if cerr := rc.Control(func(fd uintptr) { ferr = unix.Futimes(int(fd), tv) }); cerr == nil && ferr == nil {
			return nil
		}
	}
	return os.Chtimes(path, now, now)
}
