package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/mcp"
)

// TASK-3611 on the stdio door: pad_item `reminders` and `unremind` run the
// CLI's `item reminders` / `item unremind`, which read and delete through the
// server, and a server refusal reaches the caller as an error, as on the CLI.
func TestStdioRemindersAndUnremind(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	refuse := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/items/TASK-1"):
			_, _ = io.WriteString(w, `{"id":"i1","ref":"TASK-1","slug":"t-1","title":"T","collection_slug":"tasks","fields":"{}"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/items/t-1/reminders"):
			_, _ = io.WriteString(w, `{"ref":"TASK-1","reminders":[{"id":"r1","item_id":"i1","remind_at":"2099-01-01T09:00:00Z"}]}`)
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/reminders/r1"):
			if refuse {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"error":{"code":"forbidden","message":"You do not have permission to do this."}}`)
				return
			}
			w.WriteHeader(http.StatusNoContent)
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
	common := []string{"--url", srv.URL, "--workspace", "ws", "--format", "json"}
	run := func(cmdPath []string, pos string) (*mcp.CallToolResult, []string) {
		t.Helper()
		mu.Lock()
		seen = nil
		mu.Unlock()
		res, err := d.Dispatch(context.Background(), cmdPath, append(append([]string{}, common...), "--", pos))
		if err != nil {
			t.Fatalf("Dispatch %v: %v", cmdPath, err)
		}
		mu.Lock()
		defer mu.Unlock()
		return res, append([]string{}, seen...)
	}

	res, reqs := run([]string{"item", "reminders"}, "TASK-1")
	if res.IsError || !strings.Contains(strings.Join(reqs, ","), "GET /api/v1/workspaces/ws/items/t-1/reminders") {
		t.Fatalf("reminders: isError=%v requests=%v", res.IsError, reqs)
	}
	res, reqs = run([]string{"item", "unremind"}, "r1")
	if res.IsError || strings.Join(reqs, ",") != "DELETE /api/v1/workspaces/ws/reminders/r1" {
		t.Fatalf("unremind: isError=%v requests=%v", res.IsError, reqs)
	}
	refuse = true
	res, _ = run([]string{"item", "unremind"}, "r1")
	if !res.IsError {
		t.Fatal("a refused disarm must reach the caller as an error")
	}
}
