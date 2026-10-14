//go:build unix

package cli

import (
	"os"
	"syscall"
)

// lockCredentialsFile takes an advisory flock on lockPath (BUG-3649):
// exclusive (LOCK_EX) for a write, shared (LOCK_SH) for a read.
func lockCredentialsFile(lockPath string, exclusive bool) (unlock func(), err error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	// The mode on OpenFile applies only on creation (BUG-3648's class), so
	// an existing lock file is tightened here (Codex round 1). Best effort:
	// a lock file this user can write but does not own (another uid's file
	// with group or world write) refuses fchmod with EPERM, and the lock file
	// holds no secret, so that must not turn a working login into a failure
	// (bug-hunter pilot, lead ruling). A foreign-owned 0600 lock file, such
	// as one a sudo run creates, fails at the open above instead, and the
	// caller then proceeds unlocked (withCredentialsLock).
	_ = f.Chmod(0600)
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
