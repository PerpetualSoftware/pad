package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TASK-3451: `pad item show --format markdown` prints the BODY ONLY, verbatim,
// so `item update --stdin` round-trips it (IDEA-2937). Skills and playbooks
// pointed agents at it to LOAD an item, so a title-only item came back as
// 0 bytes. Readers now use `--agent`; markdown stays verbatim, and an empty
// body says so on stderr.

func runShowMarkdown(t *testing.T, content string) (string, string) {
	t.Helper()
	item := map[string]any{
		"id": "11111111-1111-1111-1111-111111111111", "workspace_id": "ws-id", "collection_id": "c-id",
		"collection_slug": "tasks", "collection_name": "Tasks", "ref": "TASK-6", "slug": "first-thing",
		"title": "First thing", "content": content, "fields": `{"status":"open"}`, "seq": 1,
		"created_at": "2026-10-06T00:00:00Z", "updated_at": "2026-10-06T00:00:00Z",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/items/TASK-6") {
			_ = json.NewEncoder(w).Encode(item)
			return
		}
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(srv.Close)
	setTempHomeMain(t)
	t.Setenv("PAD_URL", srv.URL)
	t.Setenv("PAD_TOKEN", "pad_testtoken")
	origWS, origFormat := workspaceFlag, formatFlag
	t.Cleanup(func() { workspaceFlag, formatFlag = origWS, origFormat })
	workspaceFlag, formatFlag = "ws", "markdown"

	cmd := showCmd()
	cmd.SetArgs([]string{"TASK-6"})
	var stdout string
	var execErr error
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() { execErr = cmd.Execute() })
	})
	if execErr != nil {
		t.Fatalf("item show --format markdown: %v", execErr)
	}
	return stdout, stderr
}

func TestItemShowMarkdownEmptyBodySaysSoOnStderr(t *testing.T) {
	stdout, stderr := runShowMarkdown(t, "")
	if stdout != "" {
		t.Errorf("markdown stdout must stay the body verbatim (empty), got %q", stdout)
	}
	if !strings.Contains(stderr, "TASK-6") || !strings.Contains(stderr, "--agent") {
		t.Errorf("an empty body must be named on stderr with the --agent pointer; stderr = %q", stderr)
	}
}

func TestItemShowMarkdownBodyStaysVerbatim(t *testing.T) {
	stdout, stderr := runShowMarkdown(t, "the body\n")
	if stdout != "the body\n" {
		t.Errorf("markdown stdout = %q, want the body verbatim", stdout)
	}
	if strings.Contains(stderr, "--agent") {
		t.Errorf("control: a body that is there needs no hint; stderr = %q", stderr)
	}
}

// loadsWithMarkdown matches a command that reads an ITEM through the
// body-only format. `pad playbook show --format markdown` is not matched: a
// playbook's body IS its script.
var loadsWithMarkdown = regexp.MustCompile("item show [^ `\"]+ --format markdown")

// The guard: no agent-facing instruction loads an item with --format markdown
// to READ it. A line that names the format to explain it carries "body only".
func TestNoAgentFacingTextLoadsAnItemWithMarkdown(t *testing.T) {
	if !loadsWithMarkdown.MatchString("Run `pad item show <target> --format markdown` and read") {
		t.Fatal("control: the guard's pattern no longer matches the shape it exists to catch")
	}
	root := filepath.Join("..", "..")
	files := []string{
		"skills/pad/SKILL.md",
		"plugin/skills/pad/SKILL.md",
		"internal/mcp/prompts_data.go",
		"internal/mcp/instructions.md",
	}
	more, err := filepath.Glob(filepath.Join(root, "internal", "collections", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range more {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		rel, _ := filepath.Rel(root, f)
		files = append(files, rel)
	}
	for _, rel := range files {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if loadsWithMarkdown.MatchString(line) && !strings.Contains(strings.ToLower(line), "body only") {
				t.Errorf("%s:%d loads an item with --format markdown (body only, empty for a title-only item); use --agent:\n  %s",
					rel, i+1, strings.TrimSpace(line))
			}
		}
	}
}
