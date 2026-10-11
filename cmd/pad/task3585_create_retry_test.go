package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// createServer answers capabilities (idempotency_keys = honorsKeys) and
// scripts the statuses of the create POSTs it receives.
func createServer(t *testing.T, honorsKeys bool, statuses ...int) *[]map[string]any {
	t.Helper()
	var bodies []map[string]any
	setupFormatRoutingTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/capabilities") {
			_ = json.NewEncoder(w).Encode(map[string]any{"item_patch": true, "idempotency_keys": honorsKeys, "collection_resolution": true})
			return
		}
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/items") {
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
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "i1", "slug": "s", "title": "t", "collection_slug": "tasks", "ref": "TASK-5"})
	}))
	formatFlag = "table"
	return &bodies
}

func runCreate(t *testing.T, args ...string) error {
	t.Helper()
	cmd := createCmd()
	cmd.SetArgs(append([]string{"tasks", "Keyed"}, args...))
	var err error
	_ = captureStdout(t, func() { err = cmd.Execute() })
	return err
}

func TestTASK3585_CLICreateRetriesOnceWithTheSameKey(t *testing.T) {
	bodies := createServer(t, true, http.StatusBadGateway)
	if err := runCreate(t); err != nil {
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

// A server without idempotency_keys ignores op_key, so a retried create
// there would make a second item: no capability, no retry.
func TestTASK3585_CLICreateNoRetryWithoutTheCapability(t *testing.T) {
	bodies := createServer(t, false, http.StatusBadGateway)
	if err := runCreate(t); err == nil {
		t.Fatal("a 502 with no retry must fail")
	}
	if len(*bodies) != 1 {
		t.Fatalf("retried a create against a server that ignores keys: %d attempts", len(*bodies))
	}
}

func TestTASK3585_CLICreateDoesNotRetryA4xx(t *testing.T) {
	bodies := createServer(t, true, http.StatusConflict)
	if err := runCreate(t); err == nil {
		t.Fatal("a 409 must fail")
	}
	if len(*bodies) != 1 {
		t.Fatalf("a 4xx was retried: %d attempts", len(*bodies))
	}
}

func TestTASK3585_CLICreateKeyIsGivenOrFresh(t *testing.T) {
	bodies := createServer(t, true)
	_ = runCreate(t)
	_ = runCreate(t)
	_ = runCreate(t, "--op-key", "mine")
	if len(*bodies) != 3 {
		t.Fatalf("attempts = %d", len(*bodies))
	}
	k0, _ := (*bodies)[0]["op_key"].(string)
	k1, _ := (*bodies)[1]["op_key"].(string)
	if k0 == "" || k0 == k1 || (*bodies)[2]["op_key"] != "mine" {
		t.Fatalf("keys = %q %q %v", k0, k1, (*bodies)[2]["op_key"])
	}
}
