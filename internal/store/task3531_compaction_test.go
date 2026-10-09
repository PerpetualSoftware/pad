package store_test

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-3531: CompactItemOpLog replaces a dormant, flushed op-log with ONE
// snapshot row, and CompactedResumeCovers admits a cursor the snapshot covers
// only while that row exists.

var compactFrame = []byte{0x00, 0x02, 0x02, 0xAB, 0xCD}

// seedDormantLog appends n frames to a fresh item, ages them past an hour ago,
// flushes the watermark to the last, and returns the item and its max id.
func seedDormantLog(t *testing.T, s *store.Store, backend string, n int) (string, int64) {
	t.Helper()
	_, _, item := seedStaleItem(t, s)
	var last int64
	for i := 0; i < n; i++ {
		last = appendFrame(t, s, item.ID, []byte{0x00, 0x02, 0x02, byte(i), 0x7f})
	}
	if _, err := s.DB().Exec(rebind2(backend, `UPDATE item_yjs_updates SET created_at = '2026-01-01T00:00:00Z' WHERE item_id = ?`), item.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetItemContentFlushedOpLogIDForTesting(item.ID, last); err != nil {
		t.Fatal(err)
	}
	return item.ID, last
}

// rebind2 turns this file's single ? (or two) into Postgres placeholders.
func rebind2(backend, q string) string {
	if backend != "Postgres" {
		return q
	}
	out := []byte{}
	n := 0
	for i := 0; i < len(q); i++ {
		if q[i] == '?' {
			n++
			out = append(out, []byte("$"+string(rune('0'+n)))...)
			continue
		}
		out = append(out, q[i])
	}
	return string(out)
}

func compactionCols(t *testing.T, s *store.Store, backend, itemID string) (through, snap sql.NullInt64) {
	t.Helper()
	if err := s.DB().QueryRow(rebind2(backend, `SELECT yjs_compacted_through, yjs_snapshot_op_id FROM items WHERE id = ?`), itemID).Scan(&through, &snap); err != nil {
		t.Fatal(err)
	}
	return through, snap
}

func opLogIDs(t *testing.T, s *store.Store, itemID string) []int64 {
	t.Helper()
	rows, err := s.LoadYjsUpdatesSince(itemID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

func TestTASK3531_CompactReplacesTheLogWithOneSnapshot(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			itemID, maxID := seedDormantLog(t, s, b.name, 3)
			cutoff := time.Now().Add(-time.Hour)

			snapID, err := s.CompactItemOpLog(itemID, cutoff, maxID, compactFrame, "1")
			if err != nil {
				t.Fatal(err)
			}
			if ids := opLogIDs(t, s, itemID); len(ids) != 1 || ids[0] != snapID {
				t.Fatalf("op-log ids %v, want only the snapshot %d", ids, snapID)
			}
			through, snap := compactionCols(t, s, b.name, itemID)
			if through.Int64 != maxID || snap.Int64 != snapID {
				t.Fatalf("columns through=%v snap=%v, want %d and %d", through, snap, maxID, snapID)
			}
			if got := contentState(t, s, itemID); got != "" {
				t.Fatalf("a compacted item must read flushed; got %q", got)
			}
			if only, err := s.IsCompactedLog(itemID); err != nil || !only {
				t.Fatalf("IsCompactedLog = %v, %v", only, err)
			}
			for since, want := range map[int64]bool{1: true, maxID: true, maxID + 1: false, 0: false} {
				if got, err := s.CompactedResumeCovers(itemID, since); err != nil || got != want {
					t.Fatalf("covers(since=%d) = %v, %v; want %v", since, got, err, want)
				}
			}
		})
	}
}

func TestTASK3531_CompactRefusesAChangedOrUnflushedLog(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			cases := map[string]func(s *store.Store, itemID string, maxID int64) (cutoff time.Time, expect int64){
				"a row arrived since the read": func(s *store.Store, itemID string, maxID int64) (time.Time, int64) {
					return time.Now().Add(-time.Hour), maxID - 1
				},
				"not flushed": func(s *store.Store, itemID string, maxID int64) (time.Time, int64) {
					if err := s.SetItemContentFlushedOpLogIDForTesting(itemID, maxID-1); err != nil {
						t.Fatal(err)
					}
					return time.Now().Add(-time.Hour), maxID
				},
				"no longer dormant": func(s *store.Store, itemID string, maxID int64) (time.Time, int64) {
					return time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), maxID
				},
			}
			for name, setup := range cases {
				t.Run(name, func(t *testing.T) {
					s := b.open(t)
					itemID, maxID := seedDormantLog(t, s, b.name, 3)
					cutoff, expect := setup(s, itemID, maxID)
					before := opLogIDs(t, s, itemID)
					if _, err := s.CompactItemOpLog(itemID, cutoff, expect, compactFrame, "1"); !errors.Is(err, store.ErrCompactionRefused) {
						t.Fatalf("got %v, want ErrCompactionRefused", err)
					}
					after := opLogIDs(t, s, itemID)
					if len(after) != len(before) {
						t.Fatalf("a refused compaction changed the op-log: %v -> %v", before, after)
					}
					if through, snap := compactionCols(t, s, b.name, itemID); through.Valid || snap.Valid {
						t.Fatal("a refused compaction set the columns")
					}
				})
			}
		})
	}
}

// Every path that deletes the whole log clears the compaction, and even a
// left-over yjs_compacted_through admits nobody once the snapshot row is gone.
func TestTASK3531_DeletingTheLogEndsTheCoverage(t *testing.T) {
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
			}
			for name, del := range deleters {
				t.Run(name, func(t *testing.T) {
					s := b.open(t)
					itemID, maxID := seedDormantLog(t, s, b.name, 2)
					if _, err := s.CompactItemOpLog(itemID, time.Now().Add(-time.Hour), maxID, compactFrame, "1"); err != nil {
						t.Fatal(err)
					}
					del(s, itemID)
					if through, snap := compactionCols(t, s, b.name, itemID); through.Valid || snap.Valid {
						t.Fatal("deleting the log left the compaction columns set")
					}
					if covers, err := s.CompactedResumeCovers(itemID, 1); err != nil || covers {
						t.Fatalf("covers after delete = %v, %v", covers, err)
					}
				})
			}

			// The backstop: a path that deleted the log WITHOUT clearing the
			// columns still admits nobody, because the snapshot row is gone.
			s := b.open(t)
			itemID, maxID := seedDormantLog(t, s, b.name, 2)
			if _, err := s.CompactItemOpLog(itemID, time.Now().Add(-time.Hour), maxID, compactFrame, "1"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB().Exec(rebind2(b.name, `DELETE FROM item_yjs_updates WHERE item_id = ?`), itemID); err != nil {
				t.Fatal(err)
			}
			if through, _ := compactionCols(t, s, b.name, itemID); !through.Valid {
				t.Fatal("premise: the raw delete left the columns set")
			}
			if covers, err := s.CompactedResumeCovers(itemID, 1); err != nil || covers {
				t.Fatalf("a stale yjs_compacted_through admitted a cursor with no snapshot row: %v, %v", covers, err)
			}
		})
	}
}
