package store_test

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// TASK-3532: on Postgres a row can be allocated a LOWER id and commit AFTER a
// compaction job read the op-log. MAX(id) is unchanged, so the swap's
// re-check passes; a range delete (id <= MAX) would then remove a row the
// snapshot never contained, and its content would be gone. Compaction deletes
// exactly the ids it read, so the late row survives, below the snapshot.
//
// SQLite cannot produce the interleaving (_txlock=immediate serializes
// writers, so ids commit in allocation order); this is a Postgres test.
func TestTASK3532_ALateLowerIDRowSurvivesCompaction(t *testing.T) {
	s := storetest.NewPostgres(t)
	itemID, _ := seedDormantLog(t, s, "Postgres", 2)

	// A writer allocates the next id and holds its transaction open.
	late, err := s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = late.Rollback() }()
	var lateID int64
	if err := late.QueryRow(`
		INSERT INTO item_yjs_updates (item_id, update_data, schema_version, created_at, content_hash, content_bearing)
		VALUES ($1, $2, '1', '2026-01-01T00:00:00Z', 'late', TRUE) RETURNING id`, itemID, []byte{0x00, 0x02, 0x02, 0x66, 0x7f}).Scan(&lateID); err != nil {
		t.Fatal(err)
	}

	// Another row commits with a HIGHER id, aged and flushed like the rest.
	higher := appendFrame(t, s, itemID, []byte{0x00, 0x02, 0x02, 0x77, 0x7f})
	if higher <= lateID {
		t.Fatalf("premise: the committed row's id %d is above the open one's %d", higher, lateID)
	}
	if _, err := s.DB().Exec(`UPDATE item_yjs_updates SET created_at = '2026-01-01T00:00:00Z' WHERE item_id = $1`, itemID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetItemContentFlushedOpLogIDForTesting(itemID, higher); err != nil {
		t.Fatal(err)
	}

	// The job reads the log: the open row is invisible to it.
	read := opLogIDs(t, s, itemID)
	for _, id := range read {
		if id == lateID {
			t.Fatal("premise: the uncommitted row is not in the job's read")
		}
	}
	if read[len(read)-1] != higher {
		t.Fatalf("premise: the read's max is %d, want %d", read[len(read)-1], higher)
	}

	// The late row commits before the swap.
	if err := late.Commit(); err != nil {
		t.Fatal(err)
	}

	// MAX(id) is still `higher`, so the swap's re-check passes...
	if _, err := s.CompactItemOpLog(itemID, time.Now().Add(-time.Hour), read, compactFrame, "1"); err != nil {
		t.Fatalf("compact: %v", err)
	}
	// ...and the late row, which the snapshot does not contain, is still there.
	after := opLogIDs(t, s, itemID)
	found := false
	for _, id := range after {
		if id == lateID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the late row %d was deleted by the compaction (op-log now %v): its content is gone", lateID, after)
	}
	if len(after) != 2 {
		t.Fatalf("op-log %v; want the late row and the snapshot", after)
	}
}
