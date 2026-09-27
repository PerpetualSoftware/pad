package store

import (
	"testing"
)

// BUG-3244 codex r1: the rebuild's setup lock does not stop a connected peer
// appending, and on Postgres each statement in the move takes its own
// snapshot. A content-bearing row committed after the rows to keep were chosen
// but before the op-log was cleared used to be deleted without being set
// aside. The rows kept are now the rows the DELETE returned, so a row that
// lands in that gap is either left in the op-log or set aside, never lost.
//
// Postgres only: SQLite serialises writers, so the gap cannot open there (and
// an append from another connection inside the transaction would deadlock).
func TestSetAsideKeepsARowAppendedInsideTheMove(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() == DriverSQLite {
		t.Skip("Postgres only: SQLite serialises writers, so the gap cannot open")
	}
	ws := createTestWorkspace(t, s, "SetAsideRace")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Racing", "stored body")

	first := []byte{0x00, 0x02, 0x05, 0x01, 0x44, 0x00, 0x7F, 0x00}
	late := []byte{0x00, 0x02, 0x05, 0x01, 0x45, 0x00, 0x7F, 0x00}
	if _, err := s.AppendYjsUpdate(item.ID, first, "1"); err != nil {
		t.Fatalf("AppendYjsUpdate: %v", err)
	}

	setAsideBeforeClearHook = func() {
		// Its own connection, committed before the move's DELETE runs.
		if _, err := s.AppendYjsUpdate(item.ID, late, "1"); err != nil {
			t.Errorf("append inside the move: %v", err)
		}
	}
	t.Cleanup(func() { setAsideBeforeClearHook = nil })

	moved, cleared, err := s.SetAsideAndClearOpLog(item.ID)
	setAsideBeforeClearHook = nil
	if err != nil {
		t.Fatalf("SetAsideAndClearOpLog: %v", err)
	}

	rows, err := s.ListYjsSetAside(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	ops, err := s.LoadYjsUpdatesSince(item.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Wherever the late row went, it must be somewhere.
	found := len(ops) == 1 && string(ops[0].UpdateData) == string(late)
	for _, r := range rows {
		if string(r.UpdateData) == string(late) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the row appended inside the move is in neither table: moved %d, cleared %d, set-aside %d, op-log %d", moved, cleared, len(rows), len(ops))
	}
	if int64(len(rows)) != moved {
		t.Fatalf("moved = %d but %d rows are set aside", moved, len(rows))
	}
}
