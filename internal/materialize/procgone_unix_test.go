//go:build unix

package materialize

import (
	"errors"
	"fmt"
	"syscall"
)

// processGone reports nil once pid no longer exists (the supervisor reaps
// what it kills, so a killed worker leaves no zombie either).
func processGone(pid int) error {
	err := syscall.Kill(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return fmt.Errorf("pid %d still exists (kill 0: %v)", pid, err)
}
