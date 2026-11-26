//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestOpenServerLogCreates0600AndAppends(t *testing.T) {
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })

	logPath := filepath.Join(t.TempDir(), "server.log")

	f1, err := openServerLog(logPath)
	if err != nil {
		t.Fatalf("first openServerLog: %v", err)
	}
	if _, err := f1.WriteString("first\n"); err != nil {
		_ = f1.Close()
		t.Fatalf("write first: %v", err)
	}
	if err := f1.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}

	info, err := os.Lstat(logPath)
	if err != nil {
		t.Fatalf("Lstat(%q): %v", logPath, err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode(%q) = %04o, want 0600", logPath, got)
	}

	f2, err := openServerLog(logPath)
	if err != nil {
		t.Fatalf("second openServerLog: %v", err)
	}
	if _, err := f2.WriteString("second\n"); err != nil {
		_ = f2.Close()
		t.Fatalf("write second: %v", err)
	}
	if err := f2.Close(); err != nil {
		t.Fatalf("close second: %v", err)
	}

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", logPath, err)
	}
	if string(got) != "first\nsecond\n" {
		t.Errorf("log content = %q, want %q", string(got), "first\nsecond\n")
	}
}

func TestOpenServerLogTightensExistingLog(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "server.log")
	if err := os.WriteFile(logPath, []byte("old\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(logPath, 0o644); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	f, err := openServerLog(logPath)
	if err != nil {
		t.Fatalf("openServerLog: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	info, err := os.Lstat(logPath)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode(%q) = %04o, want 0600", logPath, got)
	}
}

func TestOpenServerLogLeavesSymlinkTargetMode(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere.log")
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	logPath := filepath.Join(dir, "server.log")
	if err := os.Symlink(target, logPath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	f, err := openServerLog(logPath)
	if err != nil {
		t.Fatalf("openServerLog: %v", err)
	}
	if _, err := f.WriteString("line\n"); err != nil {
		_ = f.Close()
		t.Fatalf("write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat(target): %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("symlink target mode = %04o, want 0644 (untouched)", got)
	}
	if got, _ := os.ReadFile(target); string(got) != "line\n" {
		t.Errorf("target content = %q, want the written line", got)
	}
}
