//go:build linux

package cli

import (
	"os"
	"strconv"
)

// pidIsSelfOrAncestor reports whether pid is this process or one of its
// ancestors, walking /proc/<pid>/stat's ppid field (via procStatFields,
// the parser shared with the start-token reader). It is
// how a registration's CLAUDE_PID / PAD_SESSION_PID claim is checked on
// Linux (TASK-2767, codex round 3): a `pad session register` run by a
// harness — as a hook, a monitor, or a tool shell — is a DESCENDANT of
// that harness's session process, so a claimed session pid that is not
// in the ancestry is a claim about some other process. It does not make
// the claim proof of much else (init is everyone's ancestor), but it
// refutes the case the registry exists to prevent: naming a sibling
// session's pid as one's own.
func pidIsSelfOrAncestor(pid int) (bool, error) {
	cur := os.Getpid()
	for depth := 0; depth < 128 && cur > 0; depth++ {
		if cur == pid {
			return true, nil
		}
		parent, err := procParentPID(cur)
		if err != nil {
			return false, err
		}
		if parent == cur || parent <= 0 {
			return false, nil
		}
		cur = parent
	}
	return false, nil
}

func procParentPID(pid int) (int, error) {
	fields, err := procStatFields(pid)
	if err != nil {
		return 0, err
	}
	const ppidIndexAfterComm = 1 // field 4: state(3), ppid(4)
	if len(fields) <= ppidIndexAfterComm {
		return 0, errProcStatMalformed
	}
	return strconv.Atoi(fields[ppidIndexAfterComm])
}
