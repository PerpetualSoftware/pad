package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// BUG-2771: an arm-state file whose owner cannot be probed (the unknown
// verdict) used to be treated as dead: reaped on the next read, which handed
// the decision to auto_arm and forgot an explicit disarm. It now resolves to
// LocalArmUnverifiable — not armed, not reaped. The unknown verdict comes from
// Windows for every headless file, and on unix from an unexaminable /proc
// entry or socket; the seam reaches it on any platform, and one leg below
// reaches it through the real probe.

func withOwnerVerdict(t *testing.T, v Liveness) {
	t.Helper()
	prev := ownerLivenessFn
	ownerLivenessFn = func(*SessionOwner) Liveness { return v }
	t.Cleanup(func() { ownerLivenessFn = prev })
}

// headlessEnv is triStateEnv without the messaging socket: the cwd-keyed
// fallback, which is the path every Windows session takes.
func headlessEnv(t *testing.T) {
	t.Helper()
	triStateEnv(t)
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "")
	// A test run from inside an agent harness inherits its session pid;
	// each leg names its owner explicitly or runs as "self".
	t.Setenv("CLAUDE_PID", "")
	t.Setenv("PAD_SESSION_PID", "")
}

func armFileExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", path, err)
	}
	return err == nil
}

func TestUnverifiableDisarmIsKeptAndNotArmed(t *testing.T) {
	headlessEnv(t)
	withOwnerVerdict(t, LivenessUnknown)
	if !ResolveAutoArmFromDisk().Armed {
		t.Fatal("premise: the repo must auto_arm, or a forgotten disarm would look like a kept one")
	}

	path, err := WriteDisarmState()
	if err != nil {
		t.Fatalf("WriteDisarmState: %v", err)
	}
	for i := 0; i < 2; i++ {
		if got := SessionArmState(); got != LocalArmUnverifiable {
			t.Fatalf("read %d: SessionArmState = %v, want LocalArmUnverifiable", i+1, got)
		}
		if !armFileExists(t, path) {
			t.Fatalf("read %d reaped the disarm file", i+1)
		}
	}
	if ResolveAnnouncedArmed() {
		t.Fatal("an unverifiable disarm was forgotten: the session announces armed via auto_arm")
	}
}

func TestUnverifiableArmDoesNotArm(t *testing.T) {
	headlessEnv(t)
	withOwnerVerdict(t, LivenessUnknown)
	// A repo WITHOUT auto_arm, so only the local file could arm.
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".pad.toml"), []byte("workspace = \"demo\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	chdir(t, repo)

	if _, err := WriteArmState(); err != nil {
		t.Fatalf("WriteArmState: %v", err)
	}
	if got := SessionArmState(); got != LocalArmUnverifiable {
		t.Fatalf("SessionArmState = %v, want LocalArmUnverifiable", got)
	}
	if ResolveAnnouncedArmed() || SessionArmedLocally() {
		t.Fatal("an arm whose session cannot be verified must not arm")
	}
}

// Controls: the other two verdicts behave exactly as before.
func TestDeadVerdictStillReaps(t *testing.T) {
	headlessEnv(t)
	withOwnerVerdict(t, LivenessDead)
	path, err := WriteDisarmState()
	if err != nil {
		t.Fatal(err)
	}
	if got := SessionArmState(); got != LocalArmAbsent {
		t.Fatalf("SessionArmState = %v, want LocalArmAbsent", got)
	}
	if armFileExists(t, path) {
		t.Fatal("a dead owner's file was not reaped")
	}
	if !ResolveAnnouncedArmed() {
		t.Fatal("after a reap, auto_arm must decide (armed)")
	}
}

func TestAliveVerdictStillResolves(t *testing.T) {
	headlessEnv(t)
	withOwnerVerdict(t, LivenessAlive)
	if _, err := WriteDisarmState(); err != nil {
		t.Fatal(err)
	}
	if got := SessionArmState(); got != LocalArmOff {
		t.Fatalf("SessionArmState = %v, want LocalArmOff", got)
	}
	if _, err := WriteArmState(); err != nil {
		t.Fatal(err)
	}
	if got := SessionArmState(); got != LocalArmOn {
		t.Fatalf("SessionArmState = %v, want LocalArmOn", got)
	}
}

// Reset is the way out of a kept file: auto_arm decides again.
func TestResetRemovesUnverifiableFile(t *testing.T) {
	headlessEnv(t)
	withOwnerVerdict(t, LivenessUnknown)
	path, err := WriteDisarmState()
	if err != nil {
		t.Fatal(err)
	}
	removed, rpath, err := RemoveArmState()
	if err != nil || !removed || rpath != path {
		t.Fatalf("RemoveArmState = (%v, %q, %v), want (true, %q, nil)", removed, rpath, err, path)
	}
	if got := SessionArmState(); got != LocalArmAbsent {
		t.Fatalf("SessionArmState after reset = %v, want LocalArmAbsent", got)
	}
	if !ResolveAnnouncedArmed() {
		t.Fatal("after reset, auto_arm must decide (armed)")
	}
}

