package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// End-to-end coverage of the --format routing itself, not just the render
// helpers: the command is executed through cobra against an httptest server, so
// a future edit that drops the markdown branch from a RunE fails here even
// though the renderer tests still pass. Requested by @xarmian on PR #1070.
//
// Follows the house pattern from item_open_test.go (httptest + package flag
// vars), with one addition: USERPROFILE is set alongside HOME because Go's
// os.UserHomeDir reads USERPROFILE on Windows, and the credential store resolves
// through it. Setting only HOME leaves the store pointed at the developer's real
// ~/.pad on Windows.
func setupFormatRoutingTest(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	prevWorkspace, prevURL, prevFormat := workspaceFlag, urlFlag, formatFlag
	workspaceFlag = "demo"
	urlFlag = server.URL + "/"
	t.Cleanup(func() {
		workspaceFlag, urlFlag, formatFlag = prevWorkspace, prevURL, prevFormat
	})

	return server
}

// captureStdout runs fn with os.Stdout redirected and returns what it wrote.
// The list commands print through fmt/os.Stdout rather than cmd.OutOrStdout,
// so cobra's SetOut isn't enough here.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	_ = w.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	return buf.String()
}

func routingTestItems() []models.Item {
	one, two := 1, 2
	return []models.Item{
		{
			Slug: "first-task", CollectionSlug: "tasks", CollectionName: "Tasks",
			CollectionIcon: "✓", CollectionPrefix: "TASK", ItemNumber: &one,
			Title: "first task", Fields: `{"status":"ready","priority":"high"}`,
		},
		{
			Slug: "an-idea", CollectionSlug: "ideas", CollectionName: "Ideas",
			CollectionIcon: "💡", CollectionPrefix: "IDEA", ItemNumber: &two,
			Title: "an idea", Fields: `{"status":"new"}`,
		},
	}
}

func routingTestHandler(items []models.Item) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/items"):
			_ = json.NewEncoder(w).Encode(items)
		default:
			http.NotFound(w, r)
		}
	})
}

func TestItemListRoutesToMarkdown(t *testing.T) {
	setupFormatRoutingTest(t, routingTestHandler(routingTestItems()))
	formatFlag = "markdown"

	cmd := listCmd()
	cmd.SetArgs(nil)

	var execErr error
	out := captureStdout(t, func() { execErr = cmd.Execute() })
	if execErr != nil {
		t.Fatalf("execute item list --format markdown: %v", execErr)
	}

	// Grouped markdown: a heading per collection, each with a markdown table.
	for _, want := range []string{
		"## ✓ Tasks (1)",
		"## 💡 Ideas (1)",
		"| Ref | Title | Status | Priority | Collection | Updated |",
		"| TASK-1 |",
		"| IDEA-2 |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown output missing %q:\n%s", want, out)
		}
	}
	// The table renderer's column headers must not appear.
	if strings.Contains(out, "PRIORITY") {
		t.Errorf("table output leaked into the markdown path:\n%s", out)
	}
}

func TestItemListRoutesToTableByDefault(t *testing.T) {
	setupFormatRoutingTest(t, routingTestHandler(routingTestItems()))
	formatFlag = "table"

	cmd := listCmd()
	cmd.SetArgs(nil)

	var execErr error
	out := captureStdout(t, func() { execErr = cmd.Execute() })
	if execErr != nil {
		t.Fatalf("execute item list: %v", execErr)
	}

	if strings.Contains(out, "| Ref | Title |") {
		t.Errorf("markdown table leaked into the default table path:\n%s", out)
	}
	if !strings.Contains(out, "TASK-1") {
		t.Errorf("table output missing the item ref:\n%s", out)
	}
}

func TestCollectionListRoutesToMarkdown(t *testing.T) {
	colls := []models.Collection{
		{Name: "Tasks", Slug: "tasks", Icon: "✓", ItemCount: 3, IsDefault: true},
	}
	setupFormatRoutingTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/collections") {
			_ = json.NewEncoder(w).Encode(colls)
			return
		}
		http.NotFound(w, r)
	}))
	formatFlag = "markdown"

	cmd := collectionsCmd()
	cmd.SetArgs(nil)

	var execErr error
	out := captureStdout(t, func() { execErr = cmd.Execute() })
	if execErr != nil {
		t.Fatalf("execute collection list --format markdown: %v", execErr)
	}

	for _, want := range []string{"| Name | Slug | Items | Default |", "| ✓ Tasks | tasks | 3 | yes |"} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "NAME") {
		t.Errorf("table output leaked into the markdown path:\n%s", out)
	}
}
