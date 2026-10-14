package store_test

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3747: a compaction snapshot covers the cursors of ITS generation only.
// After a path deletes the op-log (a direct write, a restore, a dormant prune,
// a schema rebuild), the next rows start a new generation with higher ids. Once
// that generation is compacted past an old tab's cursor, the cursor must still
// read as uncovered: the tab built its Y.Doc on rows that no longer exist, and
// admitting it would merge the deleted generation's text back in.

// ageAndFlush makes the item's whole op-log dormant and flushed through last.
func ageAndFlush(t *testing.T, s *store.Store, backend, itemID string, last int64) {
	t.Helper()
	if _, err := s.DB().Exec(rebind2(backend, `UPDATE item_yjs_updates SET created_at = '2026-01-01T00:00:00Z' WHERE item_id = ?`), itemID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetItemContentFlushedOpLogIDForTesting(itemID, last); err != nil {
		t.Fatal(err)
	}
}

func compactAll(t *testing.T, s *store.Store, itemID string) int64 {
	t.Helper()
	snap, err := s.CompactItemOpLog(itemID, time.Now().Add(-time.Hour), opLogIDs(t, s, itemID), compactFrame, "1")
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestBUG3747_ACompactionDoesNotCoverAnEarlierGenerationsCursor(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			deleters := map[string]func(s *store.Store, itemID string){
				"direct write / restore (PruneItemOpLogTx)": func(s *store.Store, itemID string) {
					tx, err := s.DB().Begin()
					if err != nil {
						t.Fatal(err)
					}
					if err := s.PruneItemOpLogTx(tx, itemID); err != nil {
						t.Fatal(err)
					}
					if err := tx.Commit(); err != nil {
						t.Fatal(err)
					}
				},
				"dormant prune": func(s *store.Store, itemID string) {
					if n, err := s.PruneItemOpLogIfDormantBefore(itemID, time.Now().Add(time.Hour)); err != nil || n == 0 {
						t.Fatalf("prune: %d, %v", n, err)
					}
				},
				"schema rebuild (set aside)": func(s *store.Store, itemID string) {
					if _, _, err := s.SetAsideAndClearOpLog(itemID); err != nil {
						t.Fatal(err)
					}
				},
				// A path that forgets to clear the columns: the stale floor
				// must not carry into the next generation's snapshot.
				"raw delete that leaves the columns set": func(s *store.Store, itemID string) {
					if _, err := s.DB().Exec(rebind2(b.name, `DELETE FROM item_yjs_updates WHERE item_id = ?`), itemID); err != nil {
						t.Fatal(err)
					}
				},
			}
			for name, del := range deleters {
				t.Run(name, func(t *testing.T) {
					s := b.open(t)
					// Generation 1: the old tab applied every row through oldCursor.
					itemID, oldCursor := seedDormantLog(t, s, b.name, 3)
					compactAll(t, s, itemID)
					if covers, err := s.CompactedResumeCovers(itemID, oldCursor); err != nil || !covers {
						t.Fatalf("premise: generation 1's own cursor is covered; got %v, %v", covers, err)
					}
					del(s, itemID)

					// Generation 2: seeded and edited past the old cursor, then compacted.
					var first, last int64
					for i := 0; i < 3; i++ {
						last = appendFrame(t, s, itemID, []byte{0x00, 0x02, 0x02, byte(0x40 + i), 0x7f})
						if first == 0 {
							first = last
						}
					}
					if first <= oldCursor {
						t.Fatalf("premise: a new generation's ids are above the old cursor (%d <= %d)", first, oldCursor)
					}
					ageAndFlush(t, s, b.name, itemID, last)
					compactAll(t, s, itemID)
					if through, _ := compactionCols(t, s, b.name, itemID); through.Int64 < oldCursor {
						t.Fatalf("premise: generation 2 compacted through %d, past the old cursor %d", through.Int64, oldCursor)
					}

					for since, want := range map[int64]bool{oldCursor: false, 1: false, first: true, last: true, last + 1: false} {
						if got, err := s.CompactedResumeCovers(itemID, since); err != nil || got != want {
							t.Fatalf("covers(since=%d) = %v, %v; want %v (generation 2 is %d..%d, the old cursor %d)", since, got, err, want, first, last, oldCursor)
						}
					}
				})
			}
		})
	}
}

