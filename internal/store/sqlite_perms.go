package store

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
)

// isSQLiteFilesystemPath reports whether dbPath names an on-disk SQLite file
// rather than an in-memory database, empty path, or URI DSN.
func isSQLiteFilesystemPath(dbPath string) bool {
	return dbPath != "" && dbPath != ":memory:" && !strings.HasPrefix(dbPath, "file:")
}

// secureSQLiteFiles ensures a filesystem SQLite database and its sidecar files
// (-wal, -shm, -journal) are not readable or writable by group or other users.
//
// On Unix, when dbPath does not yet exist it is pre-created with mode 0600 so
// SQLite creates any -wal/-shm/-journal siblings with the same mode. Existing
// regular files with any group/other bits set are tightened via chmod; symlinks
// and non-regular files are left alone, and chmod failures warn without failing
// store startup. On Windows this is a no-op.
func secureSQLiteFiles(dbPath string) error {
	if runtime.GOOS == "windows" {
		return nil
	}

	f, err := os.OpenFile(dbPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		if cerr := f.Close(); cerr != nil {
			return fmt.Errorf("create database file: %w", cerr)
		}
	} else if !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create database file: %w", err)
	}

	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		p := dbPath + suffix
		info, err := os.Lstat(p)
		if err != nil {
			continue
		}
		mode := info.Mode()
		if !mode.IsRegular() {
			continue
		}
		perm := mode.Perm()
		if perm&0o077 == 0 {
			continue
		}
		if err := os.Chmod(p, perm&^0o077); err != nil {
			slog.Warn("failed to tighten sqlite file permissions",
				"path", p,
				"mode", fmt.Sprintf("%04o", perm),
				"error", err,
			)
		}
	}
	return nil
}
