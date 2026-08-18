//go:build linux

package cli

import (
	"fmt"
	"os"
	"strings"
)

// procStartToken returns a stable owner-identity token for a pid on
// Linux: the process's start time (field 22 of /proc/<pid>/stat, in clock
// ticks since boot). It is constant for the life of a process and differs
// for a reused pid, so comparing it defeats the pid-reuse hazard the
// headless arm-state fallback would otherwise have (Codex R1 HIGH-2). ok
// is false when the value can't be read, in which case the caller falls
// back to bare pid-liveness.
//
// The comm field (2) is wrapped in parentheses and may itself contain
// spaces or parentheses, so parsing starts after the LAST ')': the fields
// that follow are space-separated, and starttime is the 22nd overall —
// index 19 once pid and comm are dropped.
func procStartToken(pid int) (string, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", false
	}
	s := string(data)
	rparen := strings.LastIndexByte(s, ')')
	if rparen < 0 || rparen+1 >= len(s) {
		return "", false
	}
	fields := strings.Fields(s[rparen+1:])
	const startTimeIndexAfterComm = 19 // field 22 minus pid(1) and comm(2)
	if len(fields) <= startTimeIndexAfterComm {
		return "", false
	}
	return fields[startTimeIndexAfterComm], true
}
