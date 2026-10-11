package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// commentServer answers capabilities (idempotency_keys = honorsKeys) and
// scripts the statuses of the comment POSTs it receives.
func commentServer(t *testing.T, honorsKeys bool, statuses ...int) *[]map[string]any {
	t.Helper()
	var bodies []map[string]any
	setupFormatRoutingTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/capabilities") {
			_ = json.NewEncoder(w).Encode(map[string]any{"idempotency_keys": honorsKeys})
			return
		}
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/comments") {
			_ = json.NewEncoder(w).Encode(map[string]any{})
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		bodies = append(bodies, b)
		status := http.StatusCreated
		if n := len(bodies) - 1; n < len(statuses) {
			status = statuses[n]
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status >= 400 {
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "x", "message": "m"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "c1", "body": "hi"})
	}))
	formatFlag = "table"
	return &bodies
}

func runComment(t *testing.T, args ...string) error {
	t.Helper()
	cmd := commentCmd()
	cmd.SetArgs(append([]string{"TASK-5", "hi"}, args...))
	var err error
	_ = captureStdout(t, func() { err = cmd.Execute() })
	return err
}

func TestTASK3585_CLICommentRetriesOnceWithTheSameKey(t *testing.T) {
	bodies := commentServer(t, true, http.StatusBadGateway)
	if err := runComment(t); err != nil {
		t.Fatalf("a 502 then a 201 must succeed: %v", err)
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

func TestTASK3585_CLICommentNoRetryWithoutTheCapability(t *testing.T) {
	bodies := commentServer(t, false, http.StatusBadGateway)
	if err := runComment(t); err == nil {
		t.Fatal("a 502 with no retry must fail")
	}
	if len(*bodies) != 1 {
		t.Fatalf("retried a comment against a server that ignores keys: %d attempts", len(*bodies))
	}
}

func TestTASK3585_CLICommentKeyIsGivenOrFresh(t *testing.T) {
	bodies := commentServer(t, true)
	_ = runComment(t)
	_ = runComment(t)
	_ = runComment(t, "--op-key", "mine")
	if len(*bodies) != 3 {
		t.Fatalf("attempts = %d", len(*bodies))
	}
	k0, _ := (*bodies)[0]["op_key"].(string)
	k1, _ := (*bodies)[1]["op_key"].(string)
	if k0 == "" || k0 == k1 || (*bodies)[2]["op_key"] != "mine" {
		t.Fatalf("keys = %q %q %v", k0, k1, (*bodies)[2]["op_key"])
	}
}
