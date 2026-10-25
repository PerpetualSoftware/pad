package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TASK-4071: the chat-seen log a plugin tails to show chat to the human.

func chatSeenOwner(pid int) SessionOwner {
	return SessionOwner{PID: pid, PIDSource: "PAD_SESSION_PID"}
}

func readChatSeen(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestChatSeenAppendsLinesPrivately(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var warn bytes.Buffer
	l := OpenChatSeen(chatSeenOwner(4242), nil, &warn)
	path := l.Path()
	if want := filepath.Join(os.Getenv("HOME"), ".pad", "chat-seen", "4242.log"); path != want {
		t.Fatalf("path %q, want %q", path, want)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("no file or dir before the first line; stat err = %v", err)
	}
	l.Append("one")
	l.Append("two")
	if got := readChatSeen(t, path); got != "one\ntwo\n" || warn.Len() != 0 {
		t.Fatalf("log %q warn %q", got, warn.String())
	}
	if runtime.GOOS == "windows" {
		return
	}
	if info, _ := os.Stat(filepath.Dir(path)); info.Mode().Perm() != 0700 {
		t.Fatalf("dir mode %v", info.Mode().Perm())
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0600 {
		t.Fatalf("file mode %v", info.Mode().Perm())
	}
}

func TestChatSeenTightensLooseDirAndFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	t.Setenv("HOME", t.TempDir())
	path, _ := ChatSeenPath(7)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	var warn bytes.Buffer
	OpenChatSeen(chatSeenOwner(7), nil, &warn).Append("new")
	if got := readChatSeen(t, path); got != "old\nnew\n" || warn.Len() != 0 {
		t.Fatalf("log %q warn %q", got, warn.String())
	}
	if info, _ := os.Stat(filepath.Dir(path)); info.Mode().Perm() != 0700 {
		t.Fatalf("dir mode %v, want 0700", info.Mode().Perm())
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0600 {
		t.Fatalf("file mode %v, want 0600", info.Mode().Perm())
	}
}

func TestChatSeenRefusesSymlinkAndStops(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	t.Setenv("HOME", t.TempDir())
	path, _ := ChatSeenPath(8)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	var warn bytes.Buffer
	l := OpenChatSeen(chatSeenOwner(8), nil, &warn)
	l.Append("a")
	l.Append("b")
	if got := readChatSeen(t, target); got != "" {
		t.Fatalf("wrote through a symlink: %q", got)
	}
	if strings.Count(warn.String(), "\n") != 1 || !strings.Contains(warn.String(), "stopped showing chat lines") {
		t.Fatalf("want exactly one warning, got %q", warn.String())
	}
}

func TestChatSeenRefusesFifoWithoutHanging(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFOs")
	}
	t.Setenv("HOME", t.TempDir())
	path, _ := ChatSeenPath(9)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := mkfifoForTest(path); err != nil {
		t.Skip("mkfifo:", err)
	}
	var warn bytes.Buffer
	done := make(chan struct{})
	go func() {
		OpenChatSeen(chatSeenOwner(9), nil, &warn).Append("x")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Append blocked opening a FIFO")
	}
	if !strings.Contains(warn.String(), "stopped showing chat lines") {
		t.Fatalf("warn %q", warn.String())
	}
}

func TestChatSeenRotatesOneGeneration(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var warn bytes.Buffer
	l := OpenChatSeen(chatSeenOwner(10), nil, &warn)
	l.max = 10
	for _, s := range []string{"aaaa", "bbbb", "cccc", "dddd", "eeee"} {
		l.Append(s) // 5 bytes each: two fit in one generation
	}
	if got := readChatSeen(t, l.Path()); got != "eeee\n" {
		t.Fatalf("current %q", got)
	}
	if got := readChatSeen(t, l.Path()+".1"); got != "cccc\ndddd\n" {
		t.Fatalf("previous generation %q", got)
	}
	if warn.Len() != 0 {
		t.Fatal(warn.String())
	}
}

// A line longer than the cap still lands; it is not rotated against itself.
func TestChatSeenOversizeLineOnEmptyLog(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var warn bytes.Buffer
	l := OpenChatSeen(chatSeenOwner(11), nil, &warn)
	l.max = 4
	l.Append("longer than four")
	if got := readChatSeen(t, l.Path()); got != "longer than four\n" {
		t.Fatalf("current %q", got)
	}
	if _, err := os.Stat(l.Path() + ".1"); !os.IsNotExist(err) {
		t.Fatalf("no rotation of an empty log; stat err = %v", err)
	}
}

// C2: a pid from os.Getpid() keys nothing a reader or a prune can find.
func TestChatSeenOffWithoutHarnessPid(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PAD_SESSION_PID", "")
	t.Setenv("CLAUDE_PID", "")
	owner, err := CaptureSessionOwner()
	if err != nil || owner.PIDSource != "self" {
		t.Fatalf("setup: %+v %v", owner, err)
	}
	var warn bytes.Buffer
	l := OpenChatSeen(owner, err, &warn)
	if l.Path() != "" || warn.Len() != 0 {
		t.Fatalf("want no log and a silent start for a self pid, got %q, warn %q", l.Path(), warn.String())
	}
	l.Append("x")
	l.Append("y")
	if strings.Count(warn.String(), "\n") != 1 || !strings.Contains(warn.String(), "PAD_SESSION_PID") {
		t.Fatalf("want one stderr line naming PAD_SESSION_PID, got %q", warn.String())
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".pad", "chat-seen")); !os.IsNotExist(err) {
		t.Fatalf("nothing may be created; stat err = %v", err)
	}
}

