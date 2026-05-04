package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/collections"
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

// TestReadWorkspaces_DispatchesWorkspaceListJSON exercises the new
// pad://workspaces resource (TASK-974). It must shell out to the
// JSON output added to `pad workspace list` in PR #357 so the
// resource shape stays in lockstep with classifyExecError's
// available_workspaces enrichment (both consume the same JSON).
func TestReadWorkspaces_DispatchesWorkspaceListJSON(t *testing.T) {
	body := `[{"slug":"docapp","name":"Pad","default":true},{"slug":"pad-web","name":"Marketing"}]`
	fetcher := &fakeFetcher{stdout: body}
	srv := server.NewMCPServer("t", "1", server.WithResourceCapabilities(false, false))
	RegisterResources(srv, fetcher, nil)

	got := readResourceJSON(t, srv, WorkspacesURI)
	if got != body {
		t.Errorf("body = %q, want %q", got, body)
	}
	wantArgs := []string{"workspace", "list", "--format", "json"}
	if !equalSlice(fetcher.gotArgs, wantArgs) {
		t.Errorf("fetched args = %v, want %v", fetcher.gotArgs, wantArgs)
	}
}

// TestReadWorkspaces_RejectsWrongURI confirms the handler validates
// the URI it was bound to. Without this guard a future refactor that
// reuses readWorkspaces under a different URI registration would
// silently succeed.
func TestReadWorkspaces_RejectsWrongURI(t *testing.T) {
	r := &resources{fetcher: &fakeFetcher{stdout: `[]`}}
	req := mcp.ReadResourceRequest{}
	req.Params.URI = "pad://wrong/uri"
	_, err := r.readWorkspaces(context.Background(), req)
	if err == nil {
		t.Errorf("expected error for non-WorkspacesURI request")
	}
}

// TestReadWorkspaces_PropagatesFetcherError ensures the handler
// surfaces fetcher failures (e.g. CLI exit non-zero, no auth) as
// proper resource-read errors rather than swallowing them. MCP
// clients display these directly.
func TestReadWorkspaces_PropagatesFetcherError(t *testing.T) {
	fetcher := &fakeFetcher{err: errors.New("not authenticated")}
	srv := server.NewMCPServer("t", "1", server.WithResourceCapabilities(false, false))
	RegisterResources(srv, fetcher, nil)

	// Drive resources/read directly via HandleMessage; assert error
	// surfaces, not stdout.
	reqJSON := []byte(`{
		"jsonrpc": "2.0",
		"id": 1,
		"method": "resources/read",
		"params": { "uri": "` + WorkspacesURI + `" }
	}`)
	resp := srv.HandleMessage(context.Background(), reqJSON)
	if resp == nil {
		t.Fatalf("HandleMessage returned nil")
	}
	// The mcp-go server packages the handler's error into a JSON-RPC
	// error response. We just need to ensure the call wasn't a
	// success — a string-search on the JSON output is sufficient.
	enc, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(enc), "not authenticated") {
		t.Errorf("expected fetcher error to surface; got %s", enc)
	}
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

