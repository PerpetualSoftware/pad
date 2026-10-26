// Package testtmp makes the process-wide temp directories test binaries
// share (BUG-3412) without leaking them when a binary dies before its
// cleanup runs.
//
// A test binary killed by a timeout, a signal or the harness's memory reaper
// never reaches TestMain's Cleanup, so its template directory stayed in /tmp
// for good; /tmp is a tmpfs here, so every leaked template is RAM. Each new
// build therefore first removes the directories whose owner is gone.
//
// Ownership is a held flock on the directory's owner.lock (BUG-4088), not
// the pid in its name. A pid only means something inside one PID namespace
// while /tmp is shared across them: a sweeper in a sandbox (bwrap
// --unshare-pid) saw every host pid as dead and deleted live templates
// mid-run, and a pid recorded inside a sandbox could name an unrelated host
// process and keep a dead owner's directory forever. The kernel releases a
// flock when its holder exits, whatever namespace either side is in. The pid
// stays in the name for people, and for directories made before the lock
// existed, which keep the old pid rule.
package testtmp

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// LockName is the file inside every directory MkdirTemp makes whose flock
// the owner holds for as long as it runs. A caller that lists such a
// directory sees it beside its own files.
const LockName = "owner.lock"

// held keeps every owner.lock this process took open until it exits. A
// dropped *os.File is closed by its finalizer at the next GC, and closing it
// releases the flock, which would let any sweep remove a directory still in
// use.
var (
	heldMu sync.Mutex
	held   []*os.File
)

// MkdirTemp makes os.TempDir()/prefix<pid>-<random> after a sweep of earlier
// prefix directories whose owner is gone, and holds its owner.lock for the
// life of the process. prefix must end in "-".
func MkdirTemp(prefix string) (string, error) {
	dir := os.TempDir()
	Sweep(dir, prefix)
	path, lock, err := mkdirOwned(dir, prefix)
	if err != nil {
		return "", err
	}
	if lock != nil {
		heldMu.Lock()
		held = append(held, lock)
		heldMu.Unlock()
	}
	return path, nil
}

// Sweep removes dir/prefix<pid>-* directories owned by this user whose owner
// has exited: the directory's owner.lock can be taken, or, for a directory
// with no owner.lock (made before BUG-4088), its pid is not running. A
// directory without a pid (made before BUG-3412) is never removed: its age
// cannot prove its owner gone, since a stopped or suspended process can
// outlive any bound (codex r1). Errors are ignored: a sweep that cannot
// remove something leaves it as it was.
func Sweep(dir, prefix string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	self := os.Getpid()
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !strings.HasPrefix(name, prefix) {
			continue
		}
		path := filepath.Join(dir, name)
		info, err := e.Info()
		if err != nil || !ownedByMe(info) {
			continue
		}
		rest := name[len(prefix):]
		pidPart, _, hasDash := strings.Cut(rest, "-")
		pid, perr := strconv.Atoi(pidPart)
		if !hasDash || perr != nil || pid <= 0 {
			continue
		}
		if pid == self {
			continue
		}
		switch state, release := ownerState(path); state {
		case ownerHeld:
			continue
		case ownerGone:
			// Removed while the lock is still ours, then released.
			_ = os.RemoveAll(path)
			release()
		case ownerNoLock:
			if !processAlive(pid) {
				_ = os.RemoveAll(path)
			}
		}
	}
}

type ownership int

const (
	// ownerHeld: the owner.lock is held, or could not be tested; keep.
	ownerHeld ownership = iota
	// ownerGone: the owner.lock was free and is now held by the caller.
	ownerGone
	// ownerNoLock: there is no owner.lock; fall back to the pid.
	ownerNoLock
)

// stagingPattern is where mkdirOwned builds a directory before it holds the
// lock. It does not start with prefix, so no sweep can match it.
func stagingPattern(prefix string) string {
	return fmt.Sprintf(".%sstaging-%d-*", prefix, os.Getpid())
}
