package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/gorilla/websocket"
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

// TestCollabSnapshotRejectsCursorOnEmptyOpLog mirrors the WS
// force_refresh predicate at the HTTP layer (Codex round 11 [P1]
// of TASK-1319). A non-zero cursor against an item whose op-log
// is entirely empty is, by construction, stale: the rows the
// client claims to have applied no longer exist. Reject 409.
func TestCollabSnapshotRejectsCursorOnEmptyOpLog(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":   "Empty oplog test",
		"content": "v1",
		"source":  "cli",
		"fields":  `{"status":"open"}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var created models.Item
	parseJSON(t, rr, &created)

	// No op-log seed — the table is empty for this item. Cursor=42
	// is a non-zero claim that can't be reconciled.
	cursor := int64(42)
	rr = doRequest(srv, "PATCH",
		"/api/v1/workspaces/"+slug+"/items/"+created.Slug+"?source=collab-snapshot",
		map[string]interface{}{
			"content":       "stale-empty-oplog-content",
			"op_log_cursor": cursor,
		},
	)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for non-zero cursor on empty op-log; got %d %s", rr.Code, rr.Body.String())
	}
}

// TestCollabSnapshotRejectsCursorZeroOnNonEmptyOpLog (round 12
// [P1] of TASK-1319): a stateful tab whose previous session
// disconnected before receiving any op_log_cursor frame ends up
// with sessionStorage cursor=0 but a non-empty Y.Doc derived from
// prior replay binaries. On flush, it carries cursor=0 alongside
// Y.Doc-derived markdown. Without the gate, items.content would
// be overwritten by the stale view.
func TestCollabSnapshotRejectsCursorZeroOnNonEmptyOpLog(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":   "Cursor zero stale test",
		"content": "v1",
		"source":  "cli",
		"fields":  `{"status":"open"}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var created models.Item
	parseJSON(t, rr, &created)

	// Seed an op-log row so MIN is non-zero.
	if _, err := srv.store.AppendYjsUpdate(created.ID, []byte{0x00, 0x01}, "1"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cursor := int64(0)
	rr = doRequest(srv, "PATCH",
		"/api/v1/workspaces/"+slug+"/items/"+created.Slug+"?source=collab-snapshot",
		map[string]interface{}{
			"content":       "stale-zero-cursor-content",
			"op_log_cursor": cursor,
		},
	)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for cursor=0 on non-empty op-log; got %d %s", rr.Code, rr.Body.String())
	}
}

