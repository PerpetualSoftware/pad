package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// armStateTestEnv points HOME (and thus ~/.pad/sessions) at a temp dir and
// sets the messaging socket env to socket ("" for the headless case),
// then chdirs to a fresh repo dir so the cwd fallback key is stable.
// Returns the repo dir.
func armStateTestEnv(t *testing.T, socket string) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", socket)
	repo := t.TempDir()
	chdir(t, repo)
	return repo
}

func TestArmStateKey(t *testing.T) {
	t.Parallel()

	sessKey, headless := armStateKey("/run/user/1000/msg.sock", "/home/x/repo")
	if headless {
		t.Fatal("socket present must NOT be headless")
	}
	if !strings.HasPrefix(sessKey, "sess-") {
		t.Fatalf("socket key = %q, want sess- prefix", sessKey)
	}

	repoKey, headless := armStateKey("", "/home/x/repo")
	if !headless {
		t.Fatal("no socket must be headless (cwd fallback)")
	}
	if !strings.HasPrefix(repoKey, "repo-") {
		t.Fatalf("cwd key = %q, want repo- prefix", repoKey)
	}

	// A socket wins over cwd, and different sockets key differently while
	// the same socket is stable (so arm and monitor in one session agree).
	if sessKey == repoKey {
		t.Fatal("socket and cwd keys must differ")
	}
	other, _ := armStateKey("/run/user/1000/other.sock", "/home/x/repo")
	if other == sessKey {
		t.Fatal("different sockets must produce different keys")
	}
	again, _ := armStateKey("/run/user/1000/msg.sock", "/different/cwd")
	if again != sessKey {
		t.Fatal("same socket must produce the same key regardless of cwd")
	}
}

// TestArmState_SocketLivenessRoundTrip is the core happy path AND the
// non-negotiable liveness rule (constraint 2): a socket-keyed session is
// armed while its socket exists and DISARMED the instant the socket
// vanishes (the Claude Code session ended), with the stale file reaped so
// it can never arm a future monitor.
func TestArmState_SocketLivenessRoundTrip(t *testing.T) {
	socketFile := filepath.Join(t.TempDir(), "msg.sock")
	if err := os.WriteFile(socketFile, nil, 0600); err != nil {
		t.Fatal(err)
	}
	armStateTestEnv(t, socketFile)

	path, err := WriteArmState()
	if err != nil {
		t.Fatalf("WriteArmState: %v", err)
	}
	if !SessionArmedLocally() {
		t.Fatal("armed session with a live socket must read as armed")
	}

	// Socket vanishes → owner is dead → disarmed, and the file is reaped.
	if err := os.Remove(socketFile); err != nil {
		t.Fatal(err)
	}
	if SessionArmedLocally() {
		t.Fatal("a session whose socket vanished must NOT read as armed (consent-grandfathering)")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale arm-state file must be reaped by the reader; stat err = %v", err)
	}
}

// TestArmState_DisarmRemovesAndIsIdempotent covers constraint 3: disarm
// removes the file and reports whether one was there; a second disarm is a
// success, not an error.
func TestArmState_DisarmRemovesAndIsIdempotent(t *testing.T) {
	socketFile := filepath.Join(t.TempDir(), "msg.sock")
	if err := os.WriteFile(socketFile, nil, 0600); err != nil {
		t.Fatal(err)
	}
	armStateTestEnv(t, socketFile)

	if _, err := WriteArmState(); err != nil {
		t.Fatalf("WriteArmState: %v", err)
	}
	removed, _, err := RemoveArmState()
	if err != nil {
		t.Fatalf("RemoveArmState: %v", err)
	}
	if !removed {
		t.Fatal("first disarm should report a file was removed")
	}
	if SessionArmedLocally() {
		t.Fatal("session must not read as armed after disarm")
	}

	removed, _, err = RemoveArmState()
	if err != nil {
		t.Fatalf("idempotent disarm must not error: %v", err)
	}
	if removed {
		t.Fatal("second disarm should report nothing was removed")
	}
}

// TestArmState_HeadlessLivePid: the cwd-fallback path with no socket reads
// as armed while its owner pid (this process) is alive.
func TestArmState_HeadlessLivePid(t *testing.T) {
	armStateTestEnv(t, "") // no socket → headless, cwd-keyed, pid liveness

	if _, err := WriteArmState(); err != nil {
		t.Fatalf("WriteArmState: %v", err)
	}
	if !SessionArmedLocally() {
		t.Fatal("headless arm owned by this live process must read as armed")
	}
}

// TestArmState_HeadlessDeadPidReaped: the liveness rule for the headless
// fallback — a file whose owner pid is gone reads as DISARMED and is
// reaped. Uses a genuinely-exited process's pid rather than a guessed
// number, so the test doesn't depend on a pid being coincidentally free.
func TestArmState_HeadlessDeadPidReaped(t *testing.T) {
	repo := armStateTestEnv(t, "")

	deadPID := exitedProcessPID(t)

	// Write a headless arm-state file by hand with the dead owner pid.
	path, err := armStatePath("", repo)
	if err != nil {
		t.Fatal(err)
	}
	st := ArmState{Armed: true, PID: deadPID, Cwd: repo, StartedAt: "2026-08-18T00:00:00Z"}
	data, _ := json.MarshalIndent(st, "", "  ")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	if SessionArmedLocally() {
		t.Fatal("headless arm owned by a dead pid must NOT read as armed")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("dead-owner headless file must be reaped; stat err = %v", err)
	}
}

// TestArmState_MalformedFailsClosed: an unparseable arm-state file reads
// as not-armed rather than erroring or arming — a consent gate fails
// closed.
func TestArmState_MalformedFailsClosed(t *testing.T) {
	armStateTestEnv(t, filepath.Join(t.TempDir(), "msg.sock"))
	path, err := armStatePath(os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET"), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if SessionArmedLocally() {
		t.Fatal("a malformed arm-state file must fail closed (not armed)")
	}
}

// exitedProcessPID starts and reaps a trivial process, returning its pid,
// which is then dead. Skips on platforms without a shell.
func exitedProcessPID(t *testing.T) int {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no shell to spawn a dead process from")
	}
	cmd := exec.Command(sh, "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start throwaway process: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait() // reap — pid is now dead
	return pid
}
