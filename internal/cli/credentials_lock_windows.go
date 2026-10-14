//go:build windows

package cli

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockCredentialsFile tries a LockFileEx lock on lockPath without blocking
// (LOCKFILE_FAIL_IMMEDIATELY, BUG-3649): exclusive (LOCKFILE_EXCLUSIVE_LOCK)
// for a write, shared (no exclusive flag) for a read. A lock another process
// holds is errCredentialsLockBusy.
func lockCredentialsFile(lockPath string, exclusive bool) (unlock func(), err error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	h := windows.Handle(f.Fd())
	ol := new(windows.Overlapped)
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if exclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	if err := windows.LockFileEx(h, flags, 0, 1, 0, ol); err != nil {
		_ = f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, errCredentialsLockBusy
		}
		return nil, err
	}
	return func() {
		_ = windows.UnlockFileEx(h, 0, 1, 0, ol)
		_ = f.Close()
	}, nil
}
