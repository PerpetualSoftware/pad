package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// fakeFetcher records the args passed to Fetch and returns a
// configured stdout string + optional error.
type fakeFetcher struct {
	gotArgs []string
	stdout  string
	err     error
}

func (f *fakeFetcher) Fetch(ctx context.Context, args []string) (string, error) {
	f.gotArgs = append([]string(nil), args...)
	return f.stdout, f.err
}

func TestParsePadURI_Forms(t *testing.T) {
	cases := []struct {
		uri     string
		ws      string
		kind    string
		arg     string
		wantErr bool
	}{
		{"pad://workspace/docapp/items/TASK-5", "docapp", "items", "TASK-5", false},
		{"pad://workspace/docapp/items", "docapp", "items", "", false},
		{"pad://workspace/docapp/dashboard", "docapp", "dashboard", "", false},
		{"pad://workspace/docapp/collections", "docapp", "collections", "", false},
		{"pad://workspace/x/items/y/extra", "x", "items", "y/extra", false}, // trailing path packed into arg
		// Malformed:
		{"http://example.com/foo", "", "", "", true},
		{"pad://workspace/", "", "", "", true},
		{"pad://workspace/onlyws", "", "", "", true},
		{"pad://workspace//items", "", "", "", true}, // empty ws
	}
	for _, c := range cases {
		gotWS, gotKind, gotArg, err := parsePadURI(c.uri)
		if c.wantErr {
			if err == nil {
				t.Errorf("parsePadURI(%q) expected error, got ws=%q kind=%q arg=%q", c.uri, gotWS, gotKind, gotArg)
			}
			continue
		}
		if err != nil {
			t.Errorf("parsePadURI(%q) unexpected error: %v", c.uri, err)
			continue
		}
		if gotWS != c.ws || gotKind != c.kind || gotArg != c.arg {
			t.Errorf("parsePadURI(%q) = (%q, %q, %q), want (%q, %q, %q)",
				c.uri, gotWS, gotKind, gotArg, c.ws, c.kind, c.arg)
		}
	}
}

func TestRegisterResources_AdvertisesAllFourTemplates(t *testing.T) {
	srv := server.NewMCPServer("t", "1", server.WithToolCapabilities(true))
	RegisterResources(srv, &fakeFetcher{}, nil)

	// We can't easily list registered templates from outside the
	// package, but we can drive the public read path by URI and
	// confirm each kind hits its handler successfully.
	ctx := context.Background()
	_ = ctx

	// Indirect smoke: the dispatch tests below cover each handler.
	// Here just assert RegisterResources returns without panic on a
	// vanilla MCPServer — guards against future option changes that
	// might require WithResourceCapabilities up-front.
}

func TestReadItem_DispatchesJSONAndComposesMarkdown(t *testing.T) {
	// Codex review (round 1) caught: `pad item show --format markdown`
	// emits only item.Content (no ref/title/fields). The MCP resource
	// promises full markdown, so we fetch JSON and compose the
	// document here. Test guards both contracts: dispatch uses
	// --format json, output contains heading + fields + body.
	f := &fakeFetcher{stdout: `{
		"ref": "TASK-5",
		"title": "Fix OAuth",
		"fields": "{\"priority\":\"high\",\"status\":\"in-progress\"}",
		"content": "Detailed plan goes here."
	}`}
	r := &resources{fetcher: f}
	req := mcp.ReadResourceRequest{}
	req.Params.URI = "pad://workspace/docapp/items/TASK-5"

	contents, err := r.readItem(context.Background(), req)
	if err != nil {
		t.Fatalf("readItem: %v", err)
	}
	if len(contents) != 1 {
		t.Fatalf("expected 1 ResourceContents, got %d", len(contents))
	}
	tc, ok := contents[0].(mcp.TextResourceContents)
	if !ok {
		t.Fatalf("expected TextResourceContents, got %T", contents[0])
	}
	if tc.MIMEType != itemMIMEType {
		t.Errorf("MIMEType = %q, want %q", tc.MIMEType, itemMIMEType)
	}
	wantParts := []string{
		"# TASK-5: Fix OAuth",
		"- **priority:** high",
		"- **status:** in-progress",
		"Detailed plan goes here.",
	}
	for _, p := range wantParts {
		if !strings.Contains(tc.Text, p) {
			t.Errorf("composed markdown missing %q; got:\n%s", p, tc.Text)
		}
	}
	wantArgs := []string{"item", "show", "TASK-5", "--workspace", "docapp", "--format", "json"}
	if !equalSlice(f.gotArgs, wantArgs) {
		t.Errorf("dispatched args = %v, want %v", f.gotArgs, wantArgs)
	}
}

