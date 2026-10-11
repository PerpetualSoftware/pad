package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TASK-3585 at the CLI: every patch carries an idempotency key, and a
// transport-level failure (a 5xx) is retried ONCE with the same key; a 4xx
// answer is not retried.
func patchServer(t *testing.T, statuses ...int) *[]map[string]any {
	t.Helper()
	var bodies []map[string]any
	setupFormatRoutingTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/capabilities") {
			_ = json.NewEncoder(w).Encode(map[string]any{"item_patch": true, "idempotency_keys": true})
			return
		}
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/patch") {
			_ = json.NewEncoder(w).Encode(map[string]any{})
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		bodies = append(bodies, b)
		status := http.StatusOK
		if n := len(bodies) - 1; n < len(statuses) {
			status = statuses[n]
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status >= 400 {
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "x", "message": "m"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ref": "TASK-5", "seq": 9, "patch_id": "p1", "diff": "--- before\n+++ after\n"})
	}))
	formatFlag = "table"
	return &bodies
}

func runPatch(t *testing.T, args ...string) error {
	t.Helper()
	cmd := itemPatchCmd()
	cmd.SetArgs(append([]string{"TASK-5"}, args...))
	var err error
	_ = captureStdout(t, func() { err = cmd.Execute() })
	return err
}

func TestTASK3585_CLIRetriesA5xxOnceWithTheSameKey(t *testing.T) {
	bodies := patchServer(t, http.StatusServiceUnavailable)
	if err := runPatch(t, "--old", "a", "--new", "b"); err != nil {
		t.Fatalf("a 503 then a 200 must succeed: %v", err)
	}
	if len(*bodies) != 2 {
		t.Fatalf("want exactly one retry, got %d attempts", len(*bodies))
	}
	k1, _ := (*bodies)[0]["op_key"].(string)
	k2, _ := (*bodies)[1]["op_key"].(string)
	if k1 == "" || k1 != k2 {
		t.Fatalf("the retry must reuse the key: %q vs %q", k1, k2)
	}
}

func TestTASK3585_CLIDoesNotRetryA4xx(t *testing.T) {
	bodies := patchServer(t, http.StatusConflict)
	if err := runPatch(t, "--old", "a", "--new", "b"); err == nil {
		t.Fatal("a 409 must fail")
	}
	if len(*bodies) != 1 {
		t.Fatalf("a 4xx was retried: %d attempts", len(*bodies))
	}
}

func TestTASK3585_CLIKeysDifferPerInvocationUnlessGiven(t *testing.T) {
	bodies := patchServer(t)
	_ = runPatch(t, "--old", "a", "--new", "b")
	_ = runPatch(t, "--old", "a", "--new", "b")
	_ = runPatch(t, "--old", "a", "--new", "b", "--op-key", "mine")
	if len(*bodies) != 3 {
		t.Fatalf("attempts = %d", len(*bodies))
	}
	k0, _ := (*bodies)[0]["op_key"].(string)
	k1, _ := (*bodies)[1]["op_key"].(string)
	if k0 == k1 || (*bodies)[2]["op_key"] != "mine" {
		t.Fatalf("keys = %q %q %v", k0, k1, (*bodies)[2]["op_key"])
	}
}
