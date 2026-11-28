//go:build unix

package cli

import (
	"fmt"
	"log/slog"
	"os"
	"syscall"
	"time"
)

// pidProbeLockedHook is a test seam called after the probe acquires its lock.
var pidProbeLockedHook func(path string)

// holdPIDFile takes an exclusive advisory lock on the PID file and keeps it for
// the process's lifetime. The lock — not the pid, and not the timestamp beside
// it — is what `pad server stop` uses to tell OUR server from a stranger that
// inherited the pid (BUG-2969).
// The lock is held on the inode the path names at the moment of the claim:
// locking an unlinked predecessor would leave the new record unlocked (BUG-3647).
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
	// Retry briefly rather than failing on the first refusal. The only holder
	// a bound server can legitimately meet here is `pad server stop`'s own
	// ownership probe, which holds the lock for microseconds; giving up
	// immediately would let that instant cost a starting server its claim and
	// leave it unaddressable for its whole life. Bounded, so a genuinely stuck
	// holder degrades to the documented no-PID-file path instead of hanging
	// the start.
	var lockErr error
	for attempt := 0; attempt < 20; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return nil, err
		}
		lockErr = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if lockErr == nil {
			fi, fileErr := f.Stat()
			pi, pathErr := os.Stat(path)
			if fileErr == nil && pathErr == nil && os.SameFile(fi, pi) {
				return func() { _ = f.Close() }, nil
			}
			_ = f.Close()
			lockErr = fmt.Errorf("PID file %s kept changing while being locked", path)
			continue
		}
		_ = f.Close()
		if lockErr != syscall.EWOULDBLOCK && lockErr != syscall.EAGAIN {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	return nil, lockErr
}

// pidFileOwner reports whether a live pad server holds path.
//
// It probes the same lock non-blockingly. Acquiring it and verifying the inode
// at path proves nobody holds that record — the writer is gone, so a modern
// record is stale whatever the pid now names.
// Failing to acquire it proves a live process holds THIS file, and the only
// thing that takes this lock is a pad server that wrote this record.
//
// The probe's own lock is released immediately: it is a question, not a claim.
func pidFileOwner(path string) (pidRecord, pidFileOwnership) {
	for attempt := 0; attempt < 5; attempt++ {
		f, err := os.OpenFile(path, os.O_RDWR, 0o644)
		if err != nil {
			// The file vanished. Nothing to own.
			return pidRecord{}, pidFileStale
		}

		lockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if lockErr == nil {
			if pidProbeLockedHook != nil {
				pidProbeLockedHook(path)
			}
			fi, fileErr := f.Stat()
			pi, pathErr := os.Stat(path)
			if fileErr != nil || pathErr != nil || !os.SameFile(fi, pi) {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
				continue
			}
		}
		defer f.Close()

		// Read the record from the SAME descriptor whose lock state we just
		// probed, and read it AFTER probing. Taking the pid from
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
		if ok && rec.StartedAt.IsZero() && !processIsGone(rec.PID) {
			// A LEGACY record (BUG-2970): a bare pid, written by a build that
			// predates the lock (BUG-2969), so a free lock proves nothing about it
			// — its writer never took one, and may be a live server right now.
			// Unprovable, as Windows already answers for the same shape, and the
			// file is LEFT: deleting the record of a live server is the failure
			// BUG-2969 exists to prevent. Every record this build writes carries a
			// StartedAt, so only a pre-lock file reaches this branch.
			//
			// Only while its pid EXISTS. A pid with no process behind it cannot be
			// anyone's server, so a legacy record naming one is stale by the same
			// proof and falls through to the removal below, under this lock. The
			// kept case is therefore never a trap: `pad server start` probes the
			// port, not this file, and its claim overwrites it; and stop's
			// unprovable refusal names the manual remedy (remove the file).
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			return rec, pidFileUnprovable
		}
		// Stale. The inode check proved path names the file we locked. Remove it
		// WHILE STILL HOLDING that lock: releasing first would let a replacement
		// server claim the path before we delete its live record. A waiting
		// claimant must reopen after this unlink (see holdPIDFile).
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			slog.Warn("could not remove stale PID file", "pid_file", path, "error", err)
		}
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return rec, pidFileStale
	}
	return pidRecord{}, pidFileUnprovable
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
