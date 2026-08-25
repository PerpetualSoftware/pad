//go:build !linux

package cli

import "errors"

var errAncestryUnsupported = errors.New("process ancestry unsupported on this platform")

// pidIsSelfOrAncestor cannot walk the process tree portably here, so a
// harness pid claim is recorded UNVERIFIED (session_pid_verified=false)
// on this platform — the documented residual; consumers that need a
// verified claim read that field.
func pidIsSelfOrAncestor(pid int) (bool, error) {
	return false, errAncestryUnsupported
}
