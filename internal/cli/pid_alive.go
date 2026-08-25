package cli

import (
	"errors"
	"os"
	"syscall"
)

// pidAlive reports whether a process with the given pid is currently
// running. It is the pid probe behind OwnerLiveness (session_owner.go) —
// the headless arm-state fallback and the session registry both — and
// errs toward "dead" on any uncertainty. That is the fail-closed
// direction for the consent gate (a session wrongly judged dead simply
// doesn't arm); the registry's pruner is shielded from it on the one
// platform where the probe cannot work at all, Windows, which
// OwnerLiveness reports as unknown before ever calling this.
//
// On Unix, os.FindProcess never fails, so liveness is probed with signal
// 0: a nil error means the process exists, EPERM means it exists but is
// owned by another user (still alive), and ESRCH (or anything else) means
// it is gone. On platforms where signalling is unsupported (Windows), the
// probe returns an error and this reports dead — acceptable, since the
// headless fallback is explicitly secondary to auto_arm there.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}
