//go:build !windows

package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func setUmask022(t *testing.T) {
	t.Helper()
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })
}

func TestNewSQLiteCreates0600Files(t *testing.T) {
	setUmask022(t)

	dbPath := filepath.Join(t.TempDir(), "pad.db")
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("New(%q): %v", dbPath, err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if _, err := s.DB().Exec(`CREATE TABLE _bug3715_perm_check (id INTEGER PRIMARY KEY); INSERT INTO _bug3715_perm_check (id) VALUES (1);`); err != nil {
		t.Fatalf("write to sqlite db: %v", err)
	}

	info, err := os.Lstat(dbPath)
	if err != nil {
		t.Fatalf("Lstat(%q): %v", dbPath, err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode(%q) = %04o, want 0600", dbPath, got)
	}

	for _, suffix := range []string{"-wal", "-shm"} {
		p := dbPath + suffix
		fi, err := os.Lstat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("Lstat(%q): %v", p, err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("mode(%q) = %04o, want 0600", p, got)
		}
	}
}

func TestNewSQLiteTightensPermissiveFiles(t *testing.T) {
	setUmask022(t)

	dbPath := filepath.Join(t.TempDir(), "existing.db")
	s1, err := New(dbPath)
	if err != nil {
		t.Fatalf("initial New(%q): %v", dbPath, err)
	}
	if _, err := s1.DB().Exec(`CREATE TABLE _bug3715_tighten (id INTEGER PRIMARY KEY); INSERT INTO _bug3715_tighten (id) VALUES (1);`); err != nil {
		_ = s1.Close()
		t.Fatalf("initial write: %v", err)
	}

	// Keep a second connection open while s1 closes so SQLite does not unlink
	// the -wal/-shm siblings on last-connection close, reproducing a pre-existing
	// permissive DB + WAL/SHM set on disk.
	keeper, err := sql.Open("sqlite", dbPath)
	if err != nil {
		_ = s1.Close()
		t.Fatalf("open keeper connection: %v", err)
	}
	if err := keeper.Ping(); err != nil {
		_ = keeper.Close()
		_ = s1.Close()
		t.Fatalf("ping keeper: %v", err)
	}
	t.Cleanup(func() { _ = keeper.Close() })

	if err := s1.Close(); err != nil {
		t.Fatalf("close initial store: %v", err)
	}

	paths := []string{dbPath, dbPath + "-wal", dbPath + "-shm"}
	for _, p := range paths {
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatalf("Chmod(%q, 0644): %v", p, err)
		}
		info, err := os.Lstat(p)
		if err != nil {
			t.Fatalf("Lstat(%q): %v", p, err)
		}
		if got := info.Mode().Perm(); got != 0o644 {
			t.Fatalf("precondition mode(%q) = %04o, want 0644", p, got)
		}
	}

	s2, err := New(dbPath)
	if err != nil {
		t.Fatalf("reopen New(%q): %v", dbPath, err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	for _, p := range paths {
		info, err := os.Lstat(p)
		if err != nil {
			t.Fatalf("Lstat(%q) after reopen: %v", p, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("mode(%q) after reopen = %04o, want 0600", p, got)
		}
	}
}

func TestNewSQLiteInMemoryCreatesNoFile(t *testing.T) {
	setUmask022(t)

	cwd := t.TempDir()
	t.Chdir(cwd)

	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("New(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := s.Ping(); err != nil {
		t.Fatalf("Ping(:memory:): %v", err)
	}

	memFile := filepath.Join(cwd, ":memory:")
	if _, err := os.Lstat(memFile); !os.IsNotExist(err) {
		t.Fatalf("expected no file %q in cwd, got err=%v", memFile, err)
	}
}
