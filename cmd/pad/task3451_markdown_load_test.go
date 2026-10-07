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
	return runShowMarkdownWith(t, content, "")
}

func runShowMarkdownWith(t *testing.T, content, contentState string) (string, string) {
	t.Helper()
	item := map[string]any{
		"id": "11111111-1111-1111-1111-111111111111", "workspace_id": "ws-id", "collection_id": "c-id",
		"collection_slug": "tasks", "collection_name": "Tasks", "ref": "TASK-6", "slug": "first-thing",
		"title": "First thing", "content": content, "fields": `{"status":"open"}`, "seq": 1,
		"created_at": "2026-10-06T00:00:00Z", "updated_at": "2026-10-06T00:00:00Z",
	}
	if contentState != "" {
		item["content_state"] = contentState
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
// body-only format, in either flag spelling and with the flag before or after
// `item show` (codex r1). `pad playbook show --format markdown` is not matched:
// a playbook's body IS its script.
var loadsWithMarkdown = regexp.MustCompile("item show\\b[^\n`\"]*?--format[= ]markdown|--format[= ]markdown[^\n`\"]*?item show\\b")

// The guard: no agent-facing instruction loads an item with --format markdown
// to READ it. A line may name the format only to describe the edit round trip
// it exists for, which it does by naming `update --stdin` (codex r1: a looser
// "body only" exemption would excuse a real load).
func TestNoAgentFacingTextLoadsAnItemWithMarkdown(t *testing.T) {
	for _, shape := range []string{
		"Run `pad item show <target> --format markdown` and read",
		"Run `pad item show <target> --format=markdown` and read",
		"Run `pad item show --workspace w <target> --format markdown` and read",
		"Run `pad --format markdown item show <target>` and read",
	} {
		if !loadsWithMarkdown.MatchString(shape) {
			t.Fatalf("control: the guard's pattern misses %q", shape)
		}
	}
	if loadsWithMarkdown.MatchString("`pad playbook show ship --format markdown`") {
		t.Fatal("control: a playbook's body is its script; the guard must not flag it")
	}
	root := filepath.Join("..", "..")
	var files []string
	for _, g := range []string{
		"skills/**/*.md", "plugin/**/*.md", "docs/**/*.md", "internal/mcp/*.md",
		"internal/collections/*.go", "internal/mcp/prompts_data.go", "README.md", "CLAUDE.md",
	} {
		matches, err := doublestarGlob(root, g)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, matches...)
	}
	if len(files) < 10 {
		t.Fatalf("control: the guard scans only %d files; its globs no longer find the surfaces", len(files))
	}
	for _, rel := range files {
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if loadsWithMarkdown.MatchString(line) && !strings.Contains(line, "update --stdin") {
				t.Errorf("%s:%d loads an item with --format markdown (body only, empty for a title-only item); use --agent:\n  %s",
					rel, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// doublestarGlob resolves a glob under root, where a `**/` segment matches
// any depth. Returns paths relative to root.
func doublestarGlob(root, pattern string) ([]string, error) {
	if !strings.Contains(pattern, "**/") {
		m, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(m))
		for _, f := range m {
			rel, _ := filepath.Rel(root, f)
			out = append(out, rel)
		}
		return out, nil
	}
	parts := strings.SplitN(pattern, "**/", 2)
	base, leaf := filepath.Join(root, parts[0]), parts[1]
	var out []string
	err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return filepath.SkipDir
			}
			return err
		}
		if d.IsDir() {
			if d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if ok, _ := filepath.Match(leaf, d.Name()); ok {
			rel, _ := filepath.Rel(root, path)
			out = append(out, rel)
		}
		return nil
	})
	return out, err
}

// Codex r1: an empty body whose live document is ahead is not "no body". The
// stale warning already says what to do; the empty-body line must not add a
// claim that may be false.
func TestItemShowMarkdownStaleEmptyBodyDoesNotClaimNoBody(t *testing.T) {
	stdout, stderr := runShowMarkdownWith(t, "", "applied_pending_flush")
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if strings.Contains(stderr, "has no body") {
		t.Errorf("a stale empty body was claimed to have no body: %q", stderr)
	}
	if stderr == "" {
		t.Error("control: the stale warning itself is missing")
	}
}
