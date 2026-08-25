//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"syscall"
)

// lockSessionsDir serializes registry mutations (register's write + prune,
// and a standalone prune) across processes with an advisory flock on
// ~/.pad/sessions/.lock, so a prune cannot delete a record that a
// concurrent register renamed into place between the prune's re-read and
// its os.Remove (TASK-2767, codex round 1 P1). The lock is held for the
// few milliseconds a mutation takes; readers (ListSessions) do not take
// it — an atomic-rename writer means they see a whole old or new file.
//
// One caveat flock cannot lift: the same process must not lock twice
// (a second LOCK_EX on a fresh descriptor blocks on its own first one), so
// RegisterSession and PruneSessions share pruneLocked rather than nesting.
func lockSessionsDir(dir string) (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(dir, sessionsLockName), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
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

// openRegistryFile opens a registry entry read-only without following a
// symlink at the final component and without blocking if the name has
// become a FIFO (O_NONBLOCK makes a FIFO open return immediately; the
// caller's regular-file check then rejects it).
func openRegistryFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
