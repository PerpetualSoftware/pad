//go:build linux

package cli

import (
	"errors"
	"os"
	"syscall"
)

// initPIDNamespace is the inode of the initial PID namespace. The kernel
// fixes it (PROC_PID_INIT_INO, 0xEFFFFFFC), so a reader can tell it is in
// the host's namespace without anyone having recorded it (BUG-4090).
const initPIDNamespace uint64 = 0xEFFFFFFC

// currentPIDNamespace returns the inode of this process's PID namespace,
// the identity behind `readlink /proc/self/ns/pid` ("pid:[<inode>]"). A
// pid means something only inside one namespace, so two records — or a
// record and its reader — agree about a pid only when this agrees.
func currentPIDNamespace() (uint64, error) {
	info, err := os.Stat("/proc/self/ns/pid")
	if err != nil {
		return 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Ino == 0 {
		return 0, errors.New("pid namespace identity unavailable")
	}
	return st.Ino, nil
}
