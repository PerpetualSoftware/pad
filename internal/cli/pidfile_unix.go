//go:build unix

package cli

import (
	"fmt"
	"log/slog"
	"os"
	"syscall"
	"time"
)

// pidProbeAfterLockHook is a test seam called after the probe's lock attempt,
// whatever its result, and before the inode check.
var pidProbeAfterLockHook func(path string)

// pidClaimAfterLockHook is a test seam called once a claim holds its first lock
// on the file at path, before its record is written.
var pidClaimAfterLockHook func(path string)

// claimPIDFile writes rec to the PID file and takes an exclusive advisory lock
// on it, kept for the process's lifetime. The lock — not the pid, and not the
// timestamp beside it — is what `pad server stop` uses to tell OUR server from a
// stranger that inherited the pid (BUG-2969).
// The lock is held on the inode the path names at the moment of the claim:
// locking an unlinked predecessor would leave the new record unlocked (BUG-3647).
//
// The two lock modes split the file's life (BUG-4899). The record is written
// under a SHARED lock and the exclusive one is taken only afterwards, so an
// exclusive holder always has its own record in the file, and that is the only
// state a probe reads as ours. Taking the exclusive lock first and then writing
// left an instant in which the lock was held over a crashed predecessor's
// record, and a stop in that instant signalled the predecessor's pid. Probes
// take the shared lock too (see pidFileOwner), so they never look like an owner
// to each other either.
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
func claimPIDFile(path string, rec pidRecord) (release func(), err error) {
	data, err := marshalPIDRecord(rec)
	if err != nil {
		return nil, err
	}
	// Retry briefly rather than failing on the first refusal. The holders a
	// bound server can legitimately meet here are `pad server stop`'s own
	// ownership probe, which holds its lock for microseconds, and a previous
	// server on its way out; giving up immediately would let that instant cost
	// a starting server its claim and leave it unaddressable for its whole
	// life. Bounded, so a genuinely stuck holder degrades to the documented
	// no-PID-file path instead of hanging the start.
	var lockErr error
	for attempt := 0; attempt < 20; attempt++ {
		if attempt > 0 {
			time.Sleep(25 * time.Millisecond)
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return nil, err
		}
		// Shared first: it waits out a live owner's exclusive lock, and it
		// keeps every probe from removing the file while the record is being
		// written (a probe removes only under the exclusive lock).
		lockErr = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
		if lockErr != nil {
			_ = f.Close()
			if lockErr != syscall.EWOULDBLOCK && lockErr != syscall.EAGAIN {
				return nil, lockErr
			}
			continue
		}
		if !namesFile(path, f) {
			_ = f.Close()
			lockErr = fmt.Errorf("PID file %s kept changing while being locked", path)
			continue
		}
		if pidClaimAfterLockHook != nil {
			pidClaimAfterLockHook(path)
		}
		if err := writeRecordTo(f, data); err != nil {
			_ = f.Close()
			return nil, err
		}
		// Now exclusive. Converting a flock is not atomic: the shared lock is
		// dropped before the exclusive one is granted, and a refused attempt
		// leaves neither. In that gap a probe may find the file free, read it
		// as stale and remove it, so the inode is checked again once the
		// exclusive lock is held, and a removed file means starting over.
		lockErr = upgradeLock(f)
		if lockErr != nil {
			_ = f.Close()
			if lockErr != syscall.EWOULDBLOCK && lockErr != syscall.EAGAIN {
				return nil, lockErr
			}
			continue
		}
		if !namesFile(path, f) {
			_ = f.Close()
			lockErr = fmt.Errorf("PID file %s was removed while being claimed", path)
			continue
		}
		// A probe holding the shared lock beside ours may have emptied the
		// file before removing it. Nobody else can hold a lock now, so put
		// the record back if it is not ours.
		if string(readAll(f)) != string(data) {
			if err := writeRecordTo(f, data); err != nil {
				_ = f.Close()
				return nil, err
			}
		}
		return func() { _ = f.Close() }, nil
	}
	return nil, lockErr
}

