//go:build windows

package cli

import (
	"time"

	"golang.org/x/sys/windows"
)

// holdPIDFile is a no-op on Windows: there is no flock in the pattern this
// package uses elsewhere (session_lock_other.go documents the same boundary).
// Ownership here is decided by the recorded creation time instead — see
// pidFileOwner — so the PID file needs no lock held across the process's life.
func holdPIDFile(path string) (release func(), err error) {
	return func() {}, nil
}

// pidFileOwner reports whether the pid in rec is still the process that wrote
// the record, by comparing the OS-reported creation time against the one
// recorded at start.
//
// Creation time is the discriminator that survives pid reuse: a reused pid
// belongs to a process that started later, so the timestamps differ. Compared
// exactly — both sides come from GetProcessTimes, so there is no clock skew to
// tolerate and a tolerance window would only admit a collision.
func pidFileOwner(path string) (pidRecord, pidFileOwnership) {
	rec, ok := readPIDRecord(path)
	if !ok {
		return pidRecord{}, pidFileStale
	}
	if rec.StartedAt.IsZero() {
		// A legacy bare-pid file, or one written before the fingerprint
		// existed. Unprovable — and unprovable means we do not signal.
		return rec, pidFileUnprovable
	}

	started := processStartTime(rec.PID)
	if started.IsZero() {
		// No such process (or no rights to ask). Nothing holds this record.
		return rec, pidFileStale
	}
	if started.Equal(rec.StartedAt) {
		return rec, pidFileOurs
	}
	// The pid is live but was created at a different moment: it is a REUSE,
	// not our server.
	return rec, pidFileStale
}

// Windows deliberately does NOT delete a stale PID file, where Unix removes it
// under the lock it already holds (codex round 3).
//
// There is no lock here to make check-then-remove atomic, so a successor that
// claims the path between the two would have its live record deleted and be
// unaddressable for the rest of its life. Weigh the two outcomes: a stale file
// left behind is overwritten by the next `pad server start`, which claims the
// path unconditionally, and until then `stop` reports it as stale and signals
// nothing. A wrongly deleted record has no such recovery. So the platform
// without an atomic primitive does the harmless half and leaves cleanup to the
// next writer.

// processStartTime returns pid's creation time, or the zero time when the
// process does not exist or cannot be opened.
func processStartTime(pid int) time.Time {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return time.Time{}
	}
	defer windows.CloseHandle(h)

	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return time.Time{}
	}
	return time.Unix(0, creation.Nanoseconds()).UTC()
}

// processIsGone reports whether pid has exited.
func processIsGone(pid int) bool {
	if pid <= 0 {
		return true
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return true
	}
	defer windows.CloseHandle(h)

	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	const stillActive = 259 // STILL_ACTIVE
	return code != stillActive
}
