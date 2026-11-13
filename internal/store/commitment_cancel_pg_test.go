package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Postgres lock-order tests for the cancel policy (TASK-4353, 2c). The mark is
// written inside the parent's skip, under the parent's single-item pin (item,
// workspace, schedule). It takes no lock on the child item or schedule row, so
// it queues on the workspace with a cascade and with a resolve of the child it
// marks, and nothing crosses. The harness is commitment_links_pg_test.go's.

func (f *linkPG) cancelLink(t *testing.T, parent, child int) {
	t.Helper()
	if _, err := f.s.CreateScheduleLink(f.ws.ID, models.ScheduleLinkCreate{ParentItemID: f.items[parent].ID, ParentRole: "target", ChildItemID: f.items[child].ID, ChildRole: "target", Offset: oneDay, CancelPolicy: models.LinkCancelChildOccurrence}, f.access()); err != nil {
		t.Fatal(err)
	}
}

func (f *linkPG) skip(i int) error {
	_, err := f.s.ResolveOccurrence(f.ws.ID, f.items[i].ID, ResolveInput{OccurrenceAt: rt(*f.scs[i].NextAt), State: "skip", Actor: "user", MaySkipBeforeDeadline: true, EndSchedule: true})
	return err
}

// The parent's skip marks its child while a resolve of that child waits for the
// workspace with the child item in hand: the mark never asks for that item row.
func TestParentSkipMarkingAChildAgainstAResolveOfThatChildDoesNotDeadlock(t *testing.T) {
	f := newLinkPG(t, 2)
	f.cancelLink(t, 0, 1)
	errs := f.run(t,
		func() error { return f.skip(0) },
		func() error { return f.done(1) })
	for _, err := range errs {
		if err != nil {
			t.Fatalf("errs=%v", errs)
		}
	}
	if deadlocked(errs) != 0 {
		t.Fatal("deadlock")
	}
}

// The parent's skip against a cascade rooted at the child it marks.
func TestParentSkipMarkingAChildAgainstACascadeDoesNotDeadlock(t *testing.T) {
	f := newLinkPG(t, 3)
	f.cancelLink(t, 0, 1)
	f.link(t, 1, 2)
	errs := f.run(t,
		func() error { return f.skip(0) },
		func() error { return f.move(1, "2030-03-20T09:00:00Z") })
	for _, err := range errs {
		if err != nil {
			t.Fatalf("errs=%v", errs)
		}
	}
}

// Two parents' skips marking one child occurrence commit in turn, and the
// occurrence is marked once.
func TestTwoParentSkipsMarkingOneChildDoNotDeadlock(t *testing.T) {
	f := newLinkPG(t, 3)
	f.exec(`UPDATE item_schedules SET roles_origin='native', earliest_days=-1, earliest_secs=0 WHERE id=$1`, f.scs[2].ID)
	f.cancelLink(t, 0, 2)
	if _, err := f.s.CreateScheduleLink(f.ws.ID, models.ScheduleLinkCreate{ParentItemID: f.items[1].ID, ParentRole: "target", ChildItemID: f.items[2].ID, ChildRole: "earliest", Offset: oneDay, CancelPolicy: models.LinkCancelChildOccurrence}, f.access()); err != nil {
		t.Fatal(err)
	}
	errs := f.run(t,
		func() error { return f.skip(0) },
		func() error { return f.skip(1) })
	for _, err := range errs {
		if err != nil {
			t.Fatalf("errs=%v", errs)
		}
	}
	var n int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=$1 AND change_type='update' AND reason LIKE 'No longer needed%'`, f.scs[2].ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("journal rows=%d %v, want the occurrence marked and journalled once", n, err)
	}
}

func (f *linkPG) exec(q string, args ...any) {
	if _, err := f.s.db.Exec(q, args...); err != nil {
		panic(err)
	}
}

// The mark also disarms the child's managed reminder and may record the child's
// miss. A tick that fires the same reminders queues on the same workspace, and
// finds the row gone or already fired, never a deadlock.
func TestParentSkipDisarmingAChildAgainstATickDoesNotDeadlock(t *testing.T) {
	f := newLinkPG(t, 2)
	f.cancelLink(t, 0, 1)
	errs := f.run(t,
		func() error { return f.skip(0) },
		func() error {
			_, err := f.s.FireDueReminders("2031-01-01T00:00:00Z", 0)
			return err
		})
	for _, err := range errs {
		if err != nil {
			t.Fatalf("errs=%v", errs)
		}
	}
}
