package store

import (
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestCollabSnapshotPreservesVersionDiff is a regression guard for
// TASK-1267 (PLAN-1248 version-diff coexistence verification).
//
// The 5s collab-snapshot flush (TASK-1260) PATCHes items.content
// through the same `Store.UpdateItem` path as any other content
// write. The version-diff feature reads `items.content` snapshots
// over time — those snapshots are the canonical record consumed by
// the Versions panel and `pad history` views. This test exercises
// the full happy path:
//
//  1. Create item with content "v1" sourced as "cli" (so the
//     first UpdateItem with Source="web" isn't throttled by the
//     per-(actor, source) gate in shouldCreateItemVersion).
//  2. Update content to "v2" with `Source: "web"` after a brief
//     sleep (the version table orders by RFC3339-second created_at;
//     back-to-back writes inside one second tie and the
//     newest→oldest walk becomes order-dependent on row insertion).
//  3. Update content to "v3" with `VersionSource: "collab-snapshot"`
//     after another brief sleep — simulating a 5s flush. We use
//     VersionSource (matching what the HTTP handler does in
//     production for `?source=collab-snapshot`) so the version
//     row's Source attribution is "collab-snapshot" while
//     items.source is left alone.
//  4. List versions and reconstruct each via the diff/patch chain.
//     Locate the row written for each PATCH by `Source` rather
//     than positional index, since the create-time row is also
//     in the list.
//  5. Assert the v2 row reconstructs to "v1-content" (snapshot
//     before that write), the v3 row reconstructs to "v2-content".
//  6. Verify at least one of the rows was actually stored as a
//     reverse-patch (`IsDiff: true`) — the test contents are long
//     and mostly-identical specifically so `diff.IsDiffSmaller`
//     keeps them as patches. Without that, the test would never
//     exercise the diff/reconstruction path that motivates this
//     coexistence check.
//
// If a future change makes collab-snapshot writes bypass version
// snapshotting (e.g. by hitting a separate write path that doesn't
// run UpdateItem), this test fails. That's the regression we want
// to catch — markdown remains canonical for non-edit consumers
// (search, exports, share-page, MCP, version diff) is a load-
// bearing invariant of PLAN-1248's architecture.
func TestCollabSnapshotPreservesVersionDiff(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Collab Versions Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	// Long, mostly-identical markdown blocks. The 80-char filler
	// line dwarfs any patch describing the single-line change, so
	// `diff.IsDiffSmaller` will keep these as reverse-patches and
	// the test actually exercises the patch-reconstruction path.
	// Per Codex review round 1 [P2].
	filler := strings.Repeat("Lorem ipsum dolor sit amet, consectetur adipiscing elit. ", 20)
	v1 := filler + "\n\nSection: original heading\nContent line one.\n"
	v2 := filler + "\n\nSection: original heading\nContent line one updated.\n"
	v3 := filler + "\n\nSection: revised heading\nContent line one updated.\n"

	// Step 1: create with Source="cli". CreateItem inserts an
	// initial version row with this source; the next UpdateItem
	// with Source="web" then differs from the most-recent version's
	// source, bypassing the per-(actor, source) throttle in
	// shouldCreateItemVersion. Without this differentiation the
	// "v1→v2" row would be silently throttled away. Per Codex
	// review round 1 [P2].
	item, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:   "Versioned Item",
		Content: v1,
		Fields:  `{"status":"open"}`,
		Source:  "cli",
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	// One-second sleep between writes so each version row gets a
	// distinct `created_at`. RFC3339-second resolution + back-to-
	// back updates would otherwise tie and the newest→oldest walk
	// would depend on row-insertion order rather than time. Per
	// Codex review round 1 [P2].
	time.Sleep(1100 * time.Millisecond)

	// Step 2: web write to v2.
	_, err = s.UpdateItem(item.ID, models.ItemUpdate{
		Content:        &v2,
		LastModifiedBy: "user",
		Source:         "web",
	})
	if err != nil {
		t.Fatalf("UpdateItem v2 (web): %v", err)
	}

	time.Sleep(1100 * time.Millisecond)

	// Step 3: collab-snapshot write to v3. We use VersionSource
	// (matching what the HTTP handler does in production for
	// `?source=collab-snapshot` PATCHes — see handlers_items.go)
	// so the version row's Source attribution is "collab-snapshot"
	// while items.source stays whatever it was. The source change
	// also bypasses the per-(actor, source) throttle.
	_, err = s.UpdateItem(item.ID, models.ItemUpdate{
		Content:        &v3,
		LastModifiedBy: "user",
		VersionSource:  "collab-snapshot",
	})
	if err != nil {
		t.Fatalf("UpdateItem v3 (collab-snapshot): %v", err)
	}

	// Step 4: read back the resolved version chain.
	resolved, err := s.ListItemVersionsResolved(item.ID, v3)
	if err != nil {
		t.Fatalf("ListItemVersionsResolved: %v", err)
	}
	if len(resolved) < 3 {
		t.Fatalf("expected at least 3 version rows (initial + 2 updates), got %d", len(resolved))
	}

	// Locate the rows by Source so the assertions don't depend on
	// the relative ordering of the create-time row vs. the update
	// rows. Per Codex review round 1 [P2].
	rowBySource := map[string]models.Version{}
	for _, v := range resolved {
		// Take the most recent row per source — there's only one
		// per source in this test, but be defensive.
		if _, seen := rowBySource[v.Source]; !seen {
			rowBySource[v.Source] = v
		}
	}

	// Step 5: each version row records what items.content was
	// IMMEDIATELY BEFORE the update that created the row (see
	// items.go line ~885: `versionContent := existing.Content`).
	// After ListItemVersionsResolved unwinds the diff chain, each
	// row's `Content` is that pre-update string. Concretely:
	//   - cli row content = v1  (initial state from CreateItem)
	//   - web row content = v1  (state before the v2 UpdateItem)
	//   - collab-snapshot row content = v2  (state before the v3 UpdateItem)
	cliRow, ok := rowBySource["cli"]
	if !ok {
		t.Fatalf("no version row with Source=cli; got rows %+v", rowBySource)
	}
	if cliRow.Content != v1 {
		t.Errorf("cli row Content: want v1 (len %d), got len %d", len(v1), len(cliRow.Content))
	}

	webRow, ok := rowBySource["web"]
	if !ok {
		t.Fatalf("no version row with Source=web; the v1→v2 web update was throttled away — the test no longer covers what it claims to. Got rows %+v", rowBySource)
	}
	if webRow.Content != v1 {
		t.Errorf("web row resolved Content: want v1, got %d-byte string", len(webRow.Content))
	}

	collabRow, ok := rowBySource["collab-snapshot"]
	if !ok {
		t.Fatalf("no version row with Source=collab-snapshot; the v3 collab-snapshot update did NOT create a version row. This is the actual regression TASK-1267 watches for — collab-snapshot writes must remain in the version-diff path. Got rows %+v", rowBySource)
	}
	if collabRow.Content != v2 {
		t.Errorf("collab-snapshot row resolved Content: want v2, got %d-byte string", len(collabRow.Content))
	}

	// Step 6: at least one of the update rows should have been
	// stored as a reverse-patch on disk (not just full content),
	// confirming the diff/reconstruction path is actually
	// exercised. We re-fetch the raw rows because
	// ListItemVersionsResolved sets IsDiff=false on every row
	// after applying the patch.
	raw, err := s.ListItemVersions(item.ID)
	if err != nil {
		t.Fatalf("ListItemVersions: %v", err)
	}
	sawDiff := false
	for _, v := range raw {
		if v.IsDiff {
			sawDiff = true
			break
		}
	}
	if !sawDiff {
		t.Errorf("expected at least one version row stored as a reverse-patch (IsDiff=true); test contents are too small or too dissimilar to exercise the diff path. Bump the filler size.")
	}
}
