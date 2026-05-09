package server

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/collab"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestOpLogGC_PrunesDormantItems is the TASK-1309 server-level
// happy-path test: configure the GC for a short interval + minAge,
// seed two items — one fully dormant (all rows old), one mixed
// (recent activity) — start the loop, observe the dormant item's
// op-log fully pruned and the mixed item's preserved.
//
// Whole-log prune of dormant items only — prefix-pruning a mixed
// item would corrupt Yjs replay (causal references). Per Codex
// review of TASK-1309 [P1].
//
// Times here are tight (10ms ticks, 1min minAge) because the test
// is driving the real ticker. Test parameters are NOT representative
// of production defaults — those are 1h / 24h.
func TestOpLogGC_PrunesDormantItems(t *testing.T) {
	srv := testServerWithCollab(t)

	// Seed: workspace + collection + two items.
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "OpLogGC"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	col, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Tasks",
		Schema: `{"fields":[]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	// Both items have non-empty content so the GC watermark columns
	// (content_flushed_op_log_id and content_flushed_at) are populated
	// at creation. The id watermark starts at 0 because there are no
	// op-log rows yet — we'll backdate it past the seeded op-log
	// MAX(id) below to simulate a successful server-driven flush
	// covering those rows. The mixed item gets a recent op-log row
	// that breaks dormancy regardless of the watermark.
	dormant, err := srv.store.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title: "Dormant", Fields: `{}`, Content: "snapshot",
	})
	if err != nil {
		t.Fatalf("CreateItem dormant: %v", err)
	}
	mixed, err := srv.store.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title: "Mixed", Fields: `{}`, Content: "snapshot",
	})
	if err != nil {
		t.Fatalf("CreateItem mixed: %v", err)
	}

	// Hand-crafted INSERTs with explicit timestamps — production
	// AppendYjsUpdate stamps time.Now() so we can't backdate via it.
	old := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	fresh := time.Now().UTC().Format(time.RFC3339)
	exec := func(itemID string, payload byte, ts string) int64 {
		t.Helper()
		res, err := srv.store.DB().Exec(
			`INSERT INTO item_yjs_updates (item_id, update_data, schema_version, created_at) VALUES (?, ?, ?, ?)`,
			itemID, []byte{0, payload}, "1", ts,
		)
		if err != nil {
			t.Fatalf("seed insert: %v", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("LastInsertId: %v", err)
		}
		return id
	}
	// Dormant item: two old rows. The second LastInsertId is the
	// MAX op-log id we need the watermark to cover.
	exec(dormant.ID, 0x01, old)
	dormantMaxID := exec(dormant.ID, 0x02, old)
	// Mixed item: one old, one fresh — NOT eligible (whole-log
	// prune would corrupt the suffix's references to the prefix).
	exec(mixed.ID, 0x03, old)
	exec(mixed.ID, 0x04, fresh)

	// Bump the dormant item's content_flushed_op_log_id watermark
	// past the highest seeded op-log id, simulating a successful
	// flush that captured both old rows. CreateItem set it to 0,
	// which would otherwise leave the candidate query thinking
	// these rows aren't covered yet.
	if _, err := srv.store.DB().Exec(
		`UPDATE items SET content_flushed_op_log_id = ? WHERE id = ?`,
		dormantMaxID, dormant.ID,
	); err != nil {
		t.Fatalf("update dormant watermark: %v", err)
	}

	// minAge = 1 minute → all rows older than 60s ago are
	// candidates' MAX. Dormant item qualifies; mixed doesn't.
	srv.SetOpLogGCConfig(10*time.Millisecond, 1*time.Minute)
	srv.StartOpLogGC()
	t.Cleanup(srv.Stop)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		dormantRows, err := srv.store.LoadYjsUpdatesSince(dormant.ID, 0)
		if err != nil {
			t.Fatalf("LoadYjsUpdatesSince dormant: %v", err)
		}
		mixedRows, err := srv.store.LoadYjsUpdatesSince(mixed.ID, 0)
		if err != nil {
			t.Fatalf("LoadYjsUpdatesSince mixed: %v", err)
		}
		// Dormant item should be fully pruned (0 rows); mixed
		// should be preserved completely (2 rows).
		if len(dormantRows) == 0 && len(mixedRows) == 2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	dormantRows, _ := srv.store.LoadYjsUpdatesSince(dormant.ID, 0)
	mixedRows, _ := srv.store.LoadYjsUpdatesSince(mixed.ID, 0)
	t.Fatalf("after 2s: dormant=%d (want 0), mixed=%d (want 2)",
		len(dormantRows), len(mixedRows))
}

// TestOpLogGC_StartIsIdempotent confirms calling StartOpLogGC twice
// only spawns one goroutine. Without the guard, Stop() would block
// forever on the second goroutine that never received its stop
// signal.
func TestOpLogGC_StartIsIdempotent(t *testing.T) {
	srv := testServerWithCollab(t)

	// 1h interval = the loop never actually fires during the test;
	// we just want to exercise the running-flag check.
	srv.SetOpLogGCConfig(1*time.Hour, 24*time.Hour)
	srv.StartOpLogGC()
	srv.StartOpLogGC() // second call must be a no-op
	srv.StartOpLogGC() // third call too

	// Stop should drain in well under a second; if a second
	// goroutine was spawned without a matching stop channel,
	// Stop() would hang and the test would time out.
	done := make(chan struct{})
	go func() {
		srv.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return within 2s — likely a leaked GC goroutine")
	}
}

// TestOpLogGC_PreservesUnflushedItem is the TASK-1309 round-2 P1
// guard: an item with op-log rows that have NOT yet been captured
// by a items.content flush must NOT be pruned, even if the rows
// are dormant by age. The id watermark `items.content_flushed_op_log_id`
// must be >= MAX(op-log.id) before the GC proceeds.
//
// The motivating scenario: user types, op-log appends succeed, the
// browser/process dies before the next 5s collab-snapshot flush
// can land. The op-log is now the only durable record of those
// edits. After 24h the dormancy threshold ticks; without the
// watermark check, the GC would silently delete the unflushed
// edits.
func TestOpLogGC_PreservesUnflushedItem(t *testing.T) {
	srv := testServerWithCollab(t)

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "OpLogGC"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	col, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Tasks",
		Schema: `{"fields":[]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	// Start with empty content so both watermark columns
	// (content_flushed_op_log_id and content_flushed_at) are NULL.
	// CreateItem only sets them when content is non-empty. This
	// represents the worst case: the item has never had a successful
	// content flush.
	item, err := srv.store.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title: "Unflushed", Fields: `{}`, Content: "",
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	// Backdate two op-log rows. Both are well past minAge.
	old := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	for _, payload := range []byte{0x01, 0x02} {
		if _, err := srv.store.DB().Exec(
			`INSERT INTO item_yjs_updates (item_id, update_data, schema_version, created_at) VALUES (?, ?, ?, ?)`,
			item.ID, []byte{0, payload}, "1", old,
		); err != nil {
			t.Fatalf("seed row: %v", err)
		}
	}

	srv.SetOpLogGCConfig(10*time.Millisecond, 1*time.Minute)
	srv.StartOpLogGC()
	t.Cleanup(srv.Stop)

	// Give the sweeper plenty of ticks. content_flushed_op_log_id
	// is NULL on this item, so the candidate query MUST exclude it
	// and the rows should still be there.
	time.Sleep(200 * time.Millisecond)

	rows, err := srv.store.LoadYjsUpdatesSince(item.ID, 0)
	if err != nil {
		t.Fatalf("LoadYjsUpdatesSince: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("unflushed item must be preserved: want 2 op-log rows, got %d", len(rows))
	}
}

// TestOpLogGC_BackfillDoesNotCertifyUnflushed verifies the migration
// 052 backfill rule: items that ALREADY HAVE op-log rows must NOT
// have their content_flushed_op_log_id (or content_flushed_at)
// watermark backfilled to updated_at.
// Doing so would risk certifying an unflushed op-log row as
// "captured" by items.content — the sweeper would then delete the
// only durable copy.
//
// We can't run the migration in isolation in a test, but we can
// verify the equivalent end-state: an item with op-log rows but
// NULL watermark columns is preserved by the sweeper. Insert a
// row and a backdated op-log row directly, then explicitly NULL
// out the watermark to simulate the post-migration "items with
// op-log rows stay at NULL" outcome.
//
// Per Codex review of TASK-1309 round 3 [P1]: original backfill
// using `updated_at` could certify unflushed rows.
func TestOpLogGC_BackfillDoesNotCertifyUnflushed(t *testing.T) {
	srv := testServerWithCollab(t)

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "OpLogGC"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	col, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Tasks",
		Schema: `{"fields":[]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	item, err := srv.store.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title: "Pre-migration", Fields: `{}`, Content: "stale snapshot",
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	// Simulate pre-migration state: item exists with content + op-log
	// rows + a metadata-only PATCH that bumped updated_at PAST the
	// op-log row's created_at. The migration would see updated_at
	// > op-log MAX and naively backfill the watermark columns to
	// updated_at — but items.content is still stale relative to
	// the op-log row. Force the post-migration "stay NULL"
	// outcome by clearing both watermark columns directly.
	old := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	if _, err := srv.store.DB().Exec(
		`INSERT INTO item_yjs_updates (item_id, update_data, schema_version, created_at) VALUES (?, ?, ?, ?)`,
		item.ID, []byte{0, 0x01}, "1", old,
	); err != nil {
		t.Fatalf("seed op-log: %v", err)
	}
	// Bump updated_at AFTER the op-log row (simulating a metadata-
	// only PATCH at T_meta > T_op). NULL the watermark columns to
	// simulate the post-migration outcome for items with op-log
	// rows.
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := srv.store.DB().Exec(
		`UPDATE items SET updated_at = ?, content_flushed_at = NULL, content_flushed_op_log_id = NULL WHERE id = ?`,
		now, item.ID,
	); err != nil {
		t.Fatalf("simulate metadata patch: %v", err)
	}

	srv.SetOpLogGCConfig(10*time.Millisecond, 1*time.Minute)
	srv.StartOpLogGC()
	t.Cleanup(srv.Stop)

	// Sweeper should see content_flushed_op_log_id IS NULL and skip the
	// item. Op-log row must survive — it represents a real edit
	// that items.content does NOT contain.
	time.Sleep(200 * time.Millisecond)

	rows, err := srv.store.LoadYjsUpdatesSince(item.ID, 0)
	if err != nil {
		t.Fatalf("LoadYjsUpdatesSince: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("pre-migration unflushed row must survive (NULL watermark); got %d rows", len(rows))
	}
}

// TestOpLogGC_NoCollabIsNoop verifies the server can run StartOpLogGC
// without a collab room manager wired (no-collab build path). The
// goroutine spawns but every tick is a no-op.
func TestOpLogGC_NoCollabIsNoop(t *testing.T) {
	srv := testServer(t) // NOT testServerWithCollab — no room manager
	srv.SetOpLogGCConfig(10*time.Millisecond, 1*time.Minute)
	srv.StartOpLogGC()
	t.Cleanup(srv.Stop)

	// Let a few ticks happen; the goroutine should see s.collab==nil
	// and just return without touching the store.
	time.Sleep(100 * time.Millisecond)

	// If we got here without panic / data race / store touch, the
	// no-collab path is clean. Nothing else to assert; the absence
	// of badness IS the test.
	_ = collab.DefaultPruneMinAge // keep the import alive
}
