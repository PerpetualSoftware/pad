package testtmp

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

func TestSweepRemovesOnlyTheDead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no liveness probe on windows")
	}
	root := t.TempDir()
	const prefix = "pad-x-template-"
	// A pid that has exited: a finished child.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skip("cannot run true:", err)
	}
	dead := cmd.Process.Pid
	mk := func(name string, age time.Duration) string {
		p := filepath.Join(root, name)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "template.db"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		ts := time.Now().Add(-age)
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatal(err)
		}
		return p
	}
	deadDir := mk(prefix+strconv.Itoa(dead)+"-123", 0)
	selfDir := mk(prefix+strconv.Itoa(os.Getpid())+"-456", 0)
	parentDir := mk(prefix+strconv.Itoa(os.Getppid())+"-789", 0)
	oldLegacy := mk(prefix+"1234567", 30*24*time.Hour)
	newLegacy := mk(prefix+"7654321", 2*time.Hour)
	other := mk("pad-other-"+strconv.Itoa(dead)+"-1", 0)

	Sweep(root, prefix)

	for p, want := range map[string]bool{
		deadDir: false, selfDir: true, parentDir: true,
		oldLegacy: true, newLegacy: true, other: true,
	} {
		_, err := os.Stat(p)
		if got := err == nil; got != want {
			t.Errorf("%s: exists=%v, want %v", filepath.Base(p), got, want)
		}
	}
}

func TestMkdirTempNamesThePid(t *testing.T) {
	dir, err := MkdirTemp("pad-testtmp-selftest-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	want := "pad-testtmp-selftest-" + strconv.Itoa(os.Getpid()) + "-"
	if got := filepath.Base(dir); len(got) <= len(want) || got[:len(want)] != want {
		t.Fatalf("dir %q lacks prefix %q", got, want)
	}
}
