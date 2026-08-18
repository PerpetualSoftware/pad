//go:build unix

package cli

import (
	"os"
	"syscall"
)

// statIdentity returns the inode and device of a stat'd file on unix,
// which together identify a specific filesystem node — a recreated node
// at the same path gets a new inode. Used to bind an arm-state file to
// the exact socket instance it was armed for (see armStateOwnerAlive).
// ok is false when the underlying stat isn't a *syscall.Stat_t.
func statIdentity(info os.FileInfo) (ino, dev uint64, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0, 0, false
	}
	return st.Ino, uint64(st.Dev), true
}
