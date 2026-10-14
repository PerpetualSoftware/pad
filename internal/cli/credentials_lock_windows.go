//go:build windows

package cli

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockCredentialsFile takes a LockFileEx lock on lockPath (BUG-3649):
// exclusive (LOCKFILE_EXCLUSIVE_LOCK) for a write, shared (flags 0) for a
// read.
func lockCredentialsFile(lockPath string, exclusive bool) (unlock func(), err error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	h := windows.Handle(f.Fd())
	ol := new(windows.Overlapped)
	var flags uint32
	if exclusive {
		flags = windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	if err := windows.LockFileEx(h, flags, 0, 1, 0, ol); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = windows.UnlockFileEx(h, 0, 1, 0, ol)
		_ = f.Close()
	}, nil
}