// TestCollabSnapshotRejectedBelowRestoreBoundary is the BUG-2264 Invariant B
// guard at the HTTP boundary. Once a version restore reconciles the live Y.Doc
// and records a restore boundary (MAX(op-log.id) at restore time), a
// collab-snapshot flush whose op_log_cursor is BELOW the boundary captured a
// PRE-restore Y.Doc — its markdown is stale and would re-clobber the restored
// content — so the handler rejects it (409). A flush at or above the boundary
// is a post-restore snapshot and is accepted. Requires the RoomManager (the
// boundary lives there and the check runs only on the s.collab != nil path).
func TestCollabSnapshotRejectedBelowRestoreBoundary(t *testing.T) {
	srv := testServerWithCollab(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":   "Restore boundary gate",
		"content": "v1",
		"source":  "cli",
		"fields":  `{"status":"open"}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var created models.Item
	parseJSON(t, rr, &created)

	// Seed op-log rows so the MIN-based staleness check passes (non-empty log,
	// low MIN) — isolating the restore-boundary gate from the MIN gate.
	var ids []int64
	for i := 0; i < 5; i++ {
		id, err := srv.store.AppendYjsUpdate(created.ID, []byte{0x00, byte(i + 1)}, "1")
		if err != nil {
			t.Fatalf("AppendYjsUpdate %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	// Simulate the collab-live restore having recorded the boundary at a mid-log
	// id (the restore path sets it to MAX(op-log) after the applier ack).
	srv.collab.SetRestoreBoundary(created.ID, ids[2])

	// A flush below the boundary (but >= MIN) is a stale pre-restore snapshot.
	rr = doRequest(srv, "PATCH",
		"/api/v1/workspaces/"+slug+"/items/"+created.Slug+"?source=collab-snapshot",
		map[string]interface{}{"content": "stale pre-restore", "op_log_cursor": ids[1]},
	)
	if rr.Code != http.StatusConflict {
		t.Fatalf("pre-restore flush (cursor %d < boundary %d): expected 409, got %d %s", ids[1], ids[2], rr.Code, rr.Body.String())
	}
	// items.content must NOT have been clobbered by the stale snapshot.
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+created.Slug, nil)
	var afterReject models.Item
	parseJSON(t, rr, &afterReject)
	if afterReject.Content == "stale pre-restore" {
		t.Fatalf("stale pre-restore snapshot clobbered items.content (BUG-2264 Invariant B)")
	}

	// A flush at the boundary is a post-restore snapshot → accepted.
	rr = doRequest(srv, "PATCH",
		"/api/v1/workspaces/"+slug+"/items/"+created.Slug+"?source=collab-snapshot",
		map[string]interface{}{"content": "post-restore", "op_log_cursor": ids[2]},
	)
	if rr.Code != http.StatusOK {
		t.Fatalf("post-restore flush (cursor %d >= boundary %d): expected 200, got %d %s", ids[2], ids[2], rr.Code, rr.Body.String())
	}
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+created.Slug, nil)
	var afterAccept models.Item
	parseJSON(t, rr, &afterAccept)
	if afterAccept.Content != "post-restore" {
		t.Fatalf("post-restore flush should have written items.content, got %q", afterAccept.Content)
	}

	// Fail-closed on a MISSING cursor once a boundary exists: a snapshot with no
	// op_log_cursor can't prove it captured post-restore state, so it must be
	// rejected too — otherwise a legacy/in-flight no-cursor flush would sail past
	// the fence and clobber the restore.
	rr = doRequest(srv, "PATCH",
		"/api/v1/workspaces/"+slug+"/items/"+created.Slug+"?source=collab-snapshot",
		map[string]interface{}{"content": "no-cursor stale"},
	)
	if rr.Code != http.StatusConflict {
		t.Fatalf("no-cursor flush with a boundary set: expected 409 (fail closed), got %d %s", rr.Code, rr.Body.String())
	}
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+created.Slug, nil)
	var afterNoCursor models.Item
	parseJSON(t, rr, &afterNoCursor)
	if afterNoCursor.Content == "no-cursor stale" {
		t.Fatalf("no-cursor flush clobbered items.content past the restore boundary (BUG-2264 Invariant B)")
	}
}

// TestCollabSnapshotRejectedBelowDurableRestoreBoundaryAfterRestart is the
// BUG-2264 residual-#1 guard for the COLLAB-SNAPSHOT flush vector: after a server
// restart the in-memory RoomManager.RestoreBoundary is gone, so a surviving
// pre-restore tab's stale flush must be fenced off the DURABLE
// items.restore_boundary_op_id column instead. On an empty (post-restore, pruned)
// op-log the MIN gate accepts cursor 0 (the fresh-client path), so the durable
// boundary is the only thing standing between a stale cursor-0 flush and the
// restored content.
func TestCollabSnapshotRejectedBelowDurableRestoreBoundaryAfterRestart(t *testing.T) {
	srv := testServerWithCollab(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":   "Durable restore boundary gate",
		"content": "v1",
		"source":  "cli",
		"fields":  `{"status":"open"}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var created models.Item
	parseJSON(t, rr, &created)

	// Baseline: with NO restore boundary (durable or in-memory) and an empty
	// op-log, a cursor-0 flush is the accepted fresh-client path.
	rr = doRequest(srv, "PATCH",
		"/api/v1/workspaces/"+slug+"/items/"+created.Slug+"?source=collab-snapshot",
		map[string]interface{}{"content": "fresh cursor-0", "op_log_cursor": int64(0)},
	)
	if rr.Code != http.StatusOK {
		t.Fatalf("baseline cursor-0 flush (no boundary): expected 200, got %d %s", rr.Code, rr.Body.String())
	}

	// Simulate a restore that happened in a PRIOR process: stamp the DURABLE
	// boundary but leave the in-memory RoomManager.RestoreBoundary ABSENT (a fresh
	// process after a restart).
	if err := srv.store.SetItemRestoreBoundaryOpID(created.ID, 6); err != nil {
		t.Fatalf("SetItemRestoreBoundaryOpID: %v", err)
	}
	if _, ok := srv.collab.RestoreBoundary(created.ID); ok {
		t.Fatal("in-memory restore boundary must be absent for the restart scenario")
	}

	// The same cursor-0 flush is now FENCED by the durable boundary (cursor 0 < 6)
	// even though the in-memory boundary is gone — the restart-durability fix.
	rr = doRequest(srv, "PATCH",
		"/api/v1/workspaces/"+slug+"/items/"+created.Slug+"?source=collab-snapshot",
		map[string]interface{}{"content": "stale pre-restore", "op_log_cursor": int64(0)},
	)
	if rr.Code != http.StatusConflict {
		t.Fatalf("stale cursor-0 flush vs durable boundary: expected 409, got %d %s", rr.Code, rr.Body.String())
	}
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+created.Slug, nil)
	var after models.Item
	parseJSON(t, rr, &after)
	if after.Content == "stale pre-restore" {
		t.Fatalf("stale flush clobbered items.content past the DURABLE restore boundary")
	}
}

