package store

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The pin reads the seeds and the link component before it has the schedule
// locks. On Postgres a cross-workspace move can commit while the pin waits for
// them: it archives the end, ends its schedule and deletes its links. The pin
// re-checks all of that under the locks. SQLite serialises writers, so these
// tests make the same change on the pin's own transaction at the point the
// locks are taken (afterCommitmentLocks). The Postgres interleavings are in
// commitment_links_pg_test.go.

// movedAway does to schedule i what a cross-workspace move that committed
// first would have done.
func (f *linkFix) movedAway(i int) func(tx *sql.Tx) {
	return func(tx *sql.Tx) {
		id, item := f.scheds[i].ID, f.items[i].ID
		for _, st := range []struct {
			q    string
			args []any
		}{
			{`DELETE FROM schedule_links WHERE parent_schedule_id=? OR child_schedule_id=?`, []any{id, id}},
			{`UPDATE item_schedules SET ended_at=?, next_at=NULL, next_wall=NULL WHERE id=?`, []any{nowNano(), id}},
			{`UPDATE items SET deleted_at=? WHERE id=?`, []any{nowNano(), item}},
		} {
			if _, err := tx.Exec(f.s.q(st.q), st.args...); err != nil {
				panic(err)
			}
		}
	}
}

func (f *linkFix) linkGone(id string) func(tx *sql.Tx) {
	return func(tx *sql.Tx) {
		if _, err := tx.Exec(f.s.q(`DELETE FROM schedule_links WHERE id=?`), id); err != nil {
			panic(err)
		}
	}
}

