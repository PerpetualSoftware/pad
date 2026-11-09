package store

import (
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// A cascade that lands a child's target in the past (TASK-4353): the occurrence
// stays outstanding at its new target, arms nothing and fires nothing, and the
// child's deadline is neither moved nor lost.

var dayBefore = models.ScheduleLinkOffset{Mode: "elapsed", ElapsedSecs: -86400}

// atClock sets the linked-change clock for the rest of the test.
func (f *linkFix) atClock(t *testing.T, at string) {
	t.Helper()
	now, err := time.Parse(time.RFC3339, at)
	if err != nil {
		t.Fatal(err)
	}
	restore := f.s.SetLinkClockForTesting(func() time.Time { return now })
	t.Cleanup(restore)
}

func (f *linkFix) reminderRows(t *testing.T, i int) int {
	t.Helper()
	return f.count(t, `SELECT COUNT(*) FROM item_reminders WHERE schedule_id=?`, f.scheds[i].ID)
}

// Item 1 is the parent (03-05) and item 0 its child (03-04): a day before it.
func TestPastCascadeCancelsAnArmedReminderAndFiresNothing(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.link(t, 1, 0, "target", "target", dayBefore)
	if f.reminderRows(t, 0) != 1 {
		t.Fatal("the child has no armed reminder to begin with")
	}
	before := f.reload(t, f.items[0].ID)
	f.atClock(t, "2030-03-20T12:00:00Z")
	// The parent moves ahead of the clock; the child lands a day before it, behind the clock.
	if _, err := f.move(1, "target", "2030-03-21T09:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	if got := f.role(t, 0, "target"); got != "2030-03-20T09:00:00Z" {
		t.Fatalf("child target=%s, want the parent less a day", got)
	}
	if n := f.reminderRows(t, 0); n != 0 {
		t.Fatalf("the child still has %d reminder rows armed for its old target", n)
	}
	for _, r := range f.fire(t, "2030-03-20T10:00:00Z") {
		if r.ScheduleID != nil && *r.ScheduleID == f.scheds[0].ID {
			t.Fatalf("the child's reminder fired about a date that moved: %+v", r)
		}
	}
	after := f.reload(t, f.items[0].ID)
	if *after.NextAt != *before.NextAt || *after.NextWall != *before.NextWall || after.EndedAt != nil {
		t.Fatalf("the occurrence advanced: before=%s/%s after=%s/%s", *before.NextAt, *before.NextWall, *after.NextAt, *after.NextWall)
	}
	if after.NextOverrideAt == nil || *after.NextOverrideAt != "2030-03-20T09:00:00Z" {
		t.Fatalf("next_override_at=%v, want the new target", after.NextOverrideAt)
	}
	// A later move forward arms exactly one.
	if _, err := f.move(1, "target", "2030-03-25T09:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	if n := f.reminderRows(t, 0); n != 1 {
		t.Fatalf("reminder rows after a forward cascade=%d, want exactly 1", n)
	}
	if r := f.managed(t, f.reload(t, f.items[0].ID)); r.RemindAt != "2030-03-24T09:00:00Z" {
		t.Fatalf("reminder=%s, want the new target", r.RemindAt)
	}
}

// pairWithChild builds a parent (one-off, 03-05 09:00 UTC) and a child created by
// mk, linked a day before the parent, and returns the fixture with the child at 0.
func pairWithChild(t *testing.T, mk func(f *schedFixture, it *models.Item) *models.ItemSchedule) *linkFix {
	t.Helper()
	sf := newSchedFixture(t)
	parent := sf.item
	child := sf.another(t, "child")
	lf := &linkFix{schedFixture: sf, items: []*models.Item{child, parent}}
	lf.scheds = []*models.ItemSchedule{mk(sf, child), sf.native(t, parent, "2030-03-05T09:00", models.CommitmentRolesInput{})}
	lf.link(t, 1, 0, "target", "target", dayBefore)
	return lf
}

// A recurring child with a deadline: the cascade moves its target behind the
// clock, the occurrence stays, and the tick records one miss at the deadline on
// the same key, once.
func TestPastCascadeKeepsTheChildDeadlineAndItsOccurrenceKey(t *testing.T) {
	t.Parallel()
	f := pairWithChild(t, func(f *schedFixture, it *models.Item) *models.ItemSchedule {
		sc, err := f.s.CreateCommitmentSchedule(f.ws.ID, it.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2030-03-04T09:00", Tz: "UTC", Rrule: "FREQ=WEEKLY"}, models.CommitmentRolesInput{})
		if err != nil {
			t.Fatal(err)
		}
		f.exec(t, `UPDATE item_schedules SET roles_origin='native', deadline_days=20, deadline_secs=0, deadline_behavior='stays_owed' WHERE id=?`, sc.ID)
		return f.reload(t, it.ID)
	})
	key, err := outstandingKey(f.reload(t, f.items[0].ID), time.UTC)
	if err != nil || key == "" {
		t.Fatalf("key=%q %v", key, err)
	}
	f.atClock(t, "2030-03-20T12:00:00Z")
	if res, err := f.move(1, "target", "2030-03-21T09:00:00Z", nil); err != nil || res.RenegotiatesDeadline {
		t.Fatalf("a past cascade onto a child with a deadline is not refused: %+v %v", res, err)
	}
	after := f.reload(t, f.items[0].ID)
	if k, _ := outstandingKey(after, time.UTC); k != key {
		t.Fatalf("occurrence key %q became %q", key, k)
	}
	if got := f.role(t, 0, "deadline"); got != "2030-03-24T09:00:00Z" {
		t.Fatalf("child deadline=%s, want it untouched", got)
	}
	if got := f.eval(t, "2030-03-24T08:59:59Z", 0); got.Missed != 0 {
		t.Fatalf("before the deadline: %+v", got)
	}
	if got := f.eval(t, "2030-03-24T09:00:01Z", 0); got.Missed != 1 {
		t.Fatalf("after the deadline: %+v, want exactly one miss", got)
	}
	if got := f.eval(t, "2030-03-25T00:00:00Z", 0); got.Missed != 0 {
		t.Fatalf("a later tick: %+v, want no second miss", got)
	}
	ev := f.missEvidence(t)
	if len(ev) != 1 || !strings.HasPrefix(ev[0], key+" ") {
		t.Fatalf("evidence=%v, want one row on key %q", ev, key)
	}
}

// A floating child, armed and then fired, with a deadline override: a past
// cascade and a later change of the recipient zone leave the occurrence's
// instant, its key and the override untouched.
func TestPastCascadeOnAFloatingChildKeepsItsInstantAcrossAZoneChange(t *testing.T) {
	t.Parallel()
	for _, fired := range []bool{false, true} {
		name := map[bool]string{false: "armed", true: "fired"}[fired]
		t.Run(name, func(t *testing.T) {
			var u *models.User
			f := pairWithChild(t, func(f *schedFixture, it *models.Item) *models.ItemSchedule {
				u = f.user(t, "floating-child@example.com", "America/Los_Angeles")
				f.assign(t, it.ID, u.ID)
				sc, err := f.s.CreateCommitmentSchedule(f.ws.ID, it.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2030-03-04T09:00", Floating: true}, models.CommitmentRolesInput{})
				if err != nil {
					t.Fatal(err)
				}
				f.exec(t, `UPDATE item_schedules SET roles_origin='native', deadline_days=20, deadline_secs=0, deadline_behavior='stays_owed' WHERE id=?`, sc.ID)
				return f.reload(t, it.ID)
			})
			la, _ := time.LoadLocation("America/Los_Angeles")
			tokyo, _ := time.LoadLocation("Asia/Tokyo")
			sc := f.reload(t, f.items[0].ID)
			if fired {
				f.fire(t, "2030-03-04T18:00:00Z")
				sc = f.reload(t, f.items[0].ID)
				if !sc.ReminderFired {
					t.Fatal("the child's reminder did not fire")
				}
			}
			instant, _, _ := OutstandingInstant(sc, la)
			key, _ := outstandingKey(sc, la)
			// The recipient's own deadline for this occurrence, renegotiated to a fixed instant.
			f.deadlineOverride(t, sc, "2030-03-26T00:00:00Z")
			f.atClock(t, "2030-03-20T12:00:00Z")
			if _, err := f.move(1, "target", "2030-03-21T09:00:00Z", nil); err != nil {
				t.Fatal(err)
			}
			f.exec(t, `UPDATE users SET timezone=? WHERE id=?`, "Asia/Tokyo", u.ID)
			after := f.reload(t, f.items[0].ID)
			if fired && (!after.ReminderFired || f.count(t, `SELECT COUNT(*) FROM item_reminders WHERE schedule_id=? AND fired_at IS NOT NULL`, after.ID) != 1) {
				t.Fatal("a reminder that already fired was deleted by the cascade")
			}
			for _, view := range []*time.Location{la, tokyo} {
				if got, _, _ := OutstandingInstant(after, view); !got.Equal(instant) {
					t.Fatalf("outstanding instant in %s: %s, was %s", view, got, instant)
				}
				if k, _ := outstandingKey(after, view); k != key {
					t.Fatalf("occurrence key %q became %q", key, k)
				}
			}
			if got := f.eval(t, "2030-03-25T23:59:59Z", 0); got.Missed != 0 {
				t.Fatalf("before the override: %+v", got)
			}
			if got := f.eval(t, "2030-03-26T00:00:01Z", 0); got.Missed != 1 {
				t.Fatalf("after the override: %+v, want one miss", got)
			}
		})
	}
}
