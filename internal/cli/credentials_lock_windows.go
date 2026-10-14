//go:build windows

package cli

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockCredentialsFile takes an exclusive LockFileEx lock on lockPath, which
// UpdateStore holds across a credentials load-modify-save (BUG-3649).
func lockCredentialsFile(lockPath string) (unlock func(), err error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	h := windows.Handle(f.Fd())
	ol := new(windows.Overlapped)
	if err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, ol); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = windows.UnlockFileEx(h, 0, 1, 0, ol)
		_ = f.Close()
	}, nil
}
