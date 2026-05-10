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

// TestCollabSnapshotQueryOverridesBodyVersionSource is the TASK-1309
// round-6 [P2] regression: a `?source=collab-snapshot` query MUST
// override any `version_source` field in the PATCH body. Otherwise
// a buggy/malicious client could send the query (which triggers
// the applier-bypass path and reaches Store.UpdateItem directly)
// with `version_source: "cli"` in the body, sneaking a stale
// browser snapshot through the op-log GC watermark policy
// (TASK-1309 round 5 only advances the watermark when
// VersionSource != "collab-snapshot").
//
// We assert the version row's Source column is "collab-snapshot"
// even when the body claims something else.
func TestCollabSnapshotQueryOverridesBodyVersionSource(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":   "Override test",
		"content": "v1",
		"source":  "cli",
		"fields":  `{"status":"open"}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var created models.Item
	parseJSON(t, rr, &created)

	time.Sleep(1100 * time.Millisecond)

	// PATCH with the dangerous combo: query says collab-snapshot,
	// body claims version_source=cli (which would otherwise sneak
	// past the gate and advance the watermark).
	rr = doRequest(srv, "PATCH",
		"/api/v1/workspaces/"+slug+"/items/"+created.Slug+"?source=collab-snapshot",
		map[string]interface{}{
			"content":        "v2",
			"version_source": "cli", // attacker / buggy override
		},
	)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH: %d %s", rr.Code, rr.Body.String())
	}

	// The version row written for this PATCH must record source as
	// "collab-snapshot" — the query-param signal wins.
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+created.Slug+"/versions", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list versions: %d", rr.Code)
	}
	var versions []models.Version
	parseJSON(t, rr, &versions)

	var foundCollabRow bool
	for _, v := range versions {
		if v.Source == "collab-snapshot" {
			foundCollabRow = true
			break
		}
	}
	if !foundCollabRow {
		t.Errorf(
			"query-param `?source=collab-snapshot` must override body "+
				"`version_source`; got version sources: %v. The body "+
				"claimed cli but the trusted query param said "+
				"collab-snapshot, which is the GC-safe attribution.",
			sourcesOf(versions),
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

// TestCollabSnapshotRejectsCursorBelowMin is the TASK-1319 round 10
// [P1] regression: a collab-snapshot PATCH whose op_log_cursor is
// below the current MIN(item_yjs_updates.id) MUST be rejected at
// the handler with 409. Such a cursor proves the flushing tab's
// Y.Doc was built on op-log rows that no longer exist (PruneAndApply,
// schema rebuild, dormant GC, or the post-force_refresh race window
// where a server-side rejection is the only authoritative gate).
// Accepting it would overwrite items.content with markdown derived
// from a known-bad source.
func TestCollabSnapshotRejectsCursorBelowMin(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":   "Stale cursor test",
		"content": "v1",
		"source":  "cli",
		"fields":  `{"status":"open"}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var created models.Item
	parseJSON(t, rr, &created)

	// Seed three op-log rows then drop ids 1+2 to simulate a
	// post-PruneAndApply state where MIN(id) = 3.
	for i := 1; i <= 3; i++ {
		if _, err := srv.store.AppendYjsUpdate(created.ID, []byte{0x00, byte(i)}, "1"); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	// Truncate via the public API: prune everything older than now,
	// then re-append two rows so MIN advances past the originals.
	if _, err := srv.store.PruneYjsUpdatesBefore(created.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	for i := 4; i <= 5; i++ {
		if _, err := srv.store.AppendYjsUpdate(created.ID, []byte{0x00, byte(i)}, "1"); err != nil {
			t.Fatalf("re-seed %d: %v", i, err)
		}
	}
	// MIN is now id=4. Cursor=2 is below MIN → handler must 409.

	cursor := int64(2)
	rr = doRequest(srv, "PATCH",
		"/api/v1/workspaces/"+slug+"/items/"+created.Slug+"?source=collab-snapshot",
		map[string]interface{}{
			"content":       "stale-flush-content",
			"op_log_cursor": cursor,
		},
	)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for stale cursor; got %d %s", rr.Code, rr.Body.String())
	}

	// items.content must NOT have been written. Re-fetch and assert.
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+created.Slug, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET: %d", rr.Code)
	}
	var got models.Item
	parseJSON(t, rr, &got)
	if got.Content == "stale-flush-content" {
		t.Errorf("items.content was overwritten with stale-flush-content; the handler accepted a sub-MIN cursor PATCH")
	}
}

// TestCollabSnapshotAcceptsCursorAtOrAboveMin is the negative test:
// a cursor at or above MIN passes the gate.
func TestCollabSnapshotAcceptsCursorAtOrAboveMin(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":   "Healthy cursor test",
		"content": "v1",
		"source":  "cli",
		"fields":  `{"status":"open"}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var created models.Item
	parseJSON(t, rr, &created)

	id1, err := srv.store.AppendYjsUpdate(created.ID, []byte{0x00, 0x01}, "1")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	cursor := id1 // cursor == MIN — caught up.
	rr = doRequest(srv, "PATCH",
		"/api/v1/workspaces/"+slug+"/items/"+created.Slug+"?source=collab-snapshot",
		map[string]interface{}{
			"content":       "fresh-content",
			"op_log_cursor": cursor,
		},
	)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for cursor>=MIN; got %d %s", rr.Code, rr.Body.String())
	}
}
