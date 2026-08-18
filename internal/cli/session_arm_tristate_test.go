package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// triStateEnv sets up a live socket-keyed session in an auto_arm=true repo:
// HOME + PAD_DATA_DIR point at temp dirs, CLAUDE_CODE_MESSAGING_SOCKET is a
// real (existing) file, and the cwd is a repo whose .pad.toml opts into
// auto_arm. Returns the socket file path so a test can "end" the session by
// removing it.
func triStateEnv(t *testing.T) (socketFile string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dataDir := filepath.Join(home, ".pad")
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAD_DATA_DIR", dataDir)

	socketFile = filepath.Join(t.TempDir(), "msg.sock")
	if err := os.WriteFile(socketFile, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", socketFile)

	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".pad.toml"), []byte("workspace = \"demo\"\n[push]\nauto_arm = true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	chdir(t, repo)
	return socketFile
}

// TestTriState_DisarmBeatsAutoArmWithinSession is the core PLAN-2613 S3
// ruling: in an auto_arm=true repo, a live `pad session disarm` (explicit
// OFF) must win for THIS session — the disconnect verb must not be a lie.
func TestTriState_DisarmBeatsAutoArmWithinSession(t *testing.T) {
	triStateEnv(t)

	// Baseline: with no local override, the auto_arm repo announces armed.
	if !ResolveAnnouncedArmed() {
		t.Fatal("auto_arm repo with no override must announce armed")
	}

	if _, err := WriteDisarmState(); err != nil {
		t.Fatalf("WriteDisarmState: %v", err)
	}
	if got := SessionArmState(); got != LocalArmOff {
		t.Fatalf("SessionArmState = %v, want LocalArmOff", got)
	}
	if ResolveAnnouncedArmed() {
		t.Fatal("a live explicit disarm must beat auto_arm — session must NOT announce armed")
	}
}

// TestTriState_DisarmDiesWithSession: across sessions auto_arm remains the
// contract. A disarm marker whose owner is dead is reaped and the session
// falls back to auto_arm (re-armed) — permanent-off is a .pad.toml edit,
// not a lingering OFF file.
func TestTriState_DisarmDiesWithSession(t *testing.T) {
	socketFile := triStateEnv(t)

	path, err := WriteDisarmState()
	if err != nil {
		t.Fatalf("WriteDisarmState: %v", err)
	}
	if ResolveAnnouncedArmed() {
		t.Fatal("live disarm must suppress arming")
	}

	// End the session: the socket vanishes → the OFF marker's owner is dead.
	if err := os.Remove(socketFile); err != nil {
		t.Fatal(err)
	}
	if got := SessionArmState(); got != LocalArmAbsent {
		t.Fatalf("SessionArmState after owner death = %v, want LocalArmAbsent", got)
	}
	if !ResolveAnnouncedArmed() {
		t.Fatal("across sessions auto_arm must re-arm — a dead disarm marker must not persist")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("dead disarm marker must be reaped; stat err = %v", err)
	}
}

// TestTriState_ArmThenDisarmFlips: arm then disarm flips the announced
// value both ways within a session (the file is overwritten, not appended).
func TestTriState_ArmThenDisarmFlips(t *testing.T) {
	triStateEnv(t)

	if _, err := WriteArmState(); err != nil {
		t.Fatalf("arm: %v", err)
	}
	if SessionArmState() != LocalArmOn || !ResolveAnnouncedArmed() {
		t.Fatal("after arm: expected LocalArmOn and announced=true")
	}

	if _, err := WriteDisarmState(); err != nil {
		t.Fatalf("disarm: %v", err)
	}
	if SessionArmState() != LocalArmOff || ResolveAnnouncedArmed() {
		t.Fatal("after disarm: expected LocalArmOff and announced=false")
	}

	if _, err := WriteArmState(); err != nil {
		t.Fatalf("re-arm: %v", err)
	}
	if SessionArmState() != LocalArmOn || !ResolveAnnouncedArmed() {
		t.Fatal("after re-arm: expected LocalArmOn and announced=true")
	}
}

// TestArmState_SemanticallyCorruptFailsClosed is the Codex R2 S3 HIGH-2
// regression: a syntactically-valid but semantically-garbage file (missing
// the stamps our writer always sets) must fail CLOSED — not be judged
// owner-dead, reaped, and re-armed via auto_arm; and not be treated as a
// live headless arm just because it names a live pid like init (1).
func TestArmState_SemanticallyCorruptFailsClosed(t *testing.T) {
	for _, body := range []string{
		`{}`,
		`{"pid":1}`,
		`{"pid":1,"armed":true}`, // no StartedAt
		// Well-stamped but violates the writer invariant Armed != Disarmed:
		// neither armed nor disarmed must not resolve to armed (Codex R4).
		`{"pid":123,"started_at":"2026-08-18T00:00:00Z","armed":false,"disarmed":false}`,
		`{"pid":123,"started_at":"2026-08-18T00:00:00Z","armed":true,"disarmed":true}`,
	} {
		socketFile := triStateEnv(t) // auto_arm=true repo, so the distinction bites
		path, err := armStatePath(socketFile, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if got := SessionArmState(); got != LocalArmError {
			t.Fatalf("body %q: SessionArmState = %v, want LocalArmError", body, got)
		}
		if ResolveAnnouncedArmed() {
			t.Fatalf("body %q: must fail closed — NOT armed and NOT via auto_arm", body)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("body %q: corrupt file must NOT be reaped: %v", body, err)
		}
	}
}

// TestMarkFirstConnect_OncePerSessionAcrossToggles: the boot ritual fires
// on the first connect only (D8), and the flag survives arm/disarm toggles
// so a reconnect or a consent flip never re-fires it.
func TestMarkFirstConnect_OncePerSessionAcrossToggles(t *testing.T) {
	triStateEnv(t)
	if _, err := WriteArmState(); err != nil {
		t.Fatalf("arm: %v", err)
	}

	first, err := MarkFirstConnect()
	if err != nil || !first {
		t.Fatalf("first MarkFirstConnect must be true (err=%v)", err)
	}
	if again, _ := MarkFirstConnect(); again {
		t.Fatal("second MarkFirstConnect must be false — boot fires once")
	}

	// A disarm then re-arm within the session must NOT reset the flag.
	if _, err := WriteDisarmState(); err != nil {
		t.Fatalf("disarm: %v", err)
	}
	if again, _ := MarkFirstConnect(); again {
		t.Fatal("boot flag must survive disarm (carried forward)")
	}
	if _, err := WriteArmState(); err != nil {
		t.Fatalf("re-arm: %v", err)
	}
	if again, _ := MarkFirstConnect(); again {
		t.Fatal("boot flag must survive re-arm (carried forward)")
	}
}
