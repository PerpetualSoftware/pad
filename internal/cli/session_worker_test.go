package cli

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3613 (lead ruling): a session's lease worker id survives a /clear in
// the same process (which re-runs `pad session register`) and is new for a
// new process, including one that reuses the pid.

func TestTASK3613_WorkerIDSurvivesReRegisterBySameProcess(t *testing.T) {
	registryEnv(t)
	t.Setenv("CLAUDE_PID", itoa(os.Getppid()))

	first, err := RegisterSession("/w", "rook")
	if err != nil {
		t.Fatal(err)
	}
	reg1, _ := readRegistration(t, first.Path)
	if !models.ValidWorkerID(reg1.WorkerID) {
		t.Fatalf("register did not mint a valid worker id: %q", reg1.WorkerID)
	}
	if reg1.ProcStart == "" {
		t.Skip("this platform records no process start token, so a re-register cannot be told from a new process")
	}

	// A /clear: the same process registers again.
	second, err := RegisterSession("/w", "rook")
	if err != nil {
		t.Fatal(err)
	}
	reg2, _ := readRegistration(t, second.Path)
	if reg2.WorkerID != reg1.WorkerID {
		t.Fatalf("re-register by the same process changed the worker id: %q -> %q", reg1.WorkerID, reg2.WorkerID)
	}
	if got := ResolveWorkerID(); got != reg1.WorkerID {
		t.Fatalf("ResolveWorkerID = %q, want the registered %q", got, reg1.WorkerID)
	}
}

func TestTASK3613_NewProcessGetsANewWorkerID(t *testing.T) {
	registryEnv(t)
	t.Setenv("CLAUDE_PID", itoa(os.Getppid()))

	first, err := RegisterSession("/w", "rook")
	if err != nil {
		t.Fatal(err)
	}
	reg1, raw := readRegistration(t, first.Path)
	if reg1.ProcStart == "" {
		t.Skip("this platform records no process start token")
	}

	// The record now belongs to an EARLIER process that had this pid: same
	// pid, different start token. A registration by the live process must
	// not inherit its worker id.
	raw["proc_start"] = "an-earlier-process"
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first.Path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := RegisterSession("/w", "rook")
	if err != nil {
		t.Fatal(err)
	}
	reg2, _ := readRegistration(t, second.Path)
	if !models.ValidWorkerID(reg2.WorkerID) || reg2.WorkerID == reg1.WorkerID {
		t.Fatalf("a new process inherited the old worker id: %q -> %q", reg1.WorkerID, reg2.WorkerID)
	}
}

// A record written before worker ids existed gets one minted on first use,
// persisted, and reused.
func TestTASK3613_LazyMintForAnOlderRecord(t *testing.T) {
	registryEnv(t)
	t.Setenv("CLAUDE_PID", itoa(os.Getppid()))
	rec, err := RegisterSession("/w", "rook")
	if err != nil {
		t.Fatal(err)
	}
	_, raw := readRegistration(t, rec.Path)
	delete(raw, "worker_id")
	data, _ := json.Marshal(raw)
	if err := os.WriteFile(rec.Path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	got := ResolveWorkerID()
	if !models.ValidWorkerID(got) {
		t.Fatalf("no worker id minted for an older record: %q", got)
	}
	reg, _ := readRegistration(t, rec.Path)
	if reg.WorkerID != got {
		t.Fatalf("the minted id was not persisted: record %q, resolved %q", reg.WorkerID, got)
	}
	if again := ResolveWorkerID(); again != got {
		t.Fatalf("a second resolve minted again: %q -> %q", got, again)
	}
}

func TestTASK3613_WorkerIDOverrideAndUnregistered(t *testing.T) {
	registryEnv(t)
	t.Setenv("CLAUDE_PID", itoa(os.Getppid()))

	// Unregistered: name-only, and nothing is written.
	if got := ResolveWorkerID(); got != "" {
		t.Fatalf("an unregistered session resolved worker id %q, want none", got)
	}

	const explicit = "wk_0123456789abcdef0123456789abcdef"
	t.Setenv("PAD_WORKER_ID", explicit)
	if got := ResolveWorkerID(); got != explicit {
		t.Fatalf("PAD_WORKER_ID = %q not honoured: %q", explicit, got)
	}
	t.Setenv("PAD_WORKER_ID", "not-a-worker-id")
	if got := ResolveWorkerID(); got != "" {
		t.Fatalf("a malformed PAD_WORKER_ID was used: %q", got)
	}

	c := &Client{}
	if err := c.SetWorkerID("wk_short"); err == nil {
		t.Fatal("SetWorkerID accepted a malformed id")
	}
	if err := c.SetWorkerID(explicit); err != nil || c.workerID != explicit {
		t.Fatalf("SetWorkerID: err %v, id %q", err, c.workerID)
	}
}

// keptWorkerID directly, including platforms that record no start token,
// where the TestTASK3613 registry tests above skip: with no token on either
// side the pid alone keeps the id (a /clear must keep its leases); a token
// on one side only is a different process.
func TestTASK3613_KeptWorkerIDStartTokenRules(t *testing.T) {
	const id = "wk_0123456789abcdef0123456789abcdef"
	path := t.TempDir() + "/1234.json"
	write := func(procStart string) {
		t.Helper()
		reg := map[string]any{"session_pid": 1234, "pid": 1, "cwd": "/w", "registered_at": "2026-10-12T00:00:00Z", "worker_id": id, "version": 2}
		if procStart != "" {
			reg["proc_start"] = procStart
		}
		data, _ := json.Marshal(reg)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name, recorded, current string
		keep                    bool
	}{
		{"same token", "100", "100", true},
		{"other token (pid reuse)", "100", "200", false},
		{"no token either side", "", "", true},
		{"token on the record only", "100", "", false},
		{"token on the owner only", "", "200", false},
	}
	for _, c := range cases {
		write(c.recorded)
		got := keptWorkerID(path, SessionOwner{PID: 1234, ProcStart: c.current})
		if (got == id) != c.keep {
			t.Errorf("%s: kept = %q, want keep=%v", c.name, got, c.keep)
		}
	}
}