// Compacting a generation again keeps the cursors the first snapshot covered:
// the new snapshot replaces the old one, and the generation's start carries.
func TestBUG3747_ARecompactionKeepsItsGenerationsCursors(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			itemID, _ := seedDormantLog(t, s, b.name, 3)
			first := opLogIDs(t, s, itemID)[0]
			compactAll(t, s, itemID)
			var last int64
			for i := 0; i < 2; i++ {
				last = appendFrame(t, s, itemID, []byte{0x00, 0x02, 0x02, byte(0x60 + i), 0x7f})
			}
			ageAndFlush(t, s, b.name, itemID, last)
			compactAll(t, s, itemID)
			for since, want := range map[int64]bool{first: true, last: true, last + 1: false} {
				if got, err := s.CompactedResumeCovers(itemID, since); err != nil || got != want {
					t.Fatalf("covers(since=%d) = %v, %v; want %v", since, got, err, want)
				}
			}
		})
	}
}

// An item compacted before migration 145 has a snapshot but no floor. Nothing
// says which generation its cursors came from, so none is covered: those tabs
// refresh once, the safe answer.
func TestBUG3747_ASnapshotWithoutAFloorCoversNothing(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			itemID, last := seedDormantLog(t, s, b.name, 3)
			compactAll(t, s, itemID)
			if covers, err := s.CompactedResumeCovers(itemID, last); err != nil || !covers {
				t.Fatalf("premise: covered with a floor; got %v, %v", covers, err)
			}
			if _, err := s.DB().Exec(rebind2(b.name, `UPDATE items SET yjs_compacted_from = NULL WHERE id = ?`), itemID); err != nil {
				t.Fatal(err)
			}
			if covers, err := s.CompactedResumeCovers(itemID, last); err != nil || covers {
				t.Fatalf("a snapshot with no floor covered cursor %d: %v, %v", last, covers, err)
			}
		})
	}
}

// A row allocated a lower id that commits after a snapshot (Postgres) sits
// below the snapshot at the next compaction, so the previous snapshot is not
// the oldest row replaced. Its floor still carries: the generation's cursors
// from the first snapshot stay covered.
func TestBUG3747_TheFloorCarriesPastALateLowerRow(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			itemID, _ := seedDormantLog(t, s, b.name, 3)
			first := opLogIDs(t, s, itemID)[0]
			snap1 := compactAll(t, s, itemID)
			late := first + 1 // freed by the first compaction, below snap1
			if late >= snap1 {
				t.Fatalf("premise: an id below the snapshot is free (%d >= %d)", late, snap1)
			}
			if _, err := s.DB().Exec(rebind2(b.name, `INSERT INTO item_yjs_updates (id, item_id, update_data, schema_version, created_at) SELECT ?, item_id, update_data, schema_version, created_at FROM item_yjs_updates WHERE id = ?`), late, snap1); err != nil {
				t.Fatal(err)
			}
			last := appendFrame(t, s, itemID, []byte{0x00, 0x02, 0x02, 0x70, 0x7f})
			ageAndFlush(t, s, b.name, itemID, last)
			if ids := opLogIDs(t, s, itemID); ids[0] != late || ids[1] != snap1 {
				t.Fatalf("premise: the late row is the oldest, then the snapshot: %v", ids)
			}
			compactAll(t, s, itemID)
			for since, want := range map[int64]bool{first: true, late: true, last: true} {
				if got, err := s.CompactedResumeCovers(itemID, since); err != nil || got != want {
					t.Fatalf("covers(since=%d) = %v, %v; want %v", since, got, err, want)
				}
			}
		})
	}
}