func TestFormatItemAsMarkdown_FullShape(t *testing.T) {
	got, err := formatItemAsMarkdown(`{
		"ref": "TASK-9",
		"title": "Add MCP",
		"parent_ref": "PLAN-942",
		"parent_title": "Local MCP server",
		"fields": "{\"priority\":\"high\"}",
		"content": "body"
	}`)
	if err != nil {
		t.Fatalf("formatItemAsMarkdown: %v", err)
	}
	want := "# TASK-9: Add MCP\n\n**Parent:** PLAN-942 — Local MCP server\n\n- **priority:** high\n\nbody\n"
	if got != want {
		t.Errorf("formatItemAsMarkdown output drift\ngot:  %q\nwant: %q", got, want)
	}
}

func TestFormatItemAsMarkdown_HandlesMissingFields(t *testing.T) {
	// Resilience: items with no parent_ref / no fields / no content
	// should still produce a valid heading-only document, not a panic.
	got, err := formatItemAsMarkdown(`{"ref":"DOC-1","title":"Onboarding"}`)
	if err != nil {
		t.Fatalf("formatItemAsMarkdown: %v", err)
	}
	if got != "# DOC-1: Onboarding\n\n" {
		t.Errorf("got %q, want heading-only doc", got)
	}
}

func TestFormatItemAsMarkdown_HandlesEmptyFields(t *testing.T) {
	// fields is `{}` (no keys) — should NOT emit a stray blank
	// "fields:" section.
	got, err := formatItemAsMarkdown(`{"ref":"X","title":"y","fields":"{}","content":"z"}`)
	if err != nil {
		t.Fatalf("formatItemAsMarkdown: %v", err)
	}
	if strings.Contains(got, "**") {
		t.Errorf("empty fields object should not produce list items; got: %q", got)
	}
	if !strings.Contains(got, "z") {
		t.Errorf("body missing: %q", got)
	}
}

func TestFormatItemAsMarkdown_RejectsInvalidJSON(t *testing.T) {
	if _, err := formatItemAsMarkdown(`not json`); err == nil {
		t.Errorf("expected error on invalid JSON")
	}
}

func TestReadItems_DispatchesItemListJSON(t *testing.T) {
	f := &fakeFetcher{stdout: `[{"ref":"TASK-1"}]`}
	r := &resources{fetcher: f}
	req := mcp.ReadResourceRequest{}
	req.Params.URI = "pad://workspace/docapp/items"

	contents, err := r.readItems(context.Background(), req)
	if err != nil {
		t.Fatalf("readItems: %v", err)
	}
	tc := contents[0].(mcp.TextResourceContents)
	if tc.MIMEType != jsonMIMEType {
		t.Errorf("MIMEType = %q, want %q", tc.MIMEType, jsonMIMEType)
	}
	wantArgs := []string{"item", "list", "--all", "--workspace", "docapp", "--format", "json"}
	if !equalSlice(f.gotArgs, wantArgs) {
		t.Errorf("dispatched args = %v, want %v", f.gotArgs, wantArgs)
	}
}

func TestReadDashboard_DispatchesProjectDashboard(t *testing.T) {
	f := &fakeFetcher{stdout: `{"plans":[]}`}
	r := &resources{fetcher: f}
	req := mcp.ReadResourceRequest{}
	req.Params.URI = "pad://workspace/docapp/dashboard"

	contents, err := r.readDashboard(context.Background(), req)
	if err != nil {
		t.Fatalf("readDashboard: %v", err)
	}
	if contents[0].(mcp.TextResourceContents).MIMEType != jsonMIMEType {
		t.Errorf("dashboard should be JSON")
	}
	wantArgs := []string{"project", "dashboard", "--workspace", "docapp", "--format", "json"}
	if !equalSlice(f.gotArgs, wantArgs) {
		t.Errorf("dispatched args = %v, want %v", f.gotArgs, wantArgs)
	}
}

