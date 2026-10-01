//go:build unix

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestDBRestorePreservesServiceOwnership(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("changing the database to a different service owner requires root")
	}
	dir := t.TempDir()
	live, backup := filepath.Join(dir, "pad.db"), filepath.Join(dir, "backup.db")
	writeRestoreSQLite(t, live, "original row", 4096, false)
	writeRestoreSQLite(t, backup, "restored row", 4096, false)
	if err := os.Chown(live, 99, 99); err != nil {
		t.Fatal(err)
	}
	setSQLiteRestoreEnv(t, dir)
	if err := executeSQLiteRestore(t, backup); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(live)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if stat.Uid != 99 || stat.Gid != 99 || info.Mode().Perm() != 0o600 {
		t.Fatalf("restored owner/group/mode = %d/%d/%o", stat.Uid, stat.Gid, info.Mode().Perm())
	}
	assertRestoreSQLiteValue(t, live, "restored row")
}