// The real probe, no seam: a socket-keyed disarm whose socket's directory
// becomes unsearchable. stat fails with EACCES, which OwnerLiveness reports as
// unknown (session_owner.go). This is the unix member of the population, and
// the leg that shows the fix is keyed on the verdict rather than on GOOS.
func TestRealUnknownVerdictKeepsDisarm(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so EACCES cannot be produced")
	}
	socketFile := triStateEnv(t)
	path, err := WriteDisarmState()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(socketFile)
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	if _, err := os.Stat(socketFile); err == nil || os.IsNotExist(err) {
		t.Fatalf("premise: stat must fail with a non-ENOENT error, got %v", err)
	}

	if got := SessionArmState(); got != LocalArmUnverifiable {
		t.Fatalf("SessionArmState = %v, want LocalArmUnverifiable", got)
	}
	if !armFileExists(t, path) {
		t.Fatal("the real unknown verdict reaped the disarm file")
	}
	if ResolveAnnouncedArmed() {
		t.Fatal("the real unknown verdict forgot the disarm")
	}
}

// BUG-3227: a headless file is owned by the SESSION process the harness names,
// not by the command that wrote it. A live child stands in for the session:
// while it lives the disarm holds, through the real probe, and once it dies
// the file is reaped. Before the fix the file named the writer, which in this
// test is the test process itself, so it stayed "alive" after the session
// died; in a real `pad session disarm` it named the exiting command, so it
// was dead on arrival.
func TestHeadlessArmFileIsOwnedByTheSessionProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no pid probe on Windows; that case is LocalArmUnverifiable")
	}
	headlessEnv(t)
	session := exec.Command("sleep", "60")
	if err := session.Start(); err != nil {
		t.Fatalf("start stand-in session: %v", err)
	}
	t.Cleanup(func() { _ = session.Process.Kill(); _, _ = session.Process.Wait() })
	t.Setenv("PAD_SESSION_PID", strconv.Itoa(session.Process.Pid))

	path, err := WriteDisarmState()
	if err != nil {
		t.Fatalf("WriteDisarmState: %v", err)
	}
	if got := SessionArmState(); got != LocalArmOff {
		t.Fatalf("while the session lives: SessionArmState = %v, want LocalArmOff", got)
	}
	if ResolveAnnouncedArmed() {
		t.Fatal("a held headless disarm was overridden by auto_arm")
	}

	_ = session.Process.Kill()
	_, _ = session.Process.Wait()
	deadline := time.Now().Add(2 * time.Second)
	for SessionArmState() != LocalArmAbsent && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := SessionArmState(); got != LocalArmAbsent {
		t.Fatalf("after the session died: SessionArmState = %v, want LocalArmAbsent (file keyed on the writer, not the session)", got)
	}
	if armFileExists(t, path) {
		t.Fatal("the dead session's disarm was not reaped")
	}
}

// The verbs refuse a write that did not survive its own read-back: a dead
// owner, which is what a headless write naming no session process produces.
func TestVerifyArmStateHeldRefusesAReapedWrite(t *testing.T) {
	headlessEnv(t)
	withOwnerVerdict(t, LivenessDead)
	if _, err := WriteDisarmState(); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyArmStateHeld(); err == nil {
		t.Fatal("a disarm reaped on arrival was reported as held")
	}
}

func TestVerifyArmStateHeldAcceptsHeldStates(t *testing.T) {
	for _, v := range []Liveness{LivenessAlive, LivenessUnknown} {
		t.Run(string(v), func(t *testing.T) {
			headlessEnv(t)
			t.Setenv("PAD_SESSION_PID", strconv.Itoa(os.Getpid()))
			withOwnerVerdict(t, v)
			if _, err := WriteDisarmState(); err != nil {
				t.Fatal(err)
			}
			if state, err := VerifyArmStateHeld(); err != nil {
				t.Fatalf("verdict %s: state %v refused: %v", v, state, err)
			}
		})
	}
}

// The in-process case the read-back cannot observe: a headless write naming
// no session process is owned by the writing command, which is alive during
// the read-back and dead the moment it exits. It is refused on its source.
func TestVerifyArmStateHeldRefusesASelfOwnedHeadlessWrite(t *testing.T) {
	headlessEnv(t) // no PAD_SESSION_PID, no CLAUDE_PID: the owner is "self"
	withOwnerVerdict(t, LivenessAlive)
	if _, err := WriteDisarmState(); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyArmStateHeld(); err == nil {
		t.Fatal("a self-owned headless disarm, dead once the command exits, was reported as held")
	}
}
