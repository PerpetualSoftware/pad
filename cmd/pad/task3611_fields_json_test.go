package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/mcp"
)

func TestApplyFieldsJSON(t *testing.T) {
	dst := map[string]any{"status": "open"}
	if err := applyFieldsJSON(dst, `{"labels":["a","b"],"meta":{"k":1},"n":9007199254740993}`); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(dst)
	if got := string(b); got != `{"labels":["a","b"],"meta":{"k":1},"n":9007199254740993,"status":"open"}` {
		t.Fatalf("merged = %s", got)
	}
	for _, bad := range []struct{ name, raw, want string }{
		{"not json", `{`, "not valid JSON"},
		{"not an object", `["a"]`, "must be a JSON object"},
		{"null value", `{"x":null}`, "is null"},
		{"set twice", `{"status":"done"}`, "also set by another flag"},
		{"empty key", `{" ":1}`, "key is empty"},
	} {
		if err := applyFieldsJSON(map[string]any{"status": "open"}, bad.raw); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%s: err = %v, want %q", bad.name, err, bad.want)
		}
	}
	if err := applyFieldsJSON(map[string]any{}, ""); err != nil {
		t.Errorf("an absent flag must be a no-op: %v", err)
	}
}

// TASK-3611, end to end on the stdio door: a structured `fields` value on
// pad_item create and update reaches the server as JSON, not refused and not
// flattened to text. The fake server records each write's fields.
func TestStdioStructuredFieldsReachTheServer(t *testing.T) {
	var mu sync.Mutex
	var writes []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		item := `{"id":"i1","ref":"TASK-1","slug":"t-1","title":"T","collection_slug":"tasks","fields":"{}","seq":1}`
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/items/TASK-1"):
			_, _ = io.WriteString(w, item)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/collections/tasks/items"),
			r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/items/"):
			body, _ := io.ReadAll(r.Body)
			var m map[string]any
			_ = json.Unmarshal(body, &m)
			mu.Lock()
			writes = append(writes, m)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, item)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"no"}}`)
		}
	}))
	defer srv.Close()

	t.Setenv(padHelperEnv, "1")
	t.Setenv("HOME", t.TempDir())
	bin, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	d := &mcp.ExecDispatcher{Binary: bin}
	// The merge in catalog_item_fields.go hands the dispatcher the `fields`
	// object under this key, types intact; scalars also arrive as --field.
	native := map[string]any{"labels": []any{"a", "b"}, "meta": map[string]any{"k": float64(1)}, "status": "open", "effort": float64(3)}
	ctx := mcp.WithDispatchInput(context.Background(), map[string]any{"__fields_native": native})
	common := []string{"--url", srv.URL, "--workspace", "ws", "--format", "json", "--field", "status=open", "--field", "effort=3"}

	for _, tc := range []struct {
		name    string
		cmdPath []string
		argv    []string
	}{
		{"create", []string{"item", "create"}, append(append([]string{}, common...), "--", "tasks", "T")},
		{"update", []string{"item", "update"}, append(append([]string{}, common...), "--", "TASK-1")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			writes = nil
			mu.Unlock()
			res, err := d.Dispatch(ctx, tc.cmdPath, tc.argv)
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			if res.IsError {
				t.Fatalf("dispatch refused: %+v", res.StructuredContent)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(writes) != 1 {
				t.Fatalf("writes = %d, want 1", len(writes))
			}
			fields := writeFields(t, writes[0])
			if b, _ := json.Marshal(fields["labels"]); string(b) != `["a","b"]` {
				t.Errorf("labels = %s, want a JSON array", b)
			}
			if b, _ := json.Marshal(fields["meta"]); string(b) != `{"k":1}` {
				t.Errorf("meta = %s, want a JSON object", b)
			}
			// No schema reaches the CLI here (the fake answers 404), so a
			// number sent as --field effort=3 would land as the string "3"
			// (codex r1): only --fields-json keeps it a number, as remote does.
			if v, ok := fields["effort"].(float64); !ok || v != 3 {
				t.Errorf("effort = %#v, want the number 3", fields["effort"])
			}
			if fields["status"] != "open" {
				t.Errorf("status = %v, want the scalar --field to land too", fields["status"])
			}
		})
	}
}

// writeFields reads the field values out of a create (fields: JSON string) or
// update (fields_patch: object) body.
func writeFields(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	if fp, ok := body["fields_patch"].(map[string]any); ok {
		return fp
	}
	if s, ok := body["fields"].(string); ok {
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatalf("fields: %v", err)
		}
		return m
	}
	if m, ok := body["fields"].(map[string]any); ok {
		return m
	}
	t.Fatalf("no fields in write body: %v", body)
	return nil
}
