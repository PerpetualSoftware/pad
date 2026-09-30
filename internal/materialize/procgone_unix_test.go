//go:build unix

package materialize

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
)

// processGone reports nil once pid no longer exists. The supervisor reaps
// what it kills; an ORPHAN is reaped by whoever adopted it, so a zombie
// (Linux /proc state Z) also counts as gone: it runs nothing.
func processGone(pid int) error {
	err := syscall.Kill(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if b, rerr := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); rerr == nil {
		if i := strings.LastIndexByte(string(b), ')'); i >= 0 && strings.HasPrefix(strings.TrimSpace(string(b[i+1:])), "Z") {
			return nil
		}
	}
	return fmt.Errorf("pid %d still exists (kill 0: %v)", pid, err)
}