// upgradeLock converts a held shared lock on f to an exclusive one, retrying
// briefly past the short shared holds a probe takes.
func upgradeLock(f *os.File) error {
	var err error
	for attempt := 0; attempt < 20; attempt++ {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			return err
		}
		time.Sleep(5 * time.Millisecond)
	}
	return err
}

// namesFile reports whether path still names the file f has open.
func namesFile(path string, f *os.File) bool {
	fi, fileErr := f.Stat()
	pi, pathErr := os.Stat(path)
	return fileErr == nil && pathErr == nil && os.SameFile(fi, pi)
}

// writeRecordTo replaces the contents of f with data through the descriptor,
// so the write lands on the inode whose lock is held, whatever path names.
func writeRecordTo(f *os.File, data []byte) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	_, err := f.WriteAt(data, 0)
	return err
}

// pidFileOwner reports whether a live pad server holds path.
//
// It probes with a SHARED lock, non-blockingly. Only a server holds the
// exclusive lock, and only after writing its record (see claimPIDFile), so a
// refused shared lock on the file at path proves that a live server holds THIS
// file and that the record in it is that server's. A granted one proves no
// server holds it: the record is stale whatever the pid now names, or it is a
// claim still being written, which is not yet anyone to signal either.
//
// A probe never holds the exclusive lock over a record it has not first
// emptied, so a second probe running beside it reads no stale pid as ours.
//
// The probe's own lock is released before it returns: it is a question, not a
// claim.
func pidFileOwner(path string) (pidRecord, pidFileOwnership) {
	for attempt := 0; attempt < 5; attempt++ {
		f, err := os.OpenFile(path, os.O_RDWR, 0o644)
		if err != nil {
			// The file vanished. Nothing to own.
			return pidRecord{}, pidFileStale
		}

		lockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
		held := lockErr == syscall.EWOULDBLOCK || lockErr == syscall.EAGAIN
		if pidProbeAfterLockHook != nil {
			pidProbeAfterLockHook(path)
		}
		if lockErr == nil || held {
			// Either answer is about the file we opened, so it must still be
			// the file at path. A free lock on an unlinked predecessor would
			// license removing a successor's record; a held one could be an
			// owner's lock on a file it has just removed on its way down.
			if !namesFile(path, f) {
				if lockErr == nil {
					_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				}
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
			// EWOULDBLOCK — a server holds it. Any other error is a filesystem
			// we cannot lock on (some network mounts), which is unprovable
			// rather than owned: we will not signal on a lock we could not
			// evaluate.
			if held {
				if !ok {
					// Held, but the contents are unreadable: a server
					// rewriting its record, or a probe that emptied a stale
					// one and is removing it. Unprovable, so nothing is
					// signalled.
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
			// proof and falls through to the removal below. The kept case is
			// therefore never a trap: `pad server start` probes the port, not
			// this file, and its claim overwrites it; and stop's unprovable
			// refusal names the manual remedy (remove the file).
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			return rec, pidFileUnprovable
		}
		removeStale(path, f)
		return rec, pidFileStale
	}
	return pidRecord{}, pidFileUnprovable
}

// removeStale removes the stale PID file f, which the caller holds under a
// shared lock and has checked is the file at path, and drops that lock.
//
// The record is emptied first, while every holder is still shared: the
// exclusive lock this then takes must not sit over a readable record, or a
// probe beside this one would read it as a live server's (BUG-4899). A claim
// writing beside us has its record emptied too; it checks after taking its own
// exclusive lock and writes it again.
//
// The removal happens WHILE HOLDING the exclusive lock: only an exclusive holder
// removes the file at path, so with that lock held and the inode checked again,
// nothing can have replaced the path before the remove. A claimant waiting on
// the lock must reopen after this unlink (see claimPIDFile). When the exclusive
// lock is not granted at once, someone else holds the file, so it is left.
func removeStale(path string, f *os.File) {
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	if err := f.Truncate(0); err != nil {
		slog.Warn("could not empty stale PID file", "pid_file", path, "error", err)
		return
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return
	}
	if !namesFile(path, f) {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		slog.Warn("could not remove stale PID file", "pid_file", path, "error", err)
	}
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
