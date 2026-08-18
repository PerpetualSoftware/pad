//go:build linux

package cli

import (
	"encoding/json"
	"os"
	"testing"
)

// TestProcStartToken_CurrentProcess: on Linux the token is readable and
// stable for a live process (a sanity check that the /proc parsing works).
func TestProcStartToken_CurrentProcess(t *testing.T) {
	tok, ok := procStartToken(os.Getpid())
	if !ok || tok == "" {
		t.Fatalf("expected a proc-start token for the current pid, got ok=%v tok=%q", ok, tok)
	}
	tok2, _ := procStartToken(os.Getpid())
	if tok != tok2 {
		t.Fatalf("proc-start token must be stable: %q != %q", tok, tok2)
	}
}

// TestArmState_HeadlessPidReuseRejected is the Codex R1 HIGH-2 regression
// for the headless path on Linux: a live pid whose recorded owner-identity
// token does NOT match (a reused pid belonging to an unrelated process)
// must read as disarmed, not armed.
func TestArmState_HeadlessPidReuseRejected(t *testing.T) {
	repo := armStateTestEnv(t, "") // headless, cwd-keyed, pid liveness

	path, err := armStatePath("", repo)
	if err != nil {
		t.Fatal(err)
	}
	// A file owned by THIS (alive) pid but stamped with a different
	// proc-start token — the shape of a reused pid.
	st := ArmState{
		Armed:     true,
		PID:       os.Getpid(),
		ProcStart: "0", // guaranteed not to match this process's real start
		Cwd:       repo,
		StartedAt: "2026-08-18T00:00:00Z",
	}
	data, _ := json.MarshalIndent(st, "", "  ")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	if SessionArmedLocally() {
		t.Fatal("a live pid with a mismatched owner-identity token must NOT arm (pid reuse)")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("reused-pid file must be reaped; stat err = %v", err)
	}
}
