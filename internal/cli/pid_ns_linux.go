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

// namespacesSupported: on Linux every process is in some PID and mount
// namespace, so a namespace this process cannot identify (no procfs) is a
// possible boundary, not proof of none (BUG-4090 r3, F1/F2).
const namespacesSupported = true

// currentPIDNamespace returns the inode of this process's PID namespace,
// the identity behind `readlink /proc/self/ns/pid` ("pid:[<inode>]"). A
// pid means something only inside one namespace, so two records — or a
// record and its reader — agree about a pid only when this agrees.
func currentPIDNamespace() (uint64, error) { return currentNamespace("pid") }

// currentMountNamespace is the same for the mount namespace: a path, and
// so a socket's absence or identity, means something only to a reader
// with the recorder's view of the filesystem (BUG-4090 r1, H1/H3).
func currentMountNamespace() (uint64, error) { return currentNamespace("mnt") }

func currentNamespace(kind string) (uint64, error) {
	info, err := os.Stat("/proc/self/ns/" + kind)
	if err != nil {
		return 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Ino == 0 {
		return 0, errors.New(kind + " namespace identity unavailable")
	}
	return st.Ino, nil
}
