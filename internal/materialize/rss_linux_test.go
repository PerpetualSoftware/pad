package materialize

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// testRSSSampler reads /proc/<pid>/statm's resident pages, so the macOS
// watchdog mechanism can be exercised on Linux.
var testRSSSampler = func(pid int) (uint64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0, fmt.Errorf("statm: %q", b)
	}
	pages, err := strconv.ParseUint(f[1], 10, 64)
	if err != nil {
		return 0, err
	}
	return pages * uint64(os.Getpagesize()), nil
}
