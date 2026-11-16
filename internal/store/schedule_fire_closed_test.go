package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4585: a closed item's recurring schedule stays live (the force close in
// TASK-4538 keeps next_at), and the occurrence must not fire while it is closed.
func TestScheduleClosedItemDoesNotFire(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})
	f.force(t, sc, "2026-10-26T09:00:00", "2026-10-26T09:00:00Z", "2026-10-26T09:00:00Z")
	var reminderID string
	if err := f.s.db.QueryRow(f.s.q(`SELECT id FROM item_reminders WHERE schedule_id = ?`), sc.ID).Scan(&reminderID); err != nil {
		t.Fatal(err)
	}
	u := f.user(t, "closed4585@example.com", "")
	addDevice(t, f.s, u.ID)
	f.assign(t, f.item.ID, u.ID)
	if _, err := f.s.UpdateItem(f.item.ID, models.ItemUpdate{Fields: ptr(`{"status":"done"}`), Force: true}); err != nil {
		t.Fatal(err)
	}
	if got := f.fire(t, "2026-10-29T12:00:00Z"); len(got) != 0 {
		t.Errorf("a closed item fired %d reminder(s), first occurrence_at %v", len(got), deref(got[0].OccurrenceAt))
	}
	if r := f.reminder(t, reminderID); r.FiredAt != nil || r.RemindAt != "2026-10-26T09:00:00Z" {
		t.Errorf("closed item reminder did not stay armed at its original instant: %+v", r)
	}
	if n := f.events(t); n != 0 {
		t.Fatalf("closed item wrote %d reminder events", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM user_notifications WHERE kind = 'reminder'`); n != 0 {
		t.Fatalf("closed item wrote %d user notifications", n)
	}
}

func TestScheduleReopenedItemFiresCaughtUpOccurrence(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})
	f.force(t, sc, "2026-10-26T09:00:00", "2026-10-26T09:00:00Z", "2026-10-26T09:00:00Z")
	if _, err := f.s.UpdateItem(f.item.ID, models.ItemUpdate{Fields: ptr(`{"status":"done"}`), Force: true}); err != nil {
		t.Fatal(err)
	}
	if got := f.fire(t, "2026-10-29T12:00:00Z"); len(got) != 0 {
		t.Errorf("a closed item fired %d reminder(s)", len(got))
	}
	if _, err := f.s.UpdateItem(f.item.ID, models.ItemUpdate{Fields: ptr(`{"status":"open"}`)}); err != nil {
		t.Fatal(err)
	}
	got := f.fire(t, "2026-10-29T12:00:00Z")
	if len(got) != 1 || got[0].OccurrenceAt == nil || *got[0].OccurrenceAt != "2026-10-26T09:00:00Z" {
		t.Fatalf("after reopen fired %+v, want the outstanding occurrence", got)
	}
	if again := f.fire(t, "2026-10-29T12:00:30Z"); len(again) != 0 {
		t.Errorf("reopened item fired twice: %+v", again)
	}
	if n := f.events(t); n != 1 {
		t.Errorf("outbox events = %d, want 1", n)
	}
}

func TestScheduleClosedItemTakesNoBatchSlot(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	other := f.another(t, "Open item")
	a := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})
	b, err := f.s.createScheduleAt(f.ws.ID, other.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}, schedNow)
	if err != nil {
		t.Fatal(err)
	}
	f.force(t, a, "2026-10-25T09:00:00", "2026-10-25T09:00:00Z", "2026-10-25T09:00:00Z")
	f.force(t, b, "2026-10-26T09:00:00", "2026-10-26T09:00:00Z", "2026-10-26T09:00:00Z")
	if _, err := f.s.UpdateItem(f.item.ID, models.ItemUpdate{Fields: ptr(`{"status":"done"}`), Force: true}); err != nil {
		t.Fatal(err)
	}
	got, err := f.s.FireDueReminders("2026-10-29T12:00:00Z", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ItemID != other.ID {
		t.Fatalf("batch fired %+v, want the open item %s", got, other.ID)
	}
}

func TestInstantReminderPausesAfterClose(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	id := armReminder(t, f.s, f.ws.ID, f.item.ID, "2026-10-26T09:00:00Z")
	if _, err := f.s.UpdateItem(f.item.ID, models.ItemUpdate{Fields: ptr(`{"status":"done"}`), Force: true}); err != nil {
		t.Fatal(err)
	}
	if got := f.fire(t, "2026-10-29T12:00:00Z"); len(got) != 0 {
		t.Fatalf("closed commitment fired: %+v", got)
	}
	if r := f.reminder(t, id); !r.Armed() || r.ScheduleID == nil {
		t.Fatalf("paused state: %+v", r)
	}
}

// The scan can hand over a candidate just before the item closes. The arbiter
// must recheck terminality in its fire UPDATE, independently of the scan.
func TestScheduleClosedItemArbiterDoesNotFire(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})
	f.force(t, sc, "2026-10-26T09:00:00", "2026-10-26T09:00:00Z", "2026-10-26T09:00:00Z")
	ids, err := f.s.dueReminderCandidates("2026-10-29T12:00:00Z", 1)
	if err != nil || len(ids) != 1 {
		t.Fatalf("candidate scan = %v, %v", ids, err)
	}
	if _, err := f.s.UpdateItem(f.item.ID, models.ItemUpdate{Fields: ptr(`{"status":"done"}`), Force: true}); err != nil {
		t.Fatal(err)
	}
	got, err := f.s.fireOneReminder(ids[0], "2026-10-29T12:00:00Z")
	if err != nil || got != nil {
		t.Errorf("closed item arbiter = %+v, %v; want nil, nil", got, err)
	}
	if r := f.reminder(t, ids[0]); r.FiredAt != nil || r.RemindAt != "2026-10-26T09:00:00Z" {
		t.Errorf("closed item reminder did not stay armed at its original instant: %+v", r)
	}
	if n := f.events(t); n != 0 {
		t.Errorf("closed item wrote %d reminder events", n)
	}
}
