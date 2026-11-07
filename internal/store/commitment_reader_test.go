package store

import (
	"github.com/PerpetualSoftware/pad/internal/models"
	"testing"
)

func TestGetCommitmentMoveAndAssigneeJournal(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.nativeCommit(t)
	if err := f.s.renegotiateDeadlineAt(f.ws.ID, f.item.ID, "user", rt("2026-10-27T14:00:00Z"), "more time", renegNow); err != nil {
		t.Fatal(err)
	}
	u := f.user(t, "reader@example.com", "")
	if _, err := f.s.UpdateItem(f.item.ID, models.ItemUpdate{AssignedUserID: &u.ID}); err != nil {
		t.Fatal(err)
	}
	// Lookalikes from other writers must never enter the commitment history.
	for _, row := range []struct {
		id, typ, source string
		role, before    any
		reason          string
	}{
		{"unrelated-update", "update", "system", "deadline", nil, "Assignee changed from a to b"},
		{"roleless-miss", "missed_recorded", "system", nil, nil, ""},
		{"system-move", "move", "system", "deadline", nil, "more time"},
		{"snapshot-update", "update", "system", nil, `{}`, "Assignee changed from a to b"},
		{"other-update", "update", "system", nil, nil, "Something else changed"},
	} {
		f.exec(t, `INSERT INTO schedule_changes (id,workspace_id,item_id,schedule_id,change_type,role,before_json,actor,reason,reason_source,root_change_id,recorded_at) VALUES (?,?,?,?,?,?,?,'system',?,?,?,?)`, row.id, f.ws.ID, f.item.ID, sc.ID, row.typ, row.role, row.before, row.reason, row.source, row.id, nowNano())
	}
	got, err := f.s.GetCommitment(f.ws.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Journal) != 2 || got.Journal[0].ChangeType != "move" || got.Journal[0].Reason != "more time" || got.Journal[1].ChangeType != "update" || got.Journal[1].Reason != "Assignee changed from  to "+u.ID {
		t.Fatalf("journal %+v", got.Journal)
	}
	// Equal timestamps use id as the stable tie breaker.
	f.exec(t, `UPDATE schedule_changes SET recorded_at=? WHERE schedule_id=?`, "2026-10-27T00:00:00Z", sc.ID)
	got, err = f.s.GetCommitment(f.ws.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Journal) != 2 || got.Journal[0].ID >= got.Journal[1].ID {
		t.Fatalf("order %+v", got.Journal)
	}
}
