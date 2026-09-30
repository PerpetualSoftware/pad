package materialize

import (
	"os/exec"
	"syscall"
)

// setParentDeathSignal asks the kernel to SIGKILL the worker when its parent
// dies.
//
// The Go caveat (golang/go#27505): PR_SET_PDEATHSIG fires when the THREAD
// that forked the child exits, not the process, and Go may run the fork on
// any thread. The runtime never retires an idle thread; the one case where it
// ends a thread is a goroutine that exits while locked to it
// (runtime.LockOSThread). spawn therefore runs cmd.Start on a FRESH goroutine
// (startUnlocked), which is never locked whatever its caller is, and a thread
// that is running a locked goroutine runs nothing else, so the fork cannot
// land on a thread that will be retired. And if the parent is already gone by
// the time the child sets the flag, Go's forkExec re-checks getppid and
// signals the child itself.
func setParentDeathSignal(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
