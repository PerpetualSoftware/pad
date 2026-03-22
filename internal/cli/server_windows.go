//go:build windows

package cli

import (
	"os"
	"os/exec"
)

func setSysProcAttr(cmd *exec.Cmd) {
	// No Setsid equivalent on Windows; process will detach naturally
	// when started without a console allocation.
}

func stopProcess(p *os.Process) error {
	// Windows doesn't support SIGTERM; use Kill instead.
	return p.Kill()
}
