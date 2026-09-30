//go:build darwin || linux

package materialize

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
)

// psRSS reads a process's resident set size with `ps -o rss= -p <pid>`, the
// macOS watchdog's fallback when proc_info is unavailable. Built on Linux too
// so its parser and cost are tested where the tests run.
func psRSS(pid int) (uint64, error) {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	return parsePSRSS(out)
}

// parsePSRSS reads ps's rss column: KiB, one number, padded.
func parsePSRSS(out []byte) (uint64, error) {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return 0, errors.New("ps: no such process")
	}
	kib, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, err
	}
	return kib << 10, nil
}
