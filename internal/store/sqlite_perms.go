package store

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/fsperm"
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
		tightenSQLiteFile(dbPath + suffix)
	}
	return nil
}

// tightenSQLiteFile clears group and other bits on p when it is a regular
// file. It opens p without following symlinks and changes the mode through
// that descriptor, so a symlink swapped in after a check cannot redirect the
// chmod to another file. A missing path, a symlink or a non-regular file is
// left alone; a failed chmod logs a warning.
func tightenSQLiteFile(p string) {
	// Open only a file that needs tightening. Closing a descriptor drops every
	// POSIX lock this process holds on the file, including those of another
	// connection to the same database, so an already owner-only file (the
	// steady state after the first start) is never opened here.
	if pre, err := os.Lstat(p); err != nil || !pre.Mode().IsRegular() || pre.Mode().Perm()&0o077 == 0 {
		return
	}
	f, err := os.OpenFile(p, fsperm.NoFollowFile, 0)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	perm := info.Mode().Perm()
	if perm&0o077 == 0 {
		return
	}
	if err := f.Chmod(perm &^ 0o077); err != nil {
		slog.Warn("failed to tighten sqlite file permissions",
			"file", p,
			"mode", fmt.Sprintf("%04o", perm),
			"error", err,
		)
	}
}
