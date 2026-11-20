package store

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
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

func TestCommitmentJournalSameSecondHistoryAndExport(t *testing.T) {
	f := newSchedFixture(t)
	sc := f.nativeCommit(t)
	for _, row := range []struct{ id, at string }{{"z-exact", "2026-10-27T00:00:00Z"}, {"b-half", "2026-10-27T00:00:00.5Z"}, {"a-half", "2026-10-27T00:00:00.500Z"}, {"c-later", "2026-10-27T00:00:00.51Z"}} {
		f.exec(t, `INSERT INTO schedule_changes(id,workspace_id,item_id,schedule_id,change_type,role,before_json,after_json,actor,reason,reason_source,root_change_id,recorded_at) VALUES(?,?,?,?,'move','target','{}','{}','user','move','user',?,?)`, row.id, f.ws.ID, f.item.ID, sc.ID, row.id, row.at)
	}
	got, err := f.s.GetCommitment(f.ws.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"z-exact", "a-half", "b-half", "c-later"}
	if len(got.Journal) != len(want) {
		t.Fatalf("journal %+v", got.Journal)
	}
	for i, id := range want {
		if got.Journal[i].ID != id {
			t.Fatalf("history order: %+v", got.Journal)
		}
	}
	bundle, err := f.s.ExportWorkspace(f.ws.Slug)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range bundle.ScheduleChanges {
		if r.ChangeType == "move" {
			ids = append(ids, r.ID)
		}
	}
	if len(ids) != len(want) {
		t.Fatalf("export changes %+v", bundle.ScheduleChanges)
	}
	for i, id := range want {
		if ids[i] != id {
			t.Fatalf("export order: %v", ids)
		}
	}
}

// A recurring schedule journals one ack per acknowledged occurrence; the view
// keeps only the newest commitmentJournalAckLimit of them and counts the rest.
func TestGetCommitmentJournalCapsAckRows(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.nativeCommit(t)
	base := rt("2026-10-27T00:00:00Z")
	at := func(i int) string { return base.Add(time.Duration(i) * time.Minute).UTC().Format(time.RFC3339Nano) }
	ack := func(i int) {
		id := fmt.Sprintf("ack-%03d", i)
		f.exec(t, `INSERT INTO schedule_changes(id,workspace_id,item_id,schedule_id,occurrence_key,change_type,actor,reason,reason_source,root_change_id,recorded_at) VALUES(?,?,?,?,?,'ack','user','','user',?,?)`, id, f.ws.ID, f.item.ID, sc.ID, id, id, at(i))
	}
	read := func() (*models.Commitment, map[string]any) {
		t.Helper()
		got, err := f.s.GetCommitment(f.ws.ID, f.item.ID)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return got, m
	}
	// The oldest row is a non-ack move, older than every ack that gets dropped.
	f.exec(t, `INSERT INTO schedule_changes(id,workspace_id,item_id,schedule_id,change_type,role,before_json,after_json,actor,reason,reason_source,root_change_id,recorded_at) VALUES(?,?,?,?,'move','target','{}','{}','user','move','user',?,?)`, "move-oldest", f.ws.ID, f.item.ID, sc.ID, "move-oldest", at(-1))
	for i := 0; i < commitmentJournalAckLimit; i++ {
		ack(i)
	}
	got, m := read()
	if len(got.Journal) != commitmentJournalAckLimit+1 || got.JournalAcksOmitted != 0 {
		t.Fatalf("at the limit: len=%d omitted=%d", len(got.Journal), got.JournalAcksOmitted)
	}
	if _, ok := m["journal_acks_omitted"]; ok {
		t.Fatalf("journal_acks_omitted present at the limit: %v", m["journal_acks_omitted"])
	}
	ack(commitmentJournalAckLimit)
	ack(commitmentJournalAckLimit + 1)
	got, m = read()
	acks := 0
	for _, c := range got.Journal {
		if c.ChangeType == "ack" {
			acks++
		}
	}
	if acks != commitmentJournalAckLimit || got.JournalAcksOmitted != 2 || m["journal_acks_omitted"] != float64(2) {
		t.Fatalf("acks=%d omitted=%d json=%v", acks, got.JournalAcksOmitted, m["journal_acks_omitted"])
	}
	if len(got.Journal) != commitmentJournalAckLimit+1 || got.Journal[0].ID != "move-oldest" || got.Journal[1].ID != "ack-002" || got.Journal[len(got.Journal)-1].ID != "ack-051" {
		t.Fatalf("kept rows: first=%s second=%s last=%s len=%d", got.Journal[0].ID, got.Journal[1].ID, got.Journal[len(got.Journal)-1].ID, len(got.Journal))
	}
}
