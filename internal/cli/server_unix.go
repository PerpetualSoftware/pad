//go:build !windows

package cli

import (
	"os"
	"os/exec"
	"syscall"
)

func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
}

func stopProcess(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}