// TestReadItem_PreservesIDEAOneOnboardingBodyVerbatim is a contract test
// for the full pipeline that MCP clients (Claude Desktop, Cursor,
// Windsurf, …) traverse when an agent says "use pad to get IDEA-1":
//
//  1. The MCP resource handler dispatches `pad item show IDEA-1
//     --format json` via the fetcher.
//  2. `formatItemAsMarkdown` composes a markdown document from the
//     JSON response — heading + fields + body.
//
// The contract: the body the workspace seeder ships in
// `collections.StartupOnboardingItems()` (PLAN-1131 / DOC-1139) MUST
// reach the agent verbatim. Drift here means agents see a truncated
// or reformatted onboarding script, breaking the seeded experience
// silently — exactly the kind of regression the task surfaces.
//
// This test pulls the IDEA-1 body from the collections package
// directly, simulates the `pad item show --format json` envelope,
// runs it through readItem, and asserts every meaningful section is
// preserved. Specifically guarded:
//
//   - The opening greeting and the "What I'd find useful" section
//     (tells the agent how to behave).
//   - The schema-valid "mark this idea implemented" instruction —
//     guards against the round-1 fix on PR #402 silently regressing
//     to "mark me done" (which is invalid for the Ideas collection).
//   - The "If I've already done this before" section (the body's
//     idempotency contract for re-runs).
//   - Code-fenced commands (backtick `pad project dashboard` etc.)
//     and em-dashes — the kind of characters a bad transformation
//     might mangle.
func TestReadItem_PreservesIDEAOneOnboardingBodyVerbatim(t *testing.T) {
	seeds := collections.StartupOnboardingItems()
	var ideaSeed *collections.SeedItem
	for i := range seeds {
		if seeds[i].CollectionSlug == "ideas" {
			ideaSeed = &seeds[i]
			break
		}
	}
	if ideaSeed == nil {
		t.Fatal("expected an Ideas seed item in StartupOnboardingItems; the onboarding seeder shape changed and this test (and the design contract it locks down) needs to follow")
	}

	envelope := map[string]any{
		"ref":             "IDEA-1",
		"title":           ideaSeed.Title,
		"fields":          ideaSeed.Fields,
		"content":         ideaSeed.Content,
		"collection_slug": "ideas",
	}
	envBytes, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	f := &fakeFetcher{stdout: string(envBytes)}
	r := &resources{fetcher: f}
	req := mcp.ReadResourceRequest{}
	req.Params.URI = "pad://workspace/docapp/items/IDEA-1"

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

	// The composed heading wraps the body. The body's own H1 (the
	// seeded title) follows in the body section — not stripped.
	wantHeading := "# IDEA-1: " + ideaSeed.Title
	if !strings.Contains(tc.Text, wantHeading) {
		t.Errorf("composed markdown missing heading %q; got:\n%s", wantHeading, tc.Text)
	}

	// The full body must be present verbatim. Substring-checking the
	// entire body (rather than asserting equality of the composed
	// document) lets future revisions of `formatItemAsMarkdown` reorder
	// metadata bullets / parent-link rendering without breaking this
	// contract — the contract is on the body content, not on layout.
	if !strings.Contains(tc.Text, ideaSeed.Content) {
		t.Errorf("composed markdown does not contain the full IDEA-1 onboarding body verbatim; the resource pipeline is mangling content somewhere.\n\nwant body to appear:\n%s\n\ngot:\n%s", ideaSeed.Content, tc.Text)
	}

	// Specific sections we depend on agent-side. Failing any of these
	// means the body shape changed and the design doc / hint copy may
	// have drifted out of sync — failure should prompt a re-read of
	// PLAN-1131 / DOC-1139 before "fixing" the test.
	requiredFragments := []string{
		"# Welcome — let's get this place set up", // body's own H1, kept distinct from the resource heading
		"## What I'd find useful",                 // tells the agent how to behave
		"Then mark this idea implemented",         // schema-valid terminal status — guards round-1 fix on PR #402
		"## If I've already done this before",     // body-side idempotency contract for re-runs
		"`pad project dashboard`",                 // code-fenced command must survive
	}
	for _, frag := range requiredFragments {
		if !strings.Contains(tc.Text, frag) {
			t.Errorf("composed markdown missing required fragment %q; the IDEA-1 body shape may have drifted from PLAN-1131 / DOC-1139", frag)
		}
	}

	// Status field must surface in the metadata bullets so MCP clients
	// that surface fields prominently (resource listings) can show that
	// IDEA-1 is `new` and the user hasn't engaged yet.
	if !strings.Contains(tc.Text, "- **status:** new") {
		t.Errorf("composed markdown missing `- **status:** new` field row; metadata rendering broke")
	}

	wantArgs := []string{"item", "show", "IDEA-1", "--workspace", "docapp", "--format", "json"}
	if !equalSlice(f.gotArgs, wantArgs) {
		t.Errorf("dispatched args = %v, want %v", f.gotArgs, wantArgs)
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
