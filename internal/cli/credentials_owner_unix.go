//go:build unix

package cli

import (
	"os"
	"syscall"
)

// currentUID is os.Getuid, swappable so a test can simulate a foreign-owned
// credentials file without root privileges (BUG-3807).
var currentUID = os.Getuid

// foreignOwnedCredentials reports whether path or path+".lock" exists (following
// symlinks) and is owned by a uid other than currentUID() (BUG-3807).
func foreignOwnedCredentials(path string) bool {
	uid := currentUID()
	for _, p := range []string{path, path + ".lock"} {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			continue
		}
		if uid < 0 || st.Uid != uint32(uid) {
			return true
		}
	}
	return false
}
