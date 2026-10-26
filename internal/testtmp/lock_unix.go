//go:build !windows

package testtmp

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// mkdirOwned builds the directory under a staging name, takes the flock on
// its owner.lock, and only then renames it to prefix<pid>-<random>, so a
// sweep never sees a directory of that shape without a held lock. The flock
// belongs to the open file, which refers to the inode, so it survives the
// rename. os.OpenFile opens with O_CLOEXEC, so a child the test spawns does
// not inherit the lock and keep a dead owner's directory looking alive.
func mkdirOwned(dir, prefix string) (string, *os.File, error) {
	staging, err := os.MkdirTemp(dir, stagingPattern(prefix))
	if err != nil {
		return "", nil, err
	}
	fail := func(f *os.File, err error) (string, *os.File, error) {
		if f != nil {
			_ = f.Close()
		}
		_ = os.RemoveAll(staging)
		return "", nil, err
	}
	f, err := os.OpenFile(filepath.Join(staging, LockName), os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fail(nil, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fail(f, fmt.Errorf("lock %s: %w", f.Name(), err))
	}
	random := staging[strings.LastIndex(staging, "-")+1:]
	final := filepath.Join(dir, fmt.Sprintf("%s%d-%s", prefix, os.Getpid(), random))
	if err := os.Rename(staging, final); err != nil {
		return fail(f, err)
	}
	return final, f, nil
}

// ownerState tests path's owner.lock. When it is free the caller now holds
// it, and must call release after removing the directory.
func ownerState(path string) (ownership, func()) {
	f, err := os.OpenFile(filepath.Join(path, LockName), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ownerNoLock, nil
		}
		return ownerHeld, nil
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return ownerHeld, nil
	}
	return ownerGone, func() { _ = f.Close() }
}
