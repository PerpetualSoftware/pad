//go:build unix

package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func createTestSQLiteDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "src.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open source sqlite: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE t(x); INSERT INTO t VALUES (1)"); err != nil {
		_ = db.Close()
		t.Fatalf("seed source sqlite: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close source sqlite: %v", err)
	}
	return path
}

func TestSQLiteBackupNewFileIsPrivate(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })

	src := createTestSQLiteDB(t)
	outDir := t.TempDir()
	out := filepath.Join(outDir, "backup.db")

	if err := backupSQLite(src, out); err != nil {
		t.Fatalf("backupSQLite failed: %v", err)
	}

	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("backup mode = %04o, want 0600", perm)
	}

	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("read destination dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "backup.db" {
		t.Errorf("destination dir entries = %v, want only backup.db", entries)
	}

	bdb, err := sql.Open("sqlite", out)
	if err != nil {
		t.Fatalf("open backup sqlite: %v", err)
	}
	defer bdb.Close()
	var got int
	if err := bdb.QueryRow("SELECT x FROM t").Scan(&got); err != nil {
		t.Fatalf("query backup sqlite: %v", err)
	}
	if got != 1 {
		t.Errorf("SELECT x FROM t = %d, want 1", got)
	}
}

func TestSQLiteBackupRefusesExisting(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })

	src := createTestSQLiteDB(t)
	outDir := t.TempDir()
	out := filepath.Join(outDir, "existing.db")

	if err := os.WriteFile(out, []byte("keep"), 0o644); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}
	if err := os.Chmod(out, 0o644); err != nil {
		t.Fatalf("chmod existing file: %v", err)
	}

	err := backupSQLite(src, out)
	if err == nil || !strings.Contains(err.Error(), "output file already exists") {
		t.Fatalf("backupSQLite error = %v, want error containing %q", err, "output file already exists")
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read existing file: %v", err)
	}
	if string(got) != "keep" {
		t.Errorf("existing content = %q, want %q", got, "keep")
	}

	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat existing file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("existing mode = %04o, want 0644", perm)
	}

	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("read destination dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("destination dir entries = %v, want 1 entry", entries)
	}
}

func TestPostgresBackupNewFileIsPrivate(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })

	setupFakePostgresDump(t)
	t.Setenv("FAKE_DUMP_CONTENT", "complete SQL\n")
	t.Setenv("FAKE_DUMP_STATUS", "0")

	outDir := t.TempDir()
	out := filepath.Join(outDir, "backup.sql")

	cmd := dbBackupCmd()
	cmd.SetArgs([]string{"--output", out})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("postgres backup failed: %v", err)
	}

	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("backup mode = %04o, want 0600", perm)
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(got) != "complete SQL\n" {
		t.Errorf("backup content = %q, want %q", got, "complete SQL\n")
	}
}

func TestPostgresBackupExistingKeepsMode(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })

	setupFakePostgresDump(t)
	t.Setenv("FAKE_DUMP_CONTENT", "complete SQL\n")
	t.Setenv("FAKE_DUMP_STATUS", "0")

	outDir := t.TempDir()
	out := filepath.Join(outDir, "backup.sql")
	if err := os.WriteFile(out, []byte("previous SQL\n"), 0o640); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}
	if err := os.Chmod(out, 0o640); err != nil {
		t.Fatalf("chmod existing file: %v", err)
	}

	cmd := dbBackupCmd()
	cmd.SetArgs([]string{"--output", out})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("postgres backup failed: %v", err)
	}

	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o640 {
		t.Errorf("existing backup mode = %04o, want 0640", perm)
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(got) != "complete SQL\n" {
		t.Errorf("backup content = %q, want %q", got, "complete SQL\n")
	}
}

// withoutHardLinks makes linkFile fail as it does on a filesystem with no hard
// links, after running racer (when set) to model a file appearing at the
// destination between the failed link and the fallback.
func withoutHardLinks(t *testing.T, racer func(dst string)) {
	t.Helper()
	orig := linkFile
	linkFile = func(_, dst string) error {
		if racer != nil {
			racer(dst)
		}
		return &os.LinkError{Op: "link", New: dst, Err: syscall.EPERM}
	}
	t.Cleanup(func() { linkFile = orig })
}

func TestSQLiteBackupCopyFallbackIsPrivate(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })
	withoutHardLinks(t, nil)

	src := createTestSQLiteDB(t)
	outDir := t.TempDir()
	out := filepath.Join(outDir, "backup.db")
	if err := backupSQLite(src, out); err != nil {
		t.Fatalf("backupSQLite failed: %v", err)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("backup mode = %04o, want 0600", perm)
	}
	if entries, err := os.ReadDir(outDir); err != nil || len(entries) != 1 {
		t.Errorf("destination dir entries = %v, error %v; want only backup.db", entries, err)
	}
	bdb, err := sql.Open("sqlite", out)
	if err != nil {
		t.Fatalf("open backup sqlite: %v", err)
	}
	defer bdb.Close()
	var got int
	if err := bdb.QueryRow("SELECT x FROM t").Scan(&got); err != nil || got != 1 {
		t.Errorf("SELECT x FROM t = %d, error %v; want 1", got, err)
	}
}

func TestSQLiteBackupCopyFallbackNeverOverwrites(t *testing.T) {
	withoutHardLinks(t, func(dst string) {
		if err := os.WriteFile(dst, []byte("racer"), 0o644); err != nil {
			t.Fatalf("racer write: %v", err)
		}
	})

	src := createTestSQLiteDB(t)
	outDir := t.TempDir()
	out := filepath.Join(outDir, "backup.db")
	err := backupSQLite(src, out)
	if err == nil || !strings.Contains(err.Error(), "output file already exists") {
		t.Fatalf("backupSQLite error = %v, want error containing %q", err, "output file already exists")
	}
	if got, err := os.ReadFile(out); err != nil || string(got) != "racer" {
		t.Errorf("destination = %q, error %v; want the racer's file untouched", got, err)
	}
	if entries, err := os.ReadDir(outDir); err != nil || len(entries) != 1 {
		t.Errorf("destination dir entries = %v, error %v; want 1 entry", entries, err)
	}
}
