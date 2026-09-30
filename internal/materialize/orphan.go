package materialize

import (
	"os"
	"os/exec"
	"strconv"
)

// EnvParentPID carries the supervising process's pid to the worker, so the
// worker's orphan watch (ExitWhenOrphaned) knows which parent it must keep.
const EnvParentPID = "PAD_MATERIALIZE_PARENT_PID"

// guardAgainstOrphaning prepares cmd so the worker cannot outlive the
// supervising process. If the server dies mid-job (SIGKILL, the OOM killer,
// a crash), no deadline kill and no RSS watchdog will ever reach the worker
// again, so it would run on uncapped (macOS) or unbounded in time.
//
//   - Every Unix: the worker polls its parent pid (ExitWhenOrphaned) against
//     the pid passed here, and exits when it changes. The only layer on
//     macOS.
//   - Linux, in addition: Pdeathsig (setParentDeathSignal), so the kernel
//     sends SIGKILL at once.
//   - Windows: the Job Object's KILL_ON_JOB_CLOSE (memcap_windows.go).
func guardAgainstOrphaning(cmd *exec.Cmd) {
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	cmd.Env = append(env, EnvParentPID+"="+strconv.Itoa(os.Getpid()))
	setParentDeathSignal(cmd)
}
