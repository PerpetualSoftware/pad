package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for `pad item claim` / `pad item release` (#1221) — the CLI face
// of the execution lease. The stub answers the two endpoints; the tests
// drive the real commands and pin what the automation-facing output and
// exit behaviour look like, because sweep scripts branch on them.

type stubLeaseServer struct {
	*httptest.Server
	claimBodies   []map[string]any
	releaseBodies []map[string]any
	holdConflict  bool // when true, claim answers 409 lease_held
}

func newStubLeaseServer(t *testing.T) *stubLeaseServer {
	t.Helper()
	s := &stubLeaseServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/ws1/items/TASK-5/claim", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.claimBodies = append(s.claimBodies, body)
		if s.holdConflict {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"code":    "lease_held",
					"message": `item is leased to "other-runner" until 2026-09-02T22:00:00Z`,
					"details": map[string]any{
						"ref": "TASK-5", "holder": "other-runner",
						"acquired_at": "2026-09-02T21:45:00Z",
						"expires_at":  "2026-09-02T22:00:00Z",
					},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ref": "TASK-5",
			"lease": map[string]any{
				"holder":      "sweep-runner",
				"acquired_at": "2026-09-02T21:30:00Z",
				"expires_at":  "2026-09-02T21:45:00Z",
			},
		})
	})
	mux.HandleFunc("/api/v1/workspaces/ws1/items/TASK-5/release", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.releaseBodies = append(s.releaseBodies, body)
		released := len(s.claimBodies) > 0 // released=true only if something was claimed
		_ = json.NewEncoder(w).Encode(map[string]any{"ref": "TASK-5", "released": released})
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func setupLeaseCLI(t *testing.T) *stubLeaseServer {
	t.Helper()
	setTempHomeMain(t)
	srv := newStubLeaseServer(t)
	t.Setenv("PAD_URL", srv.URL)
	t.Setenv("PAD_TOKEN", "pad_envtoken")
	// Workspace detection walks up from CWD for a .pad.toml —
	// setTempHomeMain already chdir'd to a fresh temp dir, so a minimal
	// pin there routes getWorkspace() to the stub's workspace.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".pad.toml"), []byte("workspace = \"ws1\"\n"), 0644); err != nil {
		t.Fatalf("write .pad.toml: %v", err)
	}
	return srv
}

// claim sends holder + ttl (converted to ttl_seconds) and confirms the
// acquired lease with holder and expiry — the sweep's success signal.
func TestItemClaim_SendsTTLAndPrintsLease(t *testing.T) {
	srv := setupLeaseCLI(t)

	cmd := itemClaimCmd()
	cmd.SetArgs([]string{"TASK-5", "--holder", "sweep-runner", "--ttl", "10m"})
	var runErr error
	out := captureStdout(t, func() {
		runErr = cmd.Execute()
	})
	if runErr != nil {
		t.Fatalf("claim: %v", runErr)
	}
	if len(srv.claimBodies) != 1 {
		t.Fatalf("expected one claim POST, got %d", len(srv.claimBodies))
	}
	body := srv.claimBodies[0]
	if body["holder"] != "sweep-runner" {
		t.Errorf("posted holder = %v, want sweep-runner", body["holder"])
	}
	if n, _ := body["ttl_seconds"].(float64); int(n) != 600 {
		t.Errorf("posted ttl_seconds = %v, want 600", body["ttl_seconds"])
	}
	if !strings.Contains(out, "sweep-runner") || !strings.Contains(strings.ToLower(out), "lease") {
		t.Errorf("output should confirm the lease and holder:\n%s", out)
	}
}

// A contended claim exits non-zero with the holder and expiry in the
// error — the loser must be able to log WHO holds the item without a
// second call.
func TestItemClaim_ContendedExitsWithHolder(t *testing.T) {
	srv := setupLeaseCLI(t)
	srv.holdConflict = true

	cmd := itemClaimCmd()
	cmd.SetArgs([]string{"TASK-5"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var runErr error
	captureStdout(t, func() {
		runErr = cmd.Execute()
	})
	if runErr == nil {
		t.Fatal("contended claim must exit with an error")
	}
	if !strings.Contains(runErr.Error(), "other-runner") {
		t.Errorf("error should name the live holder, got %q", runErr.Error())
	}
}

// An unparseable --ttl fails locally; the server is never asked.
func TestItemClaim_BadTTLFailsLocally(t *testing.T) {
	srv := setupLeaseCLI(t)

	cmd := itemClaimCmd()
	cmd.SetArgs([]string{"TASK-5", "--ttl", "banana"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var runErr error
	captureStdout(t, func() {
		runErr = cmd.Execute()
	})
	if runErr == nil {
		t.Fatal("expected a parse error for --ttl banana")
	}
	if len(srv.claimBodies) != 0 {
		t.Errorf("no claim should reach the server on a bad ttl, got %d", len(srv.claimBodies))
	}
}

// release distinguishes a real release from the idempotent no-op — both
// succeed (exit 0), but the words differ so a human reading sweep logs
// can tell them apart.
func TestItemRelease_ReportsReleasedVsNoop(t *testing.T) {
	srv := setupLeaseCLI(t)

	// No claim yet: the stub answers released=false.
	cmd := itemReleaseCmd()
	cmd.SetArgs([]string{"TASK-5"})
	var runErr error
	out := captureStdout(t, func() {
		runErr = cmd.Execute()
	})
	if runErr != nil {
		t.Fatalf("no-op release must still exit 0: %v", runErr)
	}
	if !strings.Contains(strings.ToLower(out), "no live lease") {
		t.Errorf("no-op release should say nothing was held:\n%s", out)
	}

	// After a claim: released=true.
	claim := itemClaimCmd()
	claim.SetArgs([]string{"TASK-5"})
	captureStdout(t, func() { _ = claim.Execute() })

	cmd = itemReleaseCmd()
	cmd.SetArgs([]string{"TASK-5", "--holder", "sweep-runner"})
	out = captureStdout(t, func() {
		runErr = cmd.Execute()
	})
	if runErr != nil {
		t.Fatalf("release: %v", runErr)
	}
	if len(srv.releaseBodies) != 2 || srv.releaseBodies[1]["holder"] != "sweep-runner" {
		t.Errorf("release bodies = %v, want second with holder sweep-runner", srv.releaseBodies)
	}
	if !strings.Contains(strings.ToLower(out), "released") {
		t.Errorf("real release should say released:\n%s", out)
	}
}
