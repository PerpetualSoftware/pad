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

// TASK-3611 on the stdio door: pad_item link-pr runs `pad github link <ref>
// --pr-number ... --pr-url ...`, which writes the supplied PR through the typed
// github_pr member without touching git or `gh` (none is configured here), and
// answers with the item under --format json. unlink-pr sends clear_github_pr.
func TestStdioLinkPRSendsSuppliedMetadata(t *testing.T) {
	var mu sync.Mutex
	var patches []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		base := `{"id":"i1","ref":"TASK-1","slug":"t-1","title":"T","collection_slug":"tasks","fields":"{}"`
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/items/TASK-1"):
			mu.Lock()
			linked := len(patches) > 0
			mu.Unlock()
			if linked { // after the link, the item carries it, as the server's would
				_, _ = io.WriteString(w, `{"id":"i1","ref":"TASK-1","slug":"t-1","title":"T","collection_slug":"tasks","fields":"{\"github_pr\":{\"number\":42}}"}`)
				return
			}
			_, _ = io.WriteString(w, base+`}`)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/items/t-1"):
			body, _ := io.ReadAll(r.Body)
			var m map[string]any
			_ = json.Unmarshal(body, &m)
			mu.Lock()
			patches = append(patches, m)
			mu.Unlock()
			if _, ok := m["github_pr"]; ok {
				_, _ = io.WriteString(w, base+`,"code_context":{"pull_request":{"number":42}}}`)
				return
			}
			_, _ = io.WriteString(w, base+`}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"no"}}`)
		}
	}))
	defer srv.Close()

	t.Setenv(padHelperEnv, "1")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no git, no gh: the supplied path must not need them
	bin, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	d := &mcp.ExecDispatcher{Binary: bin}
	common := []string{"--url", srv.URL, "--workspace", "ws", "--format", "json"}

	argv := append(append([]string{}, common...), "--pr-number", "42", "--pr-url", "https://github.com/acme/app/pull/42", "--pr-state", "open", "--", "TASK-1")
	res, err := d.Dispatch(context.Background(), []string{"github", "link"}, argv)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("link refused: %+v", res.StructuredContent)
	}
	if _, ok := res.StructuredContent.(map[string]any); !ok {
		t.Errorf("--format json must answer with the item as JSON; got %T", res.StructuredContent)
	}
	res, err = d.Dispatch(context.Background(), []string{"github", "unlink"}, append(append([]string{}, common...), "--", "TASK-1"))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unlink refused: %+v", res.StructuredContent)
	}
	if _, ok := res.StructuredContent.(map[string]any); !ok {
		t.Errorf("unlink under --format json must answer with the item; got %T", res.StructuredContent)
	}
	// A lone optional flag opts into supplied mode and is refused for the
	// missing number and URL, rather than falling through to git and `gh`
	// (PATH is empty here, so a fall-through would fail differently).
	res, err = d.Dispatch(context.Background(), []string{"github", "link"}, append(append([]string{}, common...), "--pr-title", "x", "--", "TASK-1"))
	if err != nil {
		t.Fatal(err)
	}
	env, _ := res.StructuredContent.(mcp.ErrorEnvelope)
	if !res.IsError || !strings.Contains(env.Error.Message+" "+env.Error.Hint, "--pr-number and --pr-url") {
		t.Errorf("a lone --pr-title must be refused for the missing number and URL; got %+v", res.StructuredContent)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(patches) != 2 {
		t.Fatalf("patches = %d, want link then unlink", len(patches))
	}
	pr, _ := patches[0]["github_pr"].(map[string]any)
	if pr["number"] != float64(42) || pr["url"] != "https://github.com/acme/app/pull/42" || pr["state"] != "open" {
		t.Errorf("github_pr = %v, want the supplied metadata", pr)
	}
	if patches[1]["clear_github_pr"] != true {
		t.Errorf("unlink body = %v, want clear_github_pr", patches[1])
	}
}
