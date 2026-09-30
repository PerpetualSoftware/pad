package materialize

import (
	"fmt"
	"os/exec"

	"golang.org/x/sys/unix"
)

// Linux: RLIMIT_AS on the CHILD, set from the parent with prlimit(2) on the
// child's pid right after Start.
//
// Not setrlimit in the parent before exec: the limit would cap the server
// too. Not the child capping itself from an env var: that relies on the
// child binary doing it, and the parent could not tell a child that skipped
// it (an older binary, a factory spawning something else) from one that did.
// prlimit is applied and checked by the parent, whatever the child is.
//
// The window: between Start returning (the child has exec'd) and prlimit, the
// child runs uncapped — its Go runtime init and the start of the bundle load,
// microseconds. It holds no job in that window: the parent writes its first
// byte to the child's stdin (the readiness probe) only after prlimit
// succeeds, and a prlimit failure kills the child. So no untrusted input is
// ever processed uncapped.
//
// RLIMIT_AS limits ADDRESS SPACE, not resident memory; see MinMemLimit. A
// breach fails the allocation and the Go runtime dies with "fatal error:
// runtime: out of memory", which the stderr log recognises.
var platformMemCap = memCapImpl{
	mechanism: "rlimit_as",
	apply: func(cmd *exec.Cmd, limit uint64) (func(), error) {
		lim := unix.Rlimit{Cur: limit, Max: limit}
		if err := unix.Prlimit(cmd.Process.Pid, unix.RLIMIT_AS, &lim, nil); err != nil {
			return nil, err
		}
		var got unix.Rlimit
		if err := unix.Prlimit(cmd.Process.Pid, unix.RLIMIT_AS, nil, &got); err != nil {
			return nil, err
		}
		if got.Cur != limit {
			return nil, fmt.Errorf("RLIMIT_AS reads %d after setting %d", got.Cur, limit)
		}
		return nil, nil
	},
}
