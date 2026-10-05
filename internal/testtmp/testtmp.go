// Package testtmp makes the process-wide temp directories test binaries
// share (BUG-3412) without leaking them when a binary dies before its
// cleanup runs.
//
// A test binary killed by a timeout, a signal or the harness's memory reaper
// never reaches TestMain's Cleanup, so its template directory stayed in /tmp
// for good; /tmp is a tmpfs here, so every leaked template is RAM. The
// directory name now carries the owning process id, and each new build first
// removes the directories of processes that are gone.
package testtmp

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// legacyMaxAge bounds how long a directory in the old, pid-less format is
// kept. Only a binary built before BUG-3412 creates one, and another
// worktree may still be running such a binary: a full suite takes close to
// an hour, and its template is read for the whole run. A day is far past
// any run, so a directory that old has no owner left.
const legacyMaxAge = 24 * time.Hour

// MkdirTemp is os.MkdirTemp("", prefix+"<pid>-*") after a sweep of earlier
// prefix directories whose process is dead. prefix must end in "-".
func MkdirTemp(prefix string) (string, error) {
	Sweep(os.TempDir(), prefix, time.Now())
	return os.MkdirTemp("", fmt.Sprintf("%s%d-*", prefix, os.Getpid()))
}

// Sweep removes dir/prefix* directories owned by this user whose process
// is no longer running, and pid-less ones older than legacyMaxAge. Errors
// are ignored: a sweep that cannot remove something leaves it as it was.
func Sweep(dir, prefix string, now time.Time) {
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
			// The old format, prefix + random digits: no owner to ask.
			if now.Sub(info.ModTime()) > legacyMaxAge {
				_ = os.RemoveAll(path)
			}
			continue
		}
		if pid == self || processAlive(pid) {
			continue
		}
		_ = os.RemoveAll(path)
	}
}