// restoreCollabFixture bundles the ws + collection + item(v1, edited to v2 so a
// version resolving to v1 exists) + live test server the prune+reseed restore
// tests share.
type restoreCollabFixture struct {
	srv         *Server
	ts          *httptest.Server
	wsSlug      string
	itemID      string
	itemSlug    string
	v1VersionID string
}

func newRestoreCollabFixture(t *testing.T, name string) *restoreCollabFixture {
	t.Helper()
	srv := testServerWithCollab(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: name})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	col, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	item, err := srv.store.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "Doc", Content: "v1", Fields: `{}`})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	v2 := "v2"
	if _, err := srv.store.UpdateItem(item.ID, models.ItemUpdate{Content: &v2, LastModifiedBy: "user", Source: "web"}); err != nil {
		t.Fatalf("seed v2: %v", err)
	}
	versions, err := srv.store.ListItemVersionsResolved(item.ID, "v2")
	if err != nil {
		t.Fatalf("ListItemVersionsResolved: %v", err)
	}
	var v1VersionID string
	for _, v := range versions {
		if v.Content == "v1" {
			v1VersionID = v.ID
			break
		}
	}
	if v1VersionID == "" {
		t.Fatalf("no version resolving to v1")
	}
	return &restoreCollabFixture{srv: srv, ts: ts, wsSlug: ws.Slug, itemID: item.ID, itemSlug: item.Slug, v1VersionID: v1VersionID}
}

// dialReady dials the collab WS and blocks until the initial control frame (the
// post-replay op_log_cursor, sent AFTER addConn) proves the conn is registered.
func (f *restoreCollabFixture) dialReady(t *testing.T) *websocket.Conn {
	t.Helper()
	conn, resp, err := dialCollab(t, f.ts.URL, f.itemID, nil, "")
	if err != nil {
		status := ""
		if resp != nil {
			status = resp.Status
		}
		t.Fatalf("dialCollab: %v (%s)", err, status)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, rerr := conn.ReadMessage(); rerr != nil {
		t.Fatalf("waiting for initial collab frame: %v", rerr)
	}
	_ = conn.SetReadDeadline(time.Time{})
	return conn
}

func (f *restoreCollabFixture) restore(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(f.srv, "POST",
		"/api/v1/workspaces/"+f.wsSlug+"/items/"+f.itemSlug+"/versions/"+f.v1VersionID+"/restore", nil)
}

func assertItemContent(t *testing.T, srv *Server, itemID, want string) {
	t.Helper()
	got, err := srv.store.GetItem(itemID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if got.Content != want {
		t.Fatalf("items.content: want %q, got %q", want, got.Content)
	}
}

func assertOpLogEmpty(t *testing.T, srv *Server, itemID string) {
	t.Helper()
	if _, ok, err := srv.store.MaxOpLogID(itemID); err != nil || ok {
		t.Fatalf("op-log must be pruned/empty (ok=%v, err=%v)", ok, err)
	}
}

// assertGotForceRefresh drains conn until it either receives a force_refresh
// control frame (pass) or the conn closes without one (fail).
func assertGotForceRefresh(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	saw := false
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			break // conn closed
		}
		if mt == websocket.TextMessage {
			var ctl struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(data, &ctl) == nil && ctl.Type == "force_refresh" {
				saw = true
			}
		}
	}
	if !saw {
		t.Fatal("peer did not receive a force_refresh frame before the conn closed")
	}
}

