//go:build unix

package cli

import (
	"os"
	"syscall"
)

// lockCredentialsFile takes an exclusive advisory flock on lockPath, which
// UpdateStore holds across a credentials load-modify-save (BUG-3649).
// Readers do not take it: Save's atomic rename gives them a whole file.
func lockCredentialsFile(lockPath string) (unlock func(), err error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	// The mode on OpenFile applies only on creation (BUG-3648's class), so
	// an existing lock file is tightened here (Codex round 1).
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
