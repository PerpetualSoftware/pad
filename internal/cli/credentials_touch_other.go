//go:build !windows && !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package cli

import (
	"os"
	"time"
)

// touchCredentialsFile sets the save's file's mtime to now (BUG-3838), by
// path: this platform has no descriptor form wired here.
func touchCredentialsFile(_ *os.File, path string) error {
	now := time.Now()
	return os.Chtimes(path, now, now)
}
