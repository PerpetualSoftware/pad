package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-2198 U4: the recovery write's watermark is its CURSOR, never MAX at
// UPDATE time. On Postgres (READ COMMITTED) a row another connection commits
// after the write's checks is visible to the UPDATE's own subqueries, so a
// write that stamped MAX would claim a row its markdown never saw, and
// content_state would read clean over an unrecovered edit.
//
// Postgres only: on SQLite the hook's append would wait on the write
// transaction's lock, so the window does not exist there.
func TestMaterializeFlushWatermarkIsTheCursorNotMax(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverPostgres {
		t.Skip("the READ COMMITTED window this pins exists on Postgres only (make test-pg)")
	}
	ws := createTestWorkspace(t, s, "Recovery race")
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Notes", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	it, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "Racy", Content: "stale", Fields: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := s.AppendYjsUpdate(it.ID, []byte{0x00, 0x02, 0x05, 0x01, 0x01, 0x00, 0x7F, 0x00}, "1")
	if err != nil {
		t.Fatal(err)
	}
	var late int64
	materializeFlushAfterCheckHook = func(itemID string) {
		materializeFlushAfterCheckHook = nil
		var aerr error
		late, aerr = s.AppendYjsUpdate(itemID, []byte{0x00, 0x02, 0x05, 0x01, 0x02, 0x00, 0x7F, 0x00}, "1")
		if aerr != nil {
			t.Errorf("late append: %v", aerr)
		}
	}
	t.Cleanup(func() { materializeFlushAfterCheckHook = nil })

	outcome, _, err := s.MaterializeFlush(it.ID, cursor, "recovered")
	if err != nil || outcome != MaterializeApplied {
		t.Fatalf("MaterializeFlush = %q, %v", outcome, err)
	}
	if late <= cursor {
		t.Fatalf("the late row %d did not land after the cursor %d", late, cursor)
	}
	wm, _, err := s.GetItemContentFlushedOpLogID(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if wm == late {
		t.Fatalf("watermark %d covers the late row the recovered body never saw", wm)
	}
	got, _ := s.GetItem(it.ID)
	if got.ContentState != models.ContentStatePendingFlush {
		t.Fatalf("content_state %q, want still pending for the late row", got.ContentState)
	}
}
