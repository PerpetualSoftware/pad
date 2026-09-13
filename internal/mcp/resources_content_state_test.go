package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3033 — the MCP item resource.
//
// This door is the subtlest of the family: the marker is FETCHED and then
// dropped. `item show --format json` has carried content_state since BUG-3000,
// and readItem re-renders that JSON through formatItemAsMarkdown, which composes
// its own document — so the field arrives and is discarded on the way out. The
// CLI's stderr warning does not rescue it either: ExecResourceFetcher reads
// stdout and never surfaces stderr from a command that SUCCEEDED.
//
// Driven through readItem rather than by calling the formatter directly (CONVE-19):
// a direct-call test would vouch for the formatter while leaving the resource
// free to stop calling it. The absence leg runs first for the reason it does
// everywhere in this unit.
func TestReadItemCarriesTheStaleBodyMarkerBothWays(t *testing.T) {
	read := func(t *testing.T, itemJSON string) string {
		t.Helper()
		r := &resources{fetcher: &fakeFetcher{stdout: itemJSON}}
		req := mcp.ReadResourceRequest{}
		req.Params.URI = "pad://workspace/docapp/items/TASK-5"
		contents, err := r.readItem(context.Background(), req)
		if err != nil {
			t.Fatalf("readItem: %v", err)
		}
		tc, ok := contents[0].(mcp.TextResourceContents)
		if !ok {
			t.Fatalf("expected TextResourceContents, got %T", contents[0])
		}
		return tc.Text
	}

	const current = `{
		"ref": "TASK-5",
		"title": "Fix OAuth",
		"fields": "{\"priority\":\"high\"}",
		"content": "Detailed plan goes here."
	}`
	const stale = `{
		"ref": "TASK-5",
		"title": "Fix OAuth",
		"fields": "{\"priority\":\"high\"}",
		"content": "Detailed plan goes here.",
		"content_state": "applied_pending_flush"
	}`

	// ABSENCE FIRST. A resource that always warned would satisfy the leg below
	// and be useless — every body would read as suspect.
	if got := read(t, current); strings.Contains(got, "content_state") || strings.Contains(got, "Stale body") {
		t.Fatalf("a current item's resource warns about staleness:\n%s", got)
	}

	got := read(t, stale)
	if !strings.Contains(got, "Stale body") {
		t.Errorf("a stale item's resource carries no warning:\n%s", got)
	}
	// The literal state token, so the marker is greppable by the same name the
	// JSON field uses rather than only readable as prose.
	if !strings.Contains(got, models.ContentOutcomeAppliedPendingFlush) {
		t.Errorf("the warning omits the %q token:\n%s", models.ContentOutcomeAppliedPendingFlush, got)
	}
	// It must precede the body it qualifies — an agent reading top-down has to
	// meet the caveat before the text it is about, not after.
	markerAt := strings.Index(got, "Stale body")
	bodyAt := strings.Index(got, "Detailed plan goes here.")
	if bodyAt < 0 {
		t.Fatal("the body is missing from the marked resource; the marker is a qualifier, not a substitute")
	}
	if markerAt > bodyAt {
		t.Errorf("the marker follows the body it qualifies:\n%s", got)
	}
}

// TestFormatItemAsMarkdownIgnoresAnUnknownContentState pins the other direction
// of the same key: only the ONE value this vocabulary defines produces a
// warning.
//
// Not a hypothetical. content_state shares its value vocabulary with BUG-2995's
// write-side content_outcome deliberately, so that vocabulary is expected to
// grow — and a formatter that warned on any non-empty string would turn a future
// value it has never heard of into this specific claim about a live editor.
func TestFormatItemAsMarkdownIgnoresAnUnknownContentState(t *testing.T) {
	got, err := formatItemAsMarkdown(`{
		"ref": "TASK-5",
		"title": "Fix OAuth",
		"content": "Body.",
		"content_state": "some_future_value"
	}`)
	if err != nil {
		t.Fatalf("formatItemAsMarkdown: %v", err)
	}
	if strings.Contains(got, "Stale body") {
		t.Errorf("an unrecognised content_state was rendered as this unit's specific claim:\n%s", got)
	}
	if !strings.Contains(got, "Body.") {
		t.Errorf("the body was lost:\n%s", got)
	}
}