// TestRestoreLiveRoomPrunesAndReseeds is the BUG-2264 prune+reseed happy path: a
// restore under a live collab room writes items.content=restored, prunes the
// per-item op-log, sets the stale-flush boundary, and force-refreshes every peer
// so they rebuild from the restored content (all peers converge; unflushed edits
// are discarded — restore semantics).
func TestRestoreLiveRoomPrunesAndReseeds(t *testing.T) {
	f := newRestoreCollabFixture(t, "LiveRestore")
	conn := f.dialReady(t)
	// A peer op grows the op-log so we can prove the prune wipes it + boundary.
	if _, err := f.srv.store.AppendYjsUpdate(f.itemID, []byte{0x00, 0x01}, "1"); err != nil {
		t.Fatalf("seed op: %v", err)
	}
	maxBefore, _, _ := f.srv.store.MaxOpLogID(f.itemID)

	rr := f.restore(t)
	if rr.Code != http.StatusOK {
		t.Fatalf("restore: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	assertItemContent(t, f.srv, f.itemID, "v1")
	assertOpLogEmpty(t, f.srv, f.itemID)
	if b, ok := f.srv.collab.RestoreBoundary(f.itemID); !ok || b <= maxBefore {
		t.Fatalf("restore boundary must be > pre-prune MAX %d; got %d ok=%v", maxBefore, b, ok)
	}
	// The op-log-id boundary was also stamped DURABLY (survives restart) with the
	// same pre-prune MAX+1, atomically in the restore tx.
	if db, ok, derr := f.srv.store.ItemRestoreBoundaryOpID(f.itemID); derr != nil || !ok || db <= maxBefore {
		t.Fatalf("durable restore_boundary_op_id must be > pre-prune MAX %d; got %d ok=%v err=%v", maxBefore, db, ok, derr)
	}
	assertGotForceRefresh(t, conn)
}

// TestRestorePrunesStalePeerOpAndFencesStaleCursor is the BUG-2264 P1b guard:
// after the prune the op-log is empty, and an in-flight stale collab-snapshot —
// whether it carries the pruned peer op's cursor OR cursor 0 — is fenced by the
// pre-prune-MAX+1 boundary and cannot clobber the restored items.content.
func TestRestorePrunesStalePeerOpAndFencesStaleCursor(t *testing.T) {
	f := newRestoreCollabFixture(t, "R1PeerOp")
	conn := f.dialReady(t)
	// A concurrent peer op grows MAX(op-log) to nID (the false-positive the old
	// "MAX grew" heuristic tripped on; prune+reseed doesn't care — it prunes it).
	nID, err := f.srv.store.AppendYjsUpdate(f.itemID, []byte{0x00, 0x01}, "1")
	if err != nil {
		t.Fatalf("seed peer op: %v", err)
	}

	rr := f.restore(t)
	if rr.Code != http.StatusOK {
		t.Fatalf("restore: %d %s", rr.Code, rr.Body.String())
	}
	assertItemContent(t, f.srv, f.itemID, "v1")
	assertOpLogEmpty(t, f.srv, f.itemID)
	assertGotForceRefresh(t, conn)

	// Stale snapshot at the pruned peer op's cursor → rejected, no clobber.
	rr = doRequest(f.srv, "PATCH",
		"/api/v1/workspaces/"+f.wsSlug+"/items/"+f.itemSlug+"?source=collab-snapshot",
		map[string]interface{}{"content": "stale pre-restore", "op_log_cursor": nID})
	if rr.Code != http.StatusConflict {
		t.Fatalf("stale-cursor flush (cursor=%d) must be 409, got %d: %s", nID, rr.Code, rr.Body.String())
	}
	assertItemContent(t, f.srv, f.itemID, "v1")

	// cursor=0 must ALSO be fenced (the empty-op-log MIN gate alone accepts 0).
	rr = doRequest(f.srv, "PATCH",
		"/api/v1/workspaces/"+f.wsSlug+"/items/"+f.itemSlug+"?source=collab-snapshot",
		map[string]interface{}{"content": "stale cursor zero", "op_log_cursor": int64(0)})
	if rr.Code != http.StatusConflict {
		t.Fatalf("stale cursor=0 flush must be 409, got %d: %s", rr.Code, rr.Body.String())
	}
	assertItemContent(t, f.srv, f.itemID, "v1")
}

// TestRestoreNoRoomStillPrunesAndWrites: with collab configured but no live room,
// the restore still writes items.content=restored and prunes any stale op-log, so
// a later cold connect lazy-seeds the restored content.
func TestRestoreNoRoomStillPrunesAndWrites(t *testing.T) {
	f := newRestoreCollabFixture(t, "NoRoom") // no dialReady → no live room
	if _, err := f.srv.store.AppendYjsUpdate(f.itemID, []byte{0x00, 0x01}, "1"); err != nil {
		t.Fatalf("seed stale op: %v", err)
	}
	rr := f.restore(t)
	if rr.Code != http.StatusOK {
		t.Fatalf("restore: %d %s", rr.Code, rr.Body.String())
	}
	assertItemContent(t, f.srv, f.itemID, "v1")
	assertOpLogEmpty(t, f.srv, f.itemID)
}

// dialContentSeq dials the collab WS announcing ?content_seq=<seq> (the
// items.content generation the client's Y.Doc was seeded from).
func (f *restoreCollabFixture) dialContentSeq(t *testing.T, seq int64) *websocket.Conn {
	t.Helper()
	target := f.itemID + "?content_seq=" + strconv.FormatInt(seq, 10)
	conn, resp, err := dialCollab(t, f.ts.URL, target, nil, "")
	if err != nil {
		status := ""
		if resp != nil {
			status = resp.Status
		}
		t.Fatalf("dialCollab(content_seq=%d): %v (%s)", seq, err, status)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// assertNoForceRefresh reads the first frame and fails if it is a force_refresh
// (a normal Join's first frame is the op_log_cursor control, not force_refresh).
func assertNoForceRefresh(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	mt, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("expected a normal initial frame, got read error: %v", err)
	}
	if mt == websocket.TextMessage {
		var ctl struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(data, &ctl) == nil && ctl.Type == "force_refresh" {
			t.Fatal("current-generation client was wrongly force_refreshed")
		}
	}
}

// TestRestoreForceRefreshesStaleSeedOnJoin is the BUG-2264 stale-SEED guard
// (additional-P1 + Codex-xhigh): after a restore, a client that connects with a
// Y.Doc seeded from PRE-restore items.content — a cursor-0 peer whose
// force_refresh frame was lost, or a browser that GET items.content before the
// restore and joins after the prune — announces a ?content_seq below the
// restored generation and must be force_refreshed BEFORE its on-open state push
// can resurrect the stale document. A client that seeded at (or after) the
// restored generation must join normally.
func TestRestoreForceRefreshesStaleSeedOnJoin(t *testing.T) {
	f := newRestoreCollabFixture(t, "StaleSeed")

	// The seq a pre-restore client would have seeded from (item is at v2).
	pre, err := f.srv.store.GetItem(f.itemID)
	if err != nil {
		t.Fatalf("GetItem (pre): %v", err)
	}
	preSeq := pre.Seq

	rr := f.restore(t)
	if rr.Code != http.StatusOK {
		t.Fatalf("restore: %d %s", rr.Code, rr.Body.String())
	}
	assertItemContent(t, f.srv, f.itemID, "v1")

	lrs, ok := f.srv.collab.LastRestoreSeq(f.itemID)
	if !ok || lrs <= preSeq {
		t.Fatalf("lastRestoreSeq must be > pre-restore seq %d; got %d ok=%v", preSeq, lrs, ok)
	}

	// The DURABLE boundary (items.last_restore_seq) was stamped atomically in the
	// restore tx with the SAME seq — this is what survives a server restart.
	durable, hasDurable, derr := f.srv.store.ItemLastRestoreSeq(f.itemID)
	if derr != nil || !hasDurable {
		t.Fatalf("durable last_restore_seq must be set after restore (ok=%v, err=%v)", hasDurable, derr)
	}
	if durable != lrs {
		t.Fatalf("durable last_restore_seq %d must equal the in-memory boundary %d", durable, lrs)
	}

	// A stale-seed client (content_seq predates the restore) is force_refreshed.
	staleConn := f.dialContentSeq(t, preSeq)
	assertGotForceRefresh(t, staleConn)

	// A current-seed client (content_seq == restored generation) joins normally.
	freshConn := f.dialContentSeq(t, lrs)
	assertNoForceRefresh(t, freshConn)
}
