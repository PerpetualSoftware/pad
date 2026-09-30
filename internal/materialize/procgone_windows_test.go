package materialize

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// processGone reports nil once pid no longer names a live process. The
// supervisor waits on (and so closes its handle to) every child it kills, so
// a killed worker leaves no process object behind; one that is somehow still
// open must at least have exited.
func processGone(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil // no such process
		}
		return fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return fmt.Errorf("GetExitCodeProcess(%d): %w", pid, err)
	}
	const stillActive = 259
	if code == stillActive {
		return fmt.Errorf("pid %d is still running", pid)
	}
	return nil
}
