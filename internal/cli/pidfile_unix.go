//go:build unix

package cli

import (
	"log/slog"
	"os"
	"syscall"
	"time"
)

// holdPIDFile takes an exclusive advisory lock on the PID file and keeps it for
// the process's lifetime. The lock — not the pid, and not the timestamp beside
// it — is what `pad server stop` uses to tell OUR server from a stranger that
// inherited the pid (BUG-2969).
//
// Why a lock rather than comparing the pid's start time: the lock answers the
// question directly ("does a live pad server hold THIS file"), where a
// timestamp comparison infers identity from an attribute of a pid. It is also
// one implementation for Linux and macOS, where reading another process's start
// time is two — /proc on Linux, a sysctl on macOS. Same primitive, same
// package: session_lock_unix.go has used flock for the sessions dir since
// TASK-2767.
//
// The returned release closes the descriptor, which drops the lock. It does not
// remove the file; the caller owns that ordering.
func holdPIDFile(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}

	// Retry briefly rather than failing on the first refusal. The only holder
	// a bound server can legitimately meet here is `pad server stop`'s own
	// ownership probe, which holds the lock for microseconds; giving up
	// immediately would let that instant cost a starting server its claim and
	// leave it unaddressable for its whole life. Bounded, so a genuinely stuck
	// holder degrades to the documented no-PID-file path instead of hanging
	// the start.
	var lockErr error
	for attempt := 0; attempt < 20; attempt++ {
		lockErr = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if lockErr == nil {
			return func() { _ = f.Close() }, nil
		}
		if lockErr != syscall.EWOULDBLOCK && lockErr != syscall.EAGAIN {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = f.Close()
	return nil, lockErr
}

// pidFileOwner reports whether a live pad server holds path.
//
// It probes the same lock non-blockingly. Acquiring it proves nobody holds it —
// the writer is gone, so the record is stale whatever the pid now names.
// Failing to acquire it proves a live process holds THIS file, and the only
// thing that takes this lock is a pad server that wrote this record.
//
// The probe's own lock is released immediately: it is a question, not a claim.
func pidFileOwner(path string) (pidRecord, pidFileOwnership) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		// The file vanished. Nothing to own.
		return pidRecord{}, pidFileStale
	}
	defer f.Close()

	lockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)

	// Read the record from the SAME descriptor whose lock state we just
	// probed, and read it AFTER probing (codex round 1). Taking the pid from
	// an earlier, separate read leaves a window in which a successor claims
	// the file between the two: the lock then reports "held" — truthfully,
	// about the SUCCESSOR — while the pid handed back is the predecessor's,
	// and stop signals a process that no longer owns anything.
	rec, ok := parsePIDRecord(readAll(f))

	if lockErr != nil {
		// EWOULDBLOCK — someone holds it. Any other error is a filesystem we
		// cannot lock on (some network mounts), which is unprovable rather
		// than owned: we will not signal on a lock we could not evaluate.
		if lockErr == syscall.EWOULDBLOCK || lockErr == syscall.EAGAIN {
			if !ok {
				// Held, but the contents are unreadable — a claim caught
				// mid-write. Unprovable, so nothing is signalled.
				return pidRecord{}, pidFileUnprovable
			}
			return rec, pidFileOurs
		}
		return rec, pidFileUnprovable
	}
	// Stale. Remove the file WHILE STILL HOLDING the lock (codex round 2):
	// releasing first leaves a window in which a replacement server claims the
	// path, and the remove then deletes ITS live record — the same
	// unaddressable-server bug this fix exists to close, reintroduced by the
	// cleanup. A claimant that arrives during this instant retries (see
	// holdPIDFile) rather than losing its claim.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		slog.Warn("could not remove stale PID file", "path", path, "error", err)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return rec, pidFileStale
}

// processStartTime records when this process began, for the human-readable
// half of the record. Unix does not compare it — the lock is the discriminator
// — so wall-clock now, taken at write time, is honest enough for a reader.
func processStartTime(pid int) time.Time {
	return time.Now().UTC()
}

// processIsGone reports whether pid has exited. Signal 0 delivers nothing and
// only performs the existence and permission checks; EPERM means it is alive
// and not ours to signal, which is emphatically NOT gone.
func processIsGone(pid int) bool {
	if pid <= 0 {
		return true
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return false
	}
	return err != syscall.EPERM
}