func TestACreateWhoseEndMovedAwayWhileItWaitedWritesNoLink(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.s.afterCommitmentLocks = f.movedAway(1)
	_, err := f.s.CreateScheduleLink(f.ws.ID, models.ScheduleLinkCreate{ParentItemID: f.items[0].ID, ParentRole: "target", ChildItemID: f.items[1].ID, ChildRole: "target", Offset: oneDay}, f.allEdit())
	if err != ErrScheduleLinkNotFound {
		t.Fatalf("err=%v, want the ErrScheduleLinkNotFound value", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_links`); n != 0 {
		t.Fatalf("%d links on the moved-away end", n)
	}
}

func TestAnUpdateOfALinkThatVanishedWhileItWaitedIsNotFound(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.link(t, 0, 1, "target", "target", oneDay)
	id := f.onlyLinkID(t)
	f.s.afterCommitmentLocks = f.movedAway(1)
	upd := models.ScheduleLinkUpdate{CancelPolicy: sp(models.LinkCancelChildOccurrence)}
	res, err := f.s.UpdateScheduleLink(f.ws.ID, id, upd, f.allEdit())
	if err != ErrScheduleLinkNotFound {
		t.Fatalf("res=%+v err=%v, want the ErrScheduleLinkNotFound value", res, err)
	}
}

func TestADeleteOfALinkThatVanishedWhileItWaitedIsNotFound(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.link(t, 0, 1, "target", "target", oneDay)
	id := f.onlyLinkID(t)
	f.s.afterCommitmentLocks = f.linkGone(id)
	if err := f.s.DeleteScheduleLink(f.ws.ID, id, f.allEdit()); err != ErrScheduleLinkNotFound {
		t.Fatalf("err=%v, want the ErrScheduleLinkNotFound value", err)
	}
}

// A cascade works from the links under the locks: a link another transaction
// removed moves nothing.
func TestACascadeIgnoresALinkRemovedWhileItWaited(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.link(t, 0, 1, "target", "target", oneDay)
	id := f.onlyLinkID(t)
	before := f.role(t, 1, "target")
	f.s.afterCommitmentLocks = f.linkGone(id)
	res, err := f.move(0, "target", "2030-03-10T09:00:00Z", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.role(t, 1, "target"); got != before {
		t.Fatalf("child moved to %s through a removed link (%d changes)", got, len(res.Changes))
	}
}

// ---- review findings on the linked-schedule guards ----

func TestALinkedHardDeadlineScheduleAnswersLinkedBeforeRenegotiate(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 3)
	f.exec(t, `UPDATE item_schedules SET roles_origin='native', deadline_days=2, deadline_secs=0, deadline_behavior='stays_owed' WHERE id IN (?,?)`, f.scheds[1].ID, f.scheds[2].ID)
	f.linkRaw(t, 0, 1, "target", "target", oneDay)
	// Item 2 has a hard deadline and no link: the deadline refusal stands.
	if _, err := f.s.UpdateSchedule(f.ws.ID, f.items[2].ID, models.ScheduleUpdate{Tz: sp("America/New_York")}); !errors.Is(err, ErrHardDeadlineRenegotiate) {
		t.Fatalf("unlinked: %v", err)
	}
	// Item 1 has both: the link is the refusal that applies.
	for name, upd := range map[string]models.ScheduleUpdate{
		"zone":    {Tz: sp("America/New_York")},
		"dtstart": {Dtstart: sp("2030-03-20T09:00")},
	} {
		if _, err := f.s.UpdateSchedule(f.ws.ID, f.items[1].ID, upd); !errors.Is(err, ErrScheduleLinked) {
			t.Fatalf("linked %s: %v", name, err)
		}
	}
}

func TestASeriesRenegotiationPreviewOfALinkedDeadlineIsRefusedToo(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.exec(t, `UPDATE item_schedules SET roles_origin='native', deadline_days=2, deadline_secs=0, deadline_behavior='stays_owed' WHERE id=?`, f.scheds[1].ID)
	f.linkRaw(t, 0, 1, "target", "deadline", oneDay)
	journal := f.count(t, `SELECT COUNT(*) FROM schedule_changes`)
	in := models.CommitmentRenegotiateInput{Scope: "series", Reason: "slip", RemoveDeadline: true, Preview: true}
	if _, err := f.s.RenegotiateCommitment(f.ws.ID, f.items[1].ID, "user", in, nil); !errors.Is(err, ErrScheduleLinked) {
		t.Fatalf("preview: %v", err)
	}
	// An apply with a revision taken while the schedule was not linked is refused
	// the same way, before anything is written.
	f.exec(t, `DELETE FROM schedule_links`)
	in.Preview = true
	p, err := f.s.RenegotiateCommitment(f.ws.ID, f.items[1].ID, "user", in, nil)
	if err != nil {
		t.Fatalf("unlinked preview: %v", err)
	}
	f.linkRaw(t, 0, 1, "target", "deadline", oneDay)
	in.Preview, in.Revision = false, p.Revision
	if _, err := f.s.RenegotiateCommitment(f.ws.ID, f.items[1].ID, "user", in, nil); !errors.Is(err, ErrScheduleLinked) {
		t.Fatalf("apply: %v", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes`); n != journal {
		t.Fatalf("journal grew from %d to %d", journal, n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM item_schedules WHERE id=? AND deadline_days=2`, f.scheds[1].ID); n != 1 {
		t.Fatal("the deadline was removed")
	}
}

func TestAReSaveOfAnEndedLinkedSchedulesTimingIsNotATimingEdit(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.linkRaw(t, 0, 1, "target", "target", oneDay)
	f.exec(t, `UPDATE item_schedules SET ended_at=?, next_at=NULL, next_wall=NULL WHERE id=?`, nowNano(), f.scheds[1].ID)
	same := f.scheds[1].Dtstart
	if _, err := f.s.UpdateSchedule(f.ws.ID, f.items[1].ID, models.ScheduleUpdate{Dtstart: &same, LeadMinutes: sp(30)}); err != nil {
		t.Fatalf("an unchanged dtstart with a new lead: %v", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM item_schedules WHERE id=? AND lead_minutes=30`, f.scheds[1].ID); n != 1 {
		t.Fatal("the lead was not saved")
	}
	if _, err := f.s.UpdateSchedule(f.ws.ID, f.items[1].ID, models.ScheduleUpdate{Dtstart: sp("2030-03-20T09:00")}); !errors.Is(err, ErrScheduleLinked) {
		t.Fatalf("a real change on an ended linked schedule: %v", err)
	}
}
