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

func TestPostgresBackupPreservesGroup(t *testing.T) {
	setupFakePostgresDump(t)
	t.Setenv("FAKE_DUMP_CONTENT", "complete SQL\n")
	t.Setenv("FAKE_DUMP_STATUS", "0")
	path := filepath.Join(t.TempDir(), "backup.sql")
	if err := os.WriteFile(path, []byte("previous SQL\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	initial, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	baseline := initial.Sys().(*syscall.Stat_t)
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		groups = append(groups, int(baseline.Gid)+1)
	}
	group := int(baseline.Gid)
	for _, candidate := range groups {
		if candidate != group && os.Chown(path, -1, candidate) == nil {
			group = candidate
			break
		}
	}
	if group == int(baseline.Gid) {
		t.Skip("no alternate group available for the backup fixture")
	}
	cmd := dbBackupCmd()
	cmd.SetArgs([]string{"--output", path})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	got := info.Sys().(*syscall.Stat_t)
	if got.Uid != baseline.Uid || int(got.Gid) != group || info.Mode().Perm() != initial.Mode().Perm() {
		t.Fatalf("owner/group/mode = %d/%d/%o, want %d/%d/%o", got.Uid, got.Gid, info.Mode().Perm(), baseline.Uid, group, initial.Mode().Perm())
	}
}
