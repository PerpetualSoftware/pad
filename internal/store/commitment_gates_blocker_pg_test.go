package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// A blocker's writer and a new gate on that blocker serialize on the blocker's
// row. A gate created while the write is uncommitted waits for it, then sees the
// success, so the opening cannot fall between the two.
func TestTASK4423_R09GateCreationWaitsForBlockerWriter(t *testing.T) {
	f := newLinkPG(t, 1)
	blocker := createTestItem(t, f.s, f.ws.ID, f.items[0].CollectionID, "pg blocker", "")
	acc := f.access()
	acc.Readable[blocker.ID], acc.Editable[blocker.ID] = true, true

	writer, err := f.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.Exec(`UPDATE items SET fields=$1 WHERE id=$2`, `{"status":"done"}`, blocker.ID); err != nil {
		t.Fatal(err)
	}

	type result struct {
		gate *models.ScheduleGate
		err  error
	}
	done := make(chan result, 1)
	go func() {
		g, err := f.s.CreateScheduleGate(f.ws.ID, f.items[0].ID, blocker.ID, acc)
		done <- result{g, err}
	}()
	f.waitForWaiters(t, 1)
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.gate.OpenedAt == nil {
			t.Fatal("the gate missed the success its blocker committed while it waited")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("gate creation did not finish")
	}
}
