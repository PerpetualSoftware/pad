package materialize

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"golang.org/x/sys/unix"
)

// Linux: RLIMIT_AS on the CHILD, RELATIVE to the loaded worker. Once the
// worker has loaded its bundle (the readiness probe has answered) and before
// it is sent any job, the parent reads the worker's address-space size
// (/proc/<pid>/stat vsize) and sets RLIMIT_AS to that baseline plus the
// limit with prlimit(2), then reads it back. The limit therefore bounds how
// much ADDRESS SPACE a job may add to the loaded worker, whatever the host's
// runtime reserved before it (an absolute cap measured on one machine failed
// a 64 MiB allocation on CI runners, whose baseline was larger).
//
// Not setrlimit in the parent before exec: that would cap the server. Not the
// child capping itself: the parent could not verify it.
//
// The window: the worker's own startup and bundle load run uncapped. That is
// trusted code; the worker holds no job until the cap is set and confirmed.
// If the baseline cannot be read, the job is refused (ErrNoMemoryCap).
//
// A breach fails the allocation and the Go runtime dies with "fatal error:
// runtime: out of memory", which the stderr log recognises.
var platformMemCap = memCapImpl{
	mechanism: "rlimit_as",
	attach: func(cmd *exec.Cmd) (*capHandle, error) {
		pid := cmd.Process.Pid
		return &capHandle{
			baseline: func() (uint64, error) { return procVsize(pid) },
			set: func(capBytes uint64) error {
				lim := unix.Rlimit{Cur: capBytes, Max: capBytes}
				if err := unix.Prlimit(pid, unix.RLIMIT_AS, &lim, nil); err != nil {
					return err
				}
				var got unix.Rlimit
				if err := unix.Prlimit(pid, unix.RLIMIT_AS, nil, &got); err != nil {
					return err
				}
				if got.Cur != capBytes {
					return fmt.Errorf("RLIMIT_AS reads %d after setting %d", got.Cur, capBytes)
				}
				return nil
			},
		}, nil
	},
}

// procVsize reads a process's virtual memory size in bytes: field 23 of
// /proc/<pid>/stat, counted after the parenthesised command name (which may
// itself hold spaces or parentheses).
func procVsize(pid int) (uint64, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	return parseStatVsize(string(b))
}