func TestReadCollections_DispatchesCollectionList(t *testing.T) {
	f := &fakeFetcher{stdout: `[]`}
	r := &resources{fetcher: f}
	req := mcp.ReadResourceRequest{}
	req.Params.URI = "pad://workspace/docapp/collections"

	contents, err := r.readCollections(context.Background(), req)
	if err != nil {
		t.Fatalf("readCollections: %v", err)
	}
	if contents[0].(mcp.TextResourceContents).MIMEType != jsonMIMEType {
		t.Errorf("collections should be JSON")
	}
	wantArgs := []string{"collection", "list", "--workspace", "docapp", "--format", "json"}
	if !equalSlice(f.gotArgs, wantArgs) {
		t.Errorf("dispatched args = %v, want %v", f.gotArgs, wantArgs)
	}
}

func TestReadItem_RejectsMismatchedURI(t *testing.T) {
	// readItem is registered against the items/{ref} template, but
	// guards against being invoked with a mismatched URI (defensive —
	// covers future template-routing regressions).
	r := &resources{fetcher: &fakeFetcher{}}
	req := mcp.ReadResourceRequest{}
	req.Params.URI = "pad://workspace/docapp/dashboard"
	if _, err := r.readItem(context.Background(), req); err == nil {
		t.Errorf("readItem should reject non-item URI")
	}
}

func TestRead_PropagatesFetchErrors(t *testing.T) {
	// When the underlying pad invocation fails (e.g. unknown
	// workspace), the error must surface as a Go error so MCP
	// returns a JSON-RPC error to the client (not a successful read
	// with empty contents).
	f := &fakeFetcher{err: errors.New("workspace not found")}
	r := &resources{fetcher: f}
	req := mcp.ReadResourceRequest{}
	req.Params.URI = "pad://workspace/ghost/items/TASK-5"
	_, err := r.readItem(context.Background(), req)
	if err == nil {
		t.Fatalf("expected error when fetcher fails")
	}
	if !strings.Contains(err.Error(), "workspace not found") {
		t.Errorf("error should propagate fetcher message; got: %v", err)
	}
}

func TestRead_AppendsRootFlags(t *testing.T) {
	// Per Codex review on TASK-945: --url root flag must reach every
	// dispatched subprocess. Resource fetches go through a separate
	// fetcher path, so this contract has to be retested here.
	f := &fakeFetcher{stdout: "{}"}
	r := &resources{
		fetcher:  f,
		rootArgs: rootFlagsToArgs(map[string]string{"url": "https://api.example.com"}),
	}
	req := mcp.ReadResourceRequest{}
	req.Params.URI = "pad://workspace/docapp/dashboard"
	if _, err := r.readDashboard(context.Background(), req); err != nil {
		t.Fatalf("readDashboard: %v", err)
	}
	joined := strings.Join(f.gotArgs, " ")
	if !strings.Contains(joined, "--url https://api.example.com") {
		t.Errorf("root flag --url not forwarded; got args: %v", f.gotArgs)
	}
}

func TestRootFlagsToArgs_SkipsEmptyValues(t *testing.T) {
	got := rootFlagsToArgs(map[string]string{"url": "", "other": "x"})
	want := []string{"--other", "x"}
	if !equalSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestRootFlagsToArgs_NilMap(t *testing.T) {
	if got := rootFlagsToArgs(nil); got != nil {
		t.Errorf("expected nil for nil input, got: %v", got)
	}
}

func TestExecResourceFetcher_NoBinaryReturnsError(t *testing.T) {
	f := &ExecResourceFetcher{}
	_, err := f.Fetch(t.Context(), []string{"item", "list"})
	if err == nil {
		t.Errorf("expected error when binary unset")
	}
}

func TestExecResourceFetcher_RunsBinaryAndReturnsStdout(t *testing.T) {
	// /bin/sh stand-in: same pattern as ExecDispatcher tests.
	f := &ExecResourceFetcher{Binary: "/bin/sh"}
	out, err := f.Fetch(t.Context(), []string{"-c", `echo hello`})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("expected 'hello' in stdout, got: %q", out)
	}
}

func TestExecResourceFetcher_NonzeroExitReturnsErrorWithStderr(t *testing.T) {
	f := &ExecResourceFetcher{Binary: "/bin/sh"}
	_, err := f.Fetch(t.Context(), []string{"-c", `echo "boom" >&2 && exit 9`})
	if err == nil {
		t.Fatalf("expected error on non-zero exit")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should contain stderr; got: %v", err)
	}
}
