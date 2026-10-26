//go:build !windows

package testtmp

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// deadPid returns the pid of a child that has already exited.
func deadPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skip("cannot run true:", err)
	}
	return cmd.Process.Pid
}

// lockedDir builds root/name the way mkdirOwned does, in its order: take
// the flock in a staging directory, then rename into place. The returned
// file holds the lock; closing it plays the owner exiting.
func lockedDir(t *testing.T, root, name string) (string, *os.File) {
	t.Helper()
	staging, err := os.MkdirTemp(root, ".staging-*")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(staging, LockName), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, name)
	if err := os.Rename(staging, path); err != nil {
		t.Fatal(err)
	}
	return path, f
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// BUG-4088: the pid in the name is not the owner. A sweeper in another PID
// namespace reads every live host pid as dead, which is what a dead pid is
// here; the held lock must keep the directory anyway, and its release must
// free it.
func TestSweepKeepsALockedDirWhosePidIsDead(t *testing.T) {
	root := t.TempDir()
	const prefix = "pad-x-template-"
	path, lock := lockedDir(t, root, prefix+strconv.Itoa(deadPid(t))+"-1")

	Sweep(root, prefix)
	if !exists(path) {
		t.Fatal("swept a directory whose owner.lock is held")
	}

	_ = lock.Close()
	Sweep(root, prefix)
	if exists(path) {
		t.Fatal("kept a directory whose owner.lock is free")
	}
}

// The other direction: a pid recorded in another namespace can name an
// unrelated live process here. With an owner.lock that is free, the live
// pid must not keep the directory.
func TestSweepRemovesAnUnlockedDirWhosePidIsAlive(t *testing.T) {
	root := t.TempDir()
	const prefix = "pad-x-template-"
	path, lock := lockedDir(t, root, prefix+strconv.Itoa(os.Getppid())+"-1")
	_ = lock.Close()

	Sweep(root, prefix)
	if exists(path) {
		t.Fatal("kept a directory whose owner.lock is free because its pid is running")
	}
}

// A directory with no owner.lock was made before BUG-4088 and keeps the pid
// rule (lead ruling (a)). So a lockless directory whose pid names some live,
// unrelated process stays: by design, the leak direction is closed only for
// directories that carry a lock. Pid 1 always exists.
func TestSweepKeepsALocklessDirWhosePidIsAlive(t *testing.T) {
	root := t.TempDir()
	const prefix = "pad-x-template-"
	path := filepath.Join(root, prefix+"1-1")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}

	Sweep(root, prefix)
	if !exists(path) {
		t.Fatal("swept a lockless directory whose pid is running")
	}
}

// The lock fd must not leak into children. If it did, a child outliving the
// test binary would hold the lock and keep the directory forever.
func TestOwnerLockIsNotInheritedByChildren(t *testing.T) {
	root := t.TempDir()
	path, lock, err := mkdirOwned(root, "pad-x-template-")
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Skip("cannot run sleep:", err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })

	_ = lock.Close()
	state, release := ownerState(path)
	if state != ownerGone {
		t.Fatalf("ownerState = %v with the owner's fd closed and a child alive, want ownerGone (the child inherited the lock)", state)
	}
	release()
}

// MkdirTemp holds the lock for the life of the process. A *os.File nobody
// references is closed by its finalizer, which would release the lock at
// the next GC.
func TestMkdirTempHoldsTheLockAcrossGC(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	path, err := MkdirTemp("pad-testtmp-gc-")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		runtime.GC()
		time.Sleep(5 * time.Millisecond)
		if state, release := ownerState(path); state != ownerHeld {
			release()
			t.Fatalf("after GC %d the owner.lock is free (state %v): MkdirTemp dropped its lock", i+1, state)
		}
	}
}

// What MkdirTemp leaves behind: the named directory with a held owner.lock,
// and no staging directory.
func TestMkdirTempLeavesALockedDirAndNoStaging(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	const prefix = "pad-testtmp-shape-"
	path, err := MkdirTemp(prefix)
	if err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(path, LockName)) {
		t.Fatalf("%s has no %s", path, LockName)
	}
	if state, _ := ownerState(path); state != ownerHeld {
		t.Fatalf("ownerState = %v, want ownerHeld", state)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "staging") {
			t.Errorf("staging directory left behind: %s", e.Name())
		}
	}
}

// mkdirOwned's staging directory has no held lock yet, so a sweep in another
// process must never be able to match it: its name must not start with the
// prefix. Sweep skips this process's own pid, which is why this is checked
// on the name rather than by sweeping.
func TestStagingNameNeverMatchesTheSweepPrefix(t *testing.T) {
	for _, prefix := range []string{"pad-storetest-template-", "pad-store-template-", "x-"} {
		if p := stagingPattern(prefix); strings.HasPrefix(p, prefix) {
			t.Errorf("staging pattern %q starts with the sweep prefix %q", p, prefix)
		}
	}
}
