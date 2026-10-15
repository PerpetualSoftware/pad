//go:build !windows

package cli

import (
	"os"
	"time"
)

// touchCredentialsFile sets the save's file's mtime to now (BUG-3838). By
// path: Unix has no share modes for a second open to collide with.
func touchCredentialsFile(_ *os.File, path string) error {
	now := time.Now()
	return os.Chtimes(path, now, now)
}
