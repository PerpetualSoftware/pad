package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// appendServer answers capabilities and scripts the statuses of the item
// PATCHes (appends) it receives. A success echoes the appended entry, as the
// server does, so the command's confirmation check passes.
func appendServer(t *testing.T, honorsKeys bool, statuses ...int) *[]map[string]any {
	t.Helper()
	var bodies []map[string]any
	setupFormatRoutingTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/capabilities") {
			_ = json.NewEncoder(w).Encode(map[string]any{"item_field_append": true, "idempotency_keys": honorsKeys})
			return
		}
		if r.Method != http.MethodPatch {
			_ = json.NewEncoder(w).Encode(map[string]any{"slug": "s", "ref": "TASK-5", "fields": "{}"})
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
		note := map[string]any{"id": "note_1", "summary": "s"}
		fields, _ := json.Marshal(map[string]any{"implementation_notes": []any{note}})
		_ = json.NewEncoder(w).Encode(map[string]any{"slug": "s", "ref": "TASK-5", "fields": string(fields),
			"appended": map[string]any{"implementation_note": note}})
	}))
	formatFlag = "table"
	return &bodies
}

func runNote(t *testing.T, args ...string) error {
	t.Helper()
	cmd := noteCmd()
	cmd.SetArgs(append([]string{"TASK-5", "s"}, args...))
	var err error
	_ = captureStdout(t, func() { err = cmd.Execute() })
	return err
}

func TestTASK3585_CLINoteRetriesOnceWithTheSameKey(t *testing.T) {
	bodies := appendServer(t, true, http.StatusBadGateway)
	if err := runNote(t); err != nil {
		t.Fatalf("a 502 then a 200 must succeed: %v", err)
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

func TestTASK3585_CLINoteNoRetryWithoutTheCapability(t *testing.T) {
	bodies := appendServer(t, false, http.StatusBadGateway)
	if err := runNote(t); err == nil {
		t.Fatal("a 502 with no retry must fail")
	}
	if len(*bodies) != 1 {
		t.Fatalf("retried a note against a server that ignores keys: %d attempts", len(*bodies))
	}
}
