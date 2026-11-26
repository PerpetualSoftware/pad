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

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat(%q): %v", path, err)
	}
	return info.Mode()
}

func TestLoadTightensPermissiveDefaultDataDirAndLogs(t *testing.T) {
	setUmask022(t)

	home := t.TempDir()
	dataDir := filepath.Join(home, ".pad")
	logsDir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	for _, dir := range []string{dataDir, logsDir} {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatalf("Chmod(%q): %v", dir, err)
		}
	}

	t.Setenv("HOME", home)
	t.Setenv("PAD_DB_PATH", "")
	t.Setenv("PAD_DATA_DIR", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DataDir != dataDir {
		t.Fatalf("DataDir = %q, want default %q", cfg.DataDir, dataDir)
	}
	for _, dir := range []string{dataDir, logsDir} {
		if got := modeOf(t, dir).Perm(); got != 0o700 {
			t.Errorf("mode(%q) = %04o, want 0700", dir, got)
		}
	}
}

// A directory named through PAD_DATA_DIR may be shared or not Pad's own; an
// existing one keeps its mode.
func TestLoadLeavesExistingEnvDataDirMode(t *testing.T) {
	setUmask022(t)

	dataDir := filepath.Join(t.TempDir(), "shared")
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
		if got := modeOf(t, dir).Perm(); got != 0o755 {
			t.Errorf("mode(%q) = %04o, want 0755 (env-supplied dir untouched)", dir, got)
		}
	}
}

// PAD_DB_PATH=pad.db makes the data dir the working directory, which must
// keep its mode.
func TestLoadLeavesWorkingDirForRelativeDBPath(t *testing.T) {
	setUmask022(t)

	cwd := filepath.Join(t.TempDir(), "checkout")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Chmod(cwd, 0o755); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Chdir(cwd)

	t.Setenv("HOME", t.TempDir())
	t.Setenv("PAD_DATA_DIR", "")
	t.Setenv("PAD_DB_PATH", "pad.db")

	if _, err := Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := modeOf(t, cwd).Perm(); got != 0o755 {
		t.Errorf("mode(cwd) = %04o, want 0755", got)
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
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("MkdirAll(home): %v", err)
	}
	// The default location itself is a symlink: tightenDir must not follow it.
	link := filepath.Join(home, ".pad")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	t.Setenv("HOME", home)
	t.Setenv("PAD_DB_PATH", "")
	t.Setenv("PAD_DATA_DIR", "")

	if _, err := Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if modeOf(t, link)&os.ModeSymlink == 0 {
		t.Fatalf("expected %q to remain a symlink", link)
	}
	if got := modeOf(t, target).Perm(); got != 0o755 {
		t.Errorf("symlink target mode = %04o, want 0755 (must remain untouched)", got)
	}
}

func TestTightenDirSkipsStickyAndWorldWritable(t *testing.T) {
	setUmask022(t)

	root := t.TempDir()
	sticky := filepath.Join(root, "sticky")
	world := filepath.Join(root, "world")
	for _, d := range []string{sticky, world} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatalf("Mkdir(%q): %v", d, err)
		}
	}
	if err := os.Chmod(sticky, 0o755|os.ModeSticky); err != nil {
		t.Fatalf("Chmod(sticky): %v", err)
	}
	if err := os.Chmod(world, 0o777); err != nil {
		t.Fatalf("Chmod(world): %v", err)
	}

	tightenDir(sticky)
	tightenDir(world)

	if m := modeOf(t, sticky); m.Perm() != 0o755 || m&os.ModeSticky == 0 {
		t.Errorf("sticky dir mode = %v, want drwxr-xr-t unchanged", m)
	}
	if got := modeOf(t, world).Perm(); got != 0o777 {
		t.Errorf("world-writable dir mode = %04o, want 0777 unchanged", got)
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
