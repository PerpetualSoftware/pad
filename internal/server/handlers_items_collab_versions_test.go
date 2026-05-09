package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestCollabSnapshotHTTPSourceStamping is a regression guard for
// TASK-1267 (PLAN-1248 version-diff coexistence verification),
// covering the HTTP-handler layer specifically.
//
// The web client's collab 5s-flush PATCH sends:
//
//	PATCH /workspaces/{ws}/items/{slug}?source=collab-snapshot
//	{ "content": "..." }
//
// The body has no `source` field — the indicator is the query
// parameter. The handler must stamp `input.VersionSource =
// "collab-snapshot"` before calling `Store.UpdateItem`, otherwise
// UpdateItem's default coerces an empty source to "web" on the
// version row and the per-(actor, source) version-snapshot throttle
// suppresses every collab-driven version row that follows the
// user's last manual web edit. We use VersionSource (not Source)
// so the auto-flush doesn't also mutate items.source.
//
// This test:
//
//  1. Creates an item via the standard PATCH/POST endpoints.
//  2. PATCHes with body content + ?source=collab-snapshot query.
//  3. Reads the version list and asserts at least one row exists
//     with `Source == "collab-snapshot"`.
//
// Sister test in store/items_collab_versions_test.go covers the
// downstream diff-reconstruction path; this one covers the
// handler-to-store boundary that round-2 Codex review identified
// as the missing link.
func TestCollabSnapshotHTTPSourceStamping(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Long, mostly-identical markdown so the version row exercises
	// reverse-patch storage on at least one transition.
	filler := strings.Repeat("Lorem ipsum dolor sit amet, consectetur adipiscing elit. ", 20)
	v1 := filler + "\n\nSection: original heading\nContent line one.\n"
	v2 := filler + "\n\nSection: revised heading\nContent line one.\n"

	// Step 1: create the item with content v1, sourced as "cli" so
	// the first PATCH's Source attribution differs and bypasses the
	// per-(actor, source) throttle.
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":   "Collab versioning HTTP test",
		"content": v1,
		"source":  "cli",
		"fields":  `{"status":"open"}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var created models.Item
	parseJSON(t, rr, &created)

	// One-second sleep so the next version row gets a distinct
	// RFC3339-second timestamp.
	time.Sleep(1100 * time.Millisecond)

	// Step 2: PATCH with the collab-snapshot query param. The body
	// intentionally has NO `source` field — that's how the real web
	// flush calls the endpoint (api/client.ts uses keepalive + a
	// bare {content} body, relying on the query param for routing).
	rr = doRequest(srv, "PATCH",
		"/api/v1/workspaces/"+slug+"/items/"+created.Slug+"?source=collab-snapshot",
		map[string]interface{}{"content": v2},
	)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH ?source=collab-snapshot: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Step 3: list versions through the same HTTP path the web UI
	// uses, and verify the collab-snapshot row was written with
	// the right Source attribution.
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+created.Slug+"/versions", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list versions: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var versions []models.Version
	if err := json.Unmarshal(rr.Body.Bytes(), &versions); err != nil {
		t.Fatalf("decode versions: %v", err)
	}

	var foundCollabRow bool
	for _, v := range versions {
		if v.Source == "collab-snapshot" {
			foundCollabRow = true
			break
		}
	}
	if !foundCollabRow {
		t.Errorf(
			"no version row with Source=\"collab-snapshot\" after a "+
				"`?source=collab-snapshot` PATCH. The handler is "+
				"supposed to stamp input.VersionSource from the query "+
				"param before calling Store.UpdateItem (TASK-1267); "+
				"without that stamp, every collab 5s-flush would land "+
				"as Source=\"web\" and the per-(actor, source) version "+
				"throttle would silently suppress every snapshot after "+
				"the user's last manual web edit. Got %d version rows "+
				"with sources: %v",
			len(versions), sourcesOf(versions),
		)
	}

	// Bonus assertion: items.source should still be "cli" — the
	// auto-flush MUST NOT mutate the persisted item source. Per
	// Codex round 3 of TASK-1267 [P2]: WorkspaceHasAgentActivity
	// counts items by `source IN ('cli', 'mcp')`, so a CLI-created
	// item that gets opened and auto-flushed in the editor would
	// silently flip out of the agent-activity tally if the handler
	// stamped Source instead of VersionSource.
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+created.Slug, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get item: expected 200, got %d", rr.Code)
	}
	var refetched models.Item
	parseJSON(t, rr, &refetched)
	if refetched.Source != "cli" {
		t.Errorf(
			"items.source after collab-snapshot PATCH: want %q "+
				"(unchanged from creation), got %q. The auto-flush "+
				"must NOT re-attribute items.source — that field "+
				"feeds WorkspaceHasAgentActivity which only counts "+
				"`source IN ('cli', 'mcp')`. Use VersionSource for "+
				"the version-row attribution.",
			"cli", refetched.Source,
		)
	}
}

func sourcesOf(versions []models.Version) []string {
	out := make([]string, 0, len(versions))
	for _, v := range versions {
		out = append(out, v.Source)
	}
	return out
}
