//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package cli

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// touchCredentialsFile sets the save's open file's mtime to now (BUG-3838),
// through its descriptor only, so a path replaced or a symlink retargeted
// since the open cannot take the stamp instead (codex r3). There is no path
// fallback (codex r4): when the descriptor form fails (Linux's futimes
// resolves /proc/self/fd, so no /proc; EPERM on a foreign-owned file) the
// save relies on its truncate, which moves an empty file's mtime on Linux.
func touchCredentialsFile(f *os.File, _ string) error {
	now := time.Now()
	tv := []unix.Timeval{unix.NsecToTimeval(now.UnixNano()), unix.NsecToTimeval(now.UnixNano())}
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ferr error
	if err := rc.Control(func(fd uintptr) { ferr = unix.Futimes(int(fd), tv) }); err != nil {
		return err
	}
	return ferr
}
