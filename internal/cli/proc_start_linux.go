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
// is false when the value can't be read, in which case the caller treats
// the owner as unverifiable and fails closed.
//
// A ZOMBIE (state 'Z') reports ok=false even though its /proc entry and
// start time still exist: the arming process has exited and is only
// awaiting reap, so its consent is dead. Without this a defunct arm
// command would keep a headless session armed until its parent reaped it
// (Codex R2 finding 3).
//
// The comm field (2) is wrapped in parentheses and may itself contain
// spaces or parentheses, so parsing starts after the LAST ')': the fields
// that follow are space-separated. State is the 3rd field overall (index
// 0 after comm) and starttime is the 22nd (index 19 after comm).
func procStartToken(pid int) (string, bool) {
	tok, err := procStartTokenErr(pid)
	return tok, err == nil
}

// procStartTokenErr is procStartToken with the failure preserved, so a
// caller that must tell "this process is gone" (os.ErrNotExist, errProcZombie)
// from "this process could not be examined" (a permission or I/O error —
// hidepid mounts, a namespace boundary) can do so. The registry's liveness
// verdict needs that distinction (TASK-2767, codex round 1 P2): a probe that
// failed is not proof the owner is gone.
func procStartTokenErr(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	s := string(data)
	rparen := strings.LastIndexByte(s, ')')
	if rparen < 0 || rparen+1 >= len(s) {
		return "", errProcStatMalformed
	}
	fields := strings.Fields(s[rparen+1:])
	const (
		stateIndexAfterComm     = 0  // field 3
		startTimeIndexAfterComm = 19 // field 22, minus pid(1) and comm(2)
	)
	if len(fields) <= startTimeIndexAfterComm {
		return "", errProcStatMalformed
	}
	if fields[stateIndexAfterComm] == "Z" {
		return "", errProcZombie // the process has exited and awaits reap
	}
	return fields[startTimeIndexAfterComm], nil
}
