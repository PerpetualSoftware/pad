package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// setupPushTest mirrors setupItemOpenTest (item_open_test.go): points
// getClient()/getWorkspace() at a fake httptest server via the same
// urlFlag/workspaceFlag override the real CLI entry points read.
func setupPushTest(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	t.Setenv("HOME", t.TempDir())

	previousWorkspace := workspaceFlag
	previousURL := urlFlag
	workspaceFlag = "demo"
	urlFlag = server.URL + "/"
	t.Cleanup(func() {
		workspaceFlag = previousWorkspace
		urlFlag = previousURL
	})

	return server
}

// TestPushCmd_SendsCollapsedMessage covers the client round trip: the
// CLI posts the trimmed, newline-collapsed message to the item's /push
// endpoint and reports success.
func TestPushCmd_SendsCollapsedMessage(t *testing.T) {
	var gotPath string
	var gotBody map[string]string
	setupPushTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"ref": "TASK-5", "pushed": true})
	}))

	var output bytes.Buffer
	cmd := pushCmd()
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"TASK-5", "-m", "triage this\nwith the triage playbook"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute push: %v", err)
	}

	wantPath := "/api/v1/workspaces/demo/items/TASK-5/push"
	if gotPath != wantPath {
		t.Fatalf("path = %q, want %q", gotPath, wantPath)
	}
	wantMessage := "triage this with the triage playbook"
	if gotBody["message"] != wantMessage {
		t.Fatalf("message = %q, want %q", gotBody["message"], wantMessage)
	}
}

// TestPushCmd_FormatJSON covers the P2 finding (dispatcher review round
// 2, codex): --format json was silently ignored — the RunE hardcoded
// plain text regardless of formatFlag. Mirrors runCreateWatch's
// formatFlag == "json" branch (cmd_watch.go) and asserts the full
// PushResult shape (ref, workspace, pushed, message), not just that
// SOME JSON came out.
func TestPushCmd_FormatJSON(t *testing.T) {
	setupPushTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ref": "TASK-5", "workspace": "demo", "pushed": true, "message": "triage this",
		})
	}))
	prevFormat := formatFlag
	formatFlag = "json"
	defer func() { formatFlag = prevFormat }()

	cmd := pushCmd()
	cmd.SetArgs([]string{"TASK-5", "-m", "triage this"})

	var execErr error
	out := captureStdout(t, func() { execErr = cmd.Execute() })
	if execErr != nil {
		t.Fatalf("execute push: %v", execErr)
	}

	var result PushResultForTest
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, out)
	}
	if result.Ref != "TASK-5" || result.Workspace != "demo" || !result.Pushed || result.Message != "triage this" {
		t.Fatalf("unexpected JSON result: %+v", result)
	}
}

// PushResultForTest mirrors cli.PushResult's wire shape — cmd/pad
// doesn't import internal/cli's type back out for a test-local decode,
// so this pins the same JSON tags independently (a divergence here
// would fail this test, which is the point).
type PushResultForTest struct {
	Ref       string `json:"ref"`
	Workspace string `json:"workspace"`
	Pushed    bool   `json:"pushed"`
	Message   string `json:"message"`
}

// TestPushCmd_RequiresNonBlankMessage covers the client-side guard: an
// absent or whitespace-only -m never reaches the server at all.
func TestPushCmd_RequiresNonBlankMessage(t *testing.T) {
	called := false
	setupPushTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	for _, args := range [][]string{
		{"TASK-5"},
		{"TASK-5", "-m", ""},
		{"TASK-5", "-m", "   "},
		{"TASK-5", "-m", "\n\t "},
	} {
		called = false
		cmd := pushCmd()
		cmd.SetArgs(args)
		err := cmd.Execute()
		if err == nil {
			t.Fatalf("args %v: expected an error for a blank message, got nil", args)
		}
		if called {
			t.Fatalf("args %v: expected no HTTP call for a blank message", args)
		}
	}
}
