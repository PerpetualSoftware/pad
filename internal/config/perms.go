package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
)

// tightenDir ensures an existing directory has no group or other permission
// bits set (chmod 0700). Symlinks, non-directories and sticky or
// world-writable directories are left untouched.
// Chmod failures are logged as warnings and never fail config loading.
func tightenDir(path string) {
	if runtime.GOOS == "windows" {
		return
	}
	clean := filepath.Clean(path)
	info, err := os.Lstat(clean)
	if err != nil {
		return
	}
	mode := info.Mode()
	if mode&os.ModeSymlink != 0 || !mode.IsDir() {
		return
	}
	// A sticky or world-writable directory is shared by design; chmod 0700
	// would also clear its special bits and lock other users out.
	if mode&os.ModeSticky != 0 || mode.Perm()&0o002 != 0 {
		return
	}
	perm := mode.Perm()
	if perm&0o077 == 0 {
		return
	}
	if err := os.Chmod(clean, 0o700); err != nil {
		slog.Warn("failed to tighten directory permissions",
			"path", clean,
			"mode", fmt.Sprintf("%04o", perm),
			"error", err,
		)
	}
}
