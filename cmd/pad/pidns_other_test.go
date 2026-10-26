//go:build !linux

package main

import "testing"

// testPIDNamespace: there are no PID namespaces off Linux, so rows carry no
// stamp (BUG-4090).
func testPIDNamespace(*testing.T) uint64 { return 0 }

// inInitPIDNamespace: off Linux every reader judges by the pre-BUG-4090 rule.
func inInitPIDNamespace(*testing.T) bool { return true }
