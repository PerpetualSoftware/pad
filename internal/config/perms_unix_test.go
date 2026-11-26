//go:build !windows

package config

import (
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

func TestLoadCreatesDataDirAndLogs0700(t *testing.T) {
	setUmask022(t)

	dataDir := filepath.Join(t.TempDir(), "pad-data")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PAD_DB_PATH", "")
	t.Setenv("PAD_DATA_DIR", dataDir)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DataDir != dataDir {
		t.Fatalf("DataDir = %q, want %q", cfg.DataDir, dataDir)
	}

	for _, dir := range []string{dataDir, filepath.Join(dataDir, "logs")} {
		info, err := os.Lstat(dir)
		if err != nil {
			t.Fatalf("Lstat(%q): %v", dir, err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("mode(%q) = %04o, want 0700", dir, got)
		}
	}
}

func TestLoadTightensPermissiveDataDirAndLogs(t *testing.T) {
	setUmask022(t)

	dataDir := filepath.Join(t.TempDir(), "pad-data")
	logsDir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	for _, dir := range []string{dataDir, logsDir} {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatalf("Chmod(%q): %v", dir, err)
		}
	}

	t.Setenv("HOME", t.TempDir())
	t.Setenv("PAD_DB_PATH", "")
	t.Setenv("PAD_DATA_DIR", dataDir)

	if _, err := Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	for _, dir := range []string{dataDir, logsDir} {
		info, err := os.Lstat(dir)
		if err != nil {
			t.Fatalf("Lstat(%q): %v", dir, err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("mode(%q) = %04o, want 0700", dir, got)
		}
	}
}

func TestLoadLeavesSymlinkDataDirTargetUntouched(t *testing.T) {
	setUmask022(t)

	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("MkdirAll(target): %v", err)
	}
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatalf("Chmod(target): %v", err)
	}

	link := filepath.Join(root, "pad-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	t.Setenv("HOME", t.TempDir())
	t.Setenv("PAD_DB_PATH", "")
	t.Setenv("PAD_DATA_DIR", link)

	if _, err := Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	linkInfo, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat(link): %v", err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected %q to remain a symlink", link)
	}

	targetInfo, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("Lstat(target): %v", err)
	}
	if got := targetInfo.Mode().Perm(); got != 0o755 {
		t.Errorf("symlink target mode = %04o, want 0755 (must remain untouched)", got)
	}
}

func TestSaveCreatesDataDirAndLogs0700(t *testing.T) {
	setUmask022(t)

	dataDir := filepath.Join(t.TempDir(), "fresh-save")
	cfg := &Config{
		Mode:       ModeLocal,
		Host:       "127.0.0.1",
		Port:       7777,
		DataDir:    dataDir,
		ConfigPath: filepath.Join(dataDir, "config.toml"),
	}

	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	for _, dir := range []string{dataDir, filepath.Join(dataDir, "logs")} {
		info, err := os.Lstat(dir)
		if err != nil {
			t.Fatalf("Lstat(%q): %v", dir, err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("mode(%q) = %04o, want 0700", dir, got)
		}
	}
}
