package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestItemVersionSeqBreaksSameSecondTies is the BUG-2270 regression guard.
//
// item_versions.created_at is second-precision RFC3339. A version RESTORE
// plus rapid edits can mint several versions inside one wall-clock second;
// they tie on created_at, and the id PK is a random UUIDv4 — useless as an
// ordering tie-breaker. Before the fix, ListItemVersions ordered solely by
// `created_at DESC`, so same-second versions came back in arbitrary order
// and the newest→oldest reverse-patch walk in ListItemVersionsResolved
// reconstructed corrupt history / the wrong "latest".
//
// version_seq (migration 076/054) is a per-item monotonic counter assigned
// COALESCE(MAX,0)+1 on every version INSERT. The ORDER BYs became
// `created_at DESC, version_seq DESC`, giving a deterministic tie-break.
//
// This test mints four versions through the real Create/Update paths, then
// collapses every version row into a SINGLE created_at second to force the
// exact collision the bug is about, and asserts both:
//   - ListItemVersions returns rows in strictly-descending version_seq order
//     (deterministic, and proving the column was actually populated), and
//   - ListItemVersionsResolved reconstructs the correct pre-update content
//     for each row — which only holds if the newest→oldest walk is ordered.
func TestItemVersionSeqBreaksSameSecondTies(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Version Seq Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	// Initial version (version_seq should become 1). CreateItem inserts a
	// version row when content is non-empty.
	item, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:   "Versioned",
		Content: "content-0",
		Source:  "cli",
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	// Three more versions via the real update path. ForceVersion guarantees a
	// version row each time regardless of the per-(actor,source) throttle or
	// timing. Each version row records items.content IMMEDIATELY BEFORE the
	// update (existing.Content), so the stored contents are:
	//   seq1 (initial)          -> "content-0"
	//   seq2 (before content-1) -> "content-0"
	//   seq3 (before content-2) -> "content-1"
	//   seq4 (before content-3) -> "content-2"
	updates := []string{"content-1", "content-2", "content-3"}
	var current *models.Item
	for _, c := range updates {
		c := c
		current, err = s.UpdateItem(item.ID, models.ItemUpdate{
			Content:        &c,
			LastModifiedBy: "user",
			Source:         "web",
			ForceVersion:   true,
		})
		if err != nil {
			t.Fatalf("UpdateItem %q: %v", c, err)
		}
	}
	if current == nil || current.Content != "content-3" {
		t.Fatalf("expected final content-3, got %+v", current)
	}

	// Collapse ALL version rows into one wall-clock second — the exact
	// BUG-2270 collision. Before the fix this made the ordering arbitrary.
	const sameSecond = "2026-01-01T00:00:00Z"
	if _, err := s.db.Exec(s.q(`UPDATE item_versions SET created_at = ? WHERE item_id = ?`), sameSecond, item.ID); err != nil {
		t.Fatalf("normalize created_at: %v", err)
	}

	// Ground truth: the ids in strict version_seq-descending order, plus the
	// seq values themselves so we can assert monotonicity + distinctness.
	rows, err := s.db.Query(s.q(`
		SELECT id, version_seq FROM item_versions
		WHERE item_id = ?
		ORDER BY version_seq DESC`), item.ID)
	if err != nil {
		t.Fatalf("query version_seq: %v", err)
	}
	var wantIDs []string
	var seqs []int64
	for rows.Next() {
		var id string
		var seq int64
		if err := rows.Scan(&id, &seq); err != nil {
			rows.Close()
			t.Fatalf("scan version_seq: %v", err)
		}
		wantIDs = append(wantIDs, id)
		seqs = append(seqs, seq)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("version_seq rows: %v", err)
	}

	if len(seqs) != 4 {
		t.Fatalf("expected 4 version rows (initial + 3 updates), got %d", len(seqs))
	}
	// Strictly descending and distinct (i.e. 4,3,2,1) — proves version_seq was
	// populated monotonically rather than left at the DEFAULT 0.
	for i := 1; i < len(seqs); i++ {
		if seqs[i] >= seqs[i-1] {
			t.Fatalf("version_seq not strictly descending: %v", seqs)
		}
	}
	if seqs[len(seqs)-1] < 1 {
		t.Fatalf("smallest version_seq must be >= 1 (populated), got %v", seqs)
	}

	// ListItemVersions must return rows in the same deterministic
	// version_seq-descending order despite every row sharing created_at.
	got, err := s.ListItemVersions(item.ID)
	if err != nil {
		t.Fatalf("ListItemVersions: %v", err)
	}
	if len(got) != len(wantIDs) {
		t.Fatalf("ListItemVersions returned %d rows, want %d", len(got), len(wantIDs))
	}
	for i := range got {
		if got[i].ID != wantIDs[i] {
			t.Fatalf("ListItemVersions order mismatch at %d: got id %s, want %s (order must follow version_seq DESC)", i, got[i].ID, wantIDs[i])
		}
	}

	// ListItemVersionsResolved reconstructs each row's pre-update content by
	// walking newest→oldest. Correct output is only possible if the ordering
	// is deterministic — a wrong same-second order misapplies the reverse
	// patches and yields garbage. currentContent is the live items.content.
	resolved, err := s.ListItemVersionsResolved(item.ID, current.Content)
	if err != nil {
		t.Fatalf("ListItemVersionsResolved: %v", err)
	}
	wantContent := []string{"content-2", "content-1", "content-0", "content-0"}
	if len(resolved) != len(wantContent) {
		t.Fatalf("ListItemVersionsResolved returned %d rows, want %d", len(resolved), len(wantContent))
	}
	for i := range resolved {
		if resolved[i].Content != wantContent[i] {
			t.Fatalf("resolved content mismatch at %d: got %q, want %q (corrupt reconstruction implies non-deterministic same-second ordering)", i, resolved[i].Content, wantContent[i])
		}
	}
}
