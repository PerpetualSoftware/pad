//go:build unix

package materialize

import (
	"os"
	"strconv"
	"time"
)

// OrphanPollInterval is how often the worker checks its parent.
const OrphanPollInterval = 200 * time.Millisecond

// ExitWhenOrphaned starts a goroutine that exits the process (status 70)
// as soon as its parent pid is no longer the supervising process: the parent
// died and the worker was reparented (to init, launchd or a subreaper). The
// expected parent is EnvParentPID when set, else the parent at the time of
// the call, so a worker whose parent died before this ran still exits.
//
// Called first thing by the worker command, before the bundle loads. It
// writes nothing: stdout is the protocol channel, and nobody reads stderr
// once the parent is gone.
func ExitWhenOrphaned() {
	want := os.Getppid()
	if v, err := strconv.Atoi(os.Getenv(EnvParentPID)); err == nil && v > 0 {
		want = v
	}
	check := func() {
		if os.Getppid() != want {
			os.Exit(70)
		}
	}
	check()
	go func() {
		for {
			time.Sleep(OrphanPollInterval)
			check()
		}
	}()
}
