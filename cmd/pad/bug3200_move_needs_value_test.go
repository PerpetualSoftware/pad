package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// BUG-3200, through the command: `pad item move` refused as
// missing_required_fields asks the copy preflight what the field needs and
// prints it, and the server's refusal is still the command's error.
func TestItemMoveRefusalPrintsWhatTheFieldNeeds(t *testing.T) {
	var preflightBody map[string]any
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/items/TASK-5/move"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"code": "missing_required_fields", "message": `Required fields missing: required field "priority" has no value`,
			}})
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/items/TASK-5/copy/preflight"):
			_ = json.NewDecoder(r.Body).Decode(&preflightBody)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(fullPreflight())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	setTempHomeMain(t)
	t.Setenv("PAD_URL", srv.URL)
	t.Setenv("PAD_TOKEN", "pad_testtoken")
	origWS, origFormat := workspaceFlag, formatFlag
	t.Cleanup(func() { workspaceFlag, formatFlag = origWS, origFormat })
	workspaceFlag, formatFlag = "ws", ""

	cmd := moveCmd()
	cmd.SetArgs([]string{"TASK-5", "bugs"})
	var execErr error
	stderr := captureStderr(t, func() {
		_ = captureStdout(t, func() { execErr = cmd.Execute() })
	})

	if execErr == nil || !strings.Contains(execErr.Error(), "Required fields missing") {
		t.Fatalf("the refusal must still be the command's error, got %v", execErr)
	}
	if preflightBody == nil {
		t.Fatalf("precondition: no preflight was asked; requests: %v", paths)
	}
	if preflightBody["target_workspace"] != "ws" || preflightBody["target_collection"] != "bugs" {
		t.Fatalf("preflight asked about %v, want ws/bugs", preflightBody)
	}
	for _, want := range []string{"priority", "--field"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
}
