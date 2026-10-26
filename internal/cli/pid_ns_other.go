//go:build !linux

package cli

import "errors"

// initPIDNamespace has no meaning off Linux; nothing compares against it
// because currentPIDNamespace never succeeds here.
const initPIDNamespace uint64 = 0

// currentPIDNamespace is unavailable off Linux. Records are then written
// without a pid_ns and judged by the pid probe as before (BUG-4090).
func currentPIDNamespace() (uint64, error) {
	return 0, errors.New("pid namespaces are a Linux concept")
}