func TestChatSeenOffOnInvalidSessionPid(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PAD_SESSION_PID", "nope")
	owner, err := CaptureSessionOwner()
	var warn bytes.Buffer
	l := OpenChatSeen(owner, err, &warn)
	l.Append("x")
	if l.Path() != "" || !strings.Contains(warn.String(), "PAD_SESSION_PID") {
		t.Fatalf("log %q warn %q", l.Path(), warn.String())
	}
}

// C1's pin: the mod passes its sessionPid() as PAD_SESSION_PID, and that
// keys the log ahead of CLAUDE_PID, so the reader and writer agree.
func TestChatSeenKeyedByPadSessionPidFirst(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ppid := os.Getppid()
	t.Setenv("PAD_SESSION_PID", strconv.Itoa(ppid))
	t.Setenv("CLAUDE_PID", "1")
	owner, err := CaptureSessionOwner()
	var warn bytes.Buffer
	l := OpenChatSeen(owner, err, &warn)
	if filepath.Base(l.Path()) != strconv.Itoa(ppid)+".log" {
		t.Fatalf("log %v warn %q", l, warn.String())
	}
	t.Setenv("PAD_SESSION_PID", "")
	owner, err = CaptureSessionOwner()
	if l := OpenChatSeen(owner, err, &warn); filepath.Base(l.Path()) != "1.log" {
		t.Fatalf("CLAUDE_PID fallback: log %q", l.Path())
	}
}

func TestPruneRemovesDeadSessionsChatSeen(t *testing.T) {
	dir := registryEnv(t)
	dead := exitedProcessPID(t)
	writeV2Record(t, dir, SessionRegistration{SessionOwner: SessionOwner{PID: dead, PIDSource: "CLAUDE_PID"}, Agent: "gone", Cwd: "/b", RegisteredAt: "2026-08-01T00:00:00Z"})
	writeV2Record(t, dir, SessionRegistration{SessionOwner: SessionOwner{PID: os.Getpid(), PIDSource: "CLAUDE_PID"}, Agent: "live", Cwd: "/a", RegisteredAt: "2026-08-01T00:00:00Z"})
	var warn bytes.Buffer
	deadLog := OpenChatSeen(chatSeenOwner(dead), nil, &warn)
	deadLog.Append("x")
	if err := os.WriteFile(deadLog.Path()+".1", []byte("y\n"), 0600); err != nil {
		t.Fatal(err)
	}
	liveLog := OpenChatSeen(chatSeenOwner(os.Getpid()), nil, &warn)
	liveLog.Append("z")

	rep, err := PruneSessions(0, time.Now())
	if err != nil || rep.DeadRemoved != 1 {
		t.Fatalf("prune: %+v %v", rep, err)
	}
	for _, p := range []string{deadLog.Path(), deadLog.Path() + ".1"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s must go with its record; stat err = %v", p, err)
		}
	}
	if got := readChatSeen(t, liveLog.Path()); got != "z\n" {
		t.Fatalf("a live session's log must stay: %q", got)
	}
}

// r1: ensurePrivateDir follows a symlink, so a symlinked chat-seen directory
// would have its target chmodded and the log written into it.
func TestChatSeenRefusesSymlinkedDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	t.Setenv("HOME", t.TempDir())
	path, _ := ChatSeenPath(13)
	target := t.TempDir()
	if err := os.Chmod(target, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Dir(path)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	var warn bytes.Buffer
	OpenChatSeen(chatSeenOwner(13), nil, &warn).Append("x")
	if !strings.Contains(warn.String(), "is a symlink") {
		t.Fatalf("warn %q", warn.String())
	}
	if info, _ := os.Stat(target); info.Mode().Perm() != 0755 {
		t.Fatalf("the symlink's target was chmodded to %v", info.Mode().Perm())
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Fatalf("wrote into the symlink's target: %v", entries)
	}
}

// r1: a log removed under the writer (a prune of an unknown-liveness record)
// is recreated at its path rather than written into an unlinked file.
func TestChatSeenReopensAfterRemoval(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var warn bytes.Buffer
	l := OpenChatSeen(chatSeenOwner(14), nil, &warn)
	l.Append("before")
	if err := removeChatSeen(14); err != nil {
		t.Fatal(err)
	}
	l.Append("after")
	if got := readChatSeen(t, l.Path()); got != "after\n" || warn.Len() != 0 {
		t.Fatalf("log %q warn %q", got, warn.String())
	}
}
