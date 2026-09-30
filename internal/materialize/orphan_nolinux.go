//go:build !linux

package materialize

import "os/exec"

// setParentDeathSignal: Linux only; see orphan_linux.go.
func setParentDeathSignal(*exec.Cmd) {}
