//go:build linux

package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// applyNoDump clears PR_SET_DUMPABLE so /proc/<pid>/environ becomes unreadable
// even to the same non-root user (BUG-3718).
func applyNoDump() {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		fmt.Fprintln(os.Stderr, "stub: prctl PR_SET_DUMPABLE:", err)
		os.Exit(4)
	}
}
