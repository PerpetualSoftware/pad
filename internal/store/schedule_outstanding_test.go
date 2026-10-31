package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4103 C2a: the fire overwrite of next_at, the new managed dedupe key, and
// the missed count.

// A floating schedule that fired directly (its early bound already behind it, so
// no deferral) keeps remind_at at the bound while next_at becomes the exact
// instant. Neither a lead_minutes retime nor a zone or assignee hook may read
// that exact instant as a bound and move the fired row. RED against a mutant
// that drops the `fired_at IS NULL` guard from retimeArmedReminderTx (a) or
// from resetFloatingBound's selection and UPDATE (b): the row's remind_at moves
// from the bound (2026-11-01T19:00Z) to the exact instant minus the lead.
func TestFiredFloatingRowIsNotRetimedByLeadOrHooks(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	la := f.user(t, "c2a-la@example.com", "America/Los_Angeles")
	f.assign(t, f.item.ID, la.ID)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Floating: true, LeadMinutes: 30})
	if got := f.fire(t, "2026-11-02T20:00:00Z"); len(got) != 1 {
		t.Fatalf("setup: fired %v", got)
	}
	fired := f.reminder(t, *sc.ReminderID)
	if fired.FiredAt == nil || fired.RemindAt != "2026-11-01T18:30:00Z" {
		t.Fatalf("setup: fired row = %+v, want remind_at at the bound minus the lead", fired)
	}
	if cur := f.reload(t, f.item.ID); cur.NextAt == nil || *cur.NextAt != "2026-11-02T17:00:00Z" {
		t.Fatalf("the fire did not overwrite next_at with the exact instant: %v", cur.NextAt)
	}
	unchanged := func(step string) {
		t.Helper()
		r := f.reminder(t, *sc.ReminderID)
		if r.RemindAt != fired.RemindAt || r.FiredAt == nil || *r.FiredAt != *fired.FiredAt {
			t.Errorf("%s moved the fired row: %+v (was %+v)", step, r, fired)
		}
		if r.OccurrenceAt == nil || *r.OccurrenceAt != "2026-11-02T17:00:00Z" {
			t.Errorf("%s changed the reading: %v", step, r.OccurrenceAt)
		}
	}

	lead := 90
	f.update(t, models.ScheduleUpdate{LeadMinutes: &lead})
	unchanged("a lead_minutes retime")

	tz := "Pacific/Kiritimati"
	if _, err := f.s.UpdateUser(la.ID, models.UserUpdate{Timezone: &tz}); err != nil {
		t.Fatal(err)
	}
	unchanged("the zone hook")
	other := f.user(t, "c2a-other@example.com", "Asia/Tokyo")
	f.assign(t, f.item.ID, other.ID)
	f.s.resetFloatingForItem(f.item.ID)
	unchanged("the assignee hook")
}

// An ARMED floating row carrying next_override_at (a C2b snooze or move; set by
// SQL here because C2a has no writer): the exact instant in next_at is never a
// bound, the override is. A lead retime and both hooks give override - lead.
func TestOverriddenArmedFloatingRowRetimesFromTheOverride(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	la := f.user(t, "c2a-ov@example.com", "America/Los_Angeles")
	f.assign(t, f.item.ID, la.ID)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Floating: true, LeadMinutes: 30})
	f.exec(t, `UPDATE item_schedules SET next_at = ?, next_override_at = ? WHERE id = ?`,
		"2026-11-02T17:00:00Z", "2026-11-05T12:00:00Z", sc.ID)
	f.exec(t, `UPDATE item_reminders SET remind_at = ? WHERE id = ?`, "2026-11-05T11:30:00Z", *sc.ReminderID)

	lead := 60
	f.update(t, models.ScheduleUpdate{LeadMinutes: &lead})
	if r := f.reminder(t, *sc.ReminderID); r.RemindAt != "2026-11-05T11:00:00Z" {
		t.Errorf("lead retime: remind_at = %s, want override - lead = 2026-11-05T11:00:00Z", r.RemindAt)
	}

	// Move the row off the value so the hooks have something to reset.
	f.exec(t, `UPDATE item_reminders SET remind_at = ? WHERE id = ?`, "2026-11-05T09:00:00Z", *sc.ReminderID)
	tz := "Pacific/Kiritimati"
	if _, err := f.s.UpdateUser(la.ID, models.UserUpdate{Timezone: &tz}); err != nil {
		t.Fatal(err)
	}
	if r := f.reminder(t, *sc.ReminderID); r.RemindAt != "2026-11-05T11:00:00Z" {
		t.Errorf("zone hook: remind_at = %s, want override - lead", r.RemindAt)
	}
	f.exec(t, `UPDATE item_reminders SET remind_at = ? WHERE id = ?`, "2026-11-05T09:00:00Z", *sc.ReminderID)
	f.s.resetFloatingForItem(f.item.ID)
	if r := f.reminder(t, *sc.ReminderID); r.RemindAt != "2026-11-05T11:00:00Z" {
		t.Errorf("assignee hook: remind_at = %s, want override - lead", r.RemindAt)
	}
}

// A snooze-style re-fire of the SAME occurrence (the row re-armed and fired
// again) is a second notification. RED with the C1 key reminder:<id>:<occurrence>,
// which collapses the second push into the first.
func TestManagedRefireOfTheSameOccurrenceIsANewNotification(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	u := f.user(t, "c2a-key@example.com", "")
	addDevice(t, f.s, u.ID)
	f.assign(t, f.item.ID, u.ID)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})
	f.force(t, sc, "2026-10-28T09:00:00", "2026-10-28T09:00:00Z", "2026-10-28T09:00:00Z")
	f.fire(t, "2026-10-29T12:00:00Z")
	f.exec(t, `UPDATE item_reminders SET fired_at = NULL, acked_at = NULL, remind_at = ? WHERE id = ?`,
		"2026-10-29T13:00:00Z", *sc.ReminderID)
	f.fire(t, "2026-10-29T13:00:00Z")
	if n := f.count(t, `SELECT COUNT(*) FROM user_notifications WHERE user_id = ? AND kind = 'reminder'`, u.ID); n != 2 {
		t.Errorf("notifications = %d, want 2 (one per arming of the same occurrence)", n)
	}
	if n := f.events(t); n != 2 {
		t.Errorf("events = %d, want 2", n)
	}
}

func hourlyFixture(t *testing.T, f *schedFixture, o time.Time) *models.ItemSchedule {
	t.Helper()
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=HOURLY"})
	f.force(t, sc, o.Format("2006-01-02T15:04:05"), o.Format(time.RFC3339), o.Format(time.RFC3339))
	return f.reload(t, f.item.ID)
}

// The missed count excludes the outstanding occurrence and counts the instances
// strictly after it up to now-1s (an instance exactly at now-1s counts, one at
// now does not). RED if O itself is counted, if the cut is now instead of
// now-1s, or if the count is read from schedule_occurrences.
func TestScheduleMissedCountsPassedInstancesAfterTheOutstandingOne(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	o := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})
	f.force(t, sc, "2026-10-28T09:00:00", o.Format(time.RFC3339), o.Format(time.RFC3339))
	f.fire(t, "2026-10-28T12:00:00Z")
	if got := f.fire(t, "2026-10-31T12:00:00Z"); len(got) != 0 {
		t.Fatalf("a fired row fired again: %v", got)
	}
	sc = f.reload(t, f.item.ID)
	if !sc.ReminderFired {
		t.Fatal("ReminderFired not read")
	}
	at := func(s string) time.Time {
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	cases := []struct {
		now   string
		count int
	}{
		{"2026-10-28T12:00:00Z", 0}, // O alone is not missed
		{"2026-10-28T08:00:00Z", 0}, // before O
		{"2026-10-29T09:00:00Z", 0}, // next instance is at now: not yet
		{"2026-10-29T09:00:01Z", 1}, // at now-1s: counts
		{"2026-10-30T10:00:00Z", 2},
		{"2026-10-31T12:00:00Z", 3},
	}
	for _, c := range cases {
		n, trunc, err := ScheduleMissed(sc, nil, at(c.now))
		if err != nil || n != c.count || trunc {
			t.Errorf("now %s: count %d truncated %v err %v, want %d", c.now, n, trunc, err, c.count)
		}
	}
	n, trunc, err := ScheduleMissed(sc, nil, at("2027-12-01T00:00:00Z"))
	if err != nil || !trunc || n != 92 {
		t.Errorf("400 days on: count %d truncated %v err %v, want 92 truncated", n, trunc, err)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM schedule_occurrences`); got != 0 {
		t.Errorf("the count wrote %d occurrence rows", got)
	}
	// One tick through all that: still the one fire.
	if n := f.events(t); n != 1 {
		t.Errorf("events = %d, want 1", n)
	}
}

// More than the instance cap passed: a floor of 500, flagged truncated, no error.
func TestScheduleMissedInstanceCapIsATruncatedFloor(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	o := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	sc := hourlyFixture(t, f, o)
	n, trunc, err := ScheduleMissed(sc, nil, o.Add(700*time.Hour))
	if err != nil || n != 500 || !trunc {
		t.Errorf("count %d truncated %v err %v, want 500 truncated", n, trunc, err)
	}
}

// The walk starts at anchor_wall, never at dtstart (TASK-4102 PR1b r1, H-q2): a
// rule that is only affordable near its re-anchor still counts. From the 2000
// dtstart this is CapScan. RED if ScheduleMissed compiles from dtstart.
func TestScheduleMissedNeverWalksBeforeTheAnchor(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc, err := f.s.createScheduleAt(f.ws.ID, f.item.ID, models.ScheduleCreate{
		Kind: "check", Dtstart: "2000-01-01T09:00", Tz: "UTC", Rrule: "FREQ=MINUTELY;BYHOUR=9",
	}, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	o := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	f.force(t, sc, "2026-10-28T09:00:00", o.Format(time.RFC3339), o.Format(time.RFC3339))
	f.exec(t, `UPDATE item_schedules SET anchor_wall = ? WHERE id = ?`, "2026-10-28T09:00:00", sc.ID)
	sc = f.reload(t, f.item.ID)
	n, _, err := ScheduleMissed(sc, nil, o.Add(10*time.Minute+time.Second))
	if err != nil || n != 10 {
		t.Errorf("count %d err %v, want 10 from the anchor", n, err)
	}
}

// A healthy future schedule, an ended one and a broken one cost nothing and
// report nothing.
func TestScheduleMissedIsZeroWhenNothingPassed(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})
	if n, trunc, err := ScheduleMissed(sc, nil, schedNow); err != nil || n != 0 || trunc {
		t.Errorf("future: %d %v %v", n, trunc, err)
	}
	e := "syntax"
	sc.ErrorCode = &e
	if n, _, err := ScheduleMissed(sc, nil, schedNow.Add(1000*time.Hour)); err != nil || n != 0 {
		t.Errorf("broken: %d %v", n, err)
	}
}

// firedFloating makes a fired floating schedule for a recipient in zone, whose
// outstanding occurrence is the wall 2026-11-02T09:00.
func firedFloating(t *testing.T, f *schedFixture, zone, rrule, fireAt string) *models.ItemSchedule {
	t.Helper()
	u := f.user(t, "c2a-missed-"+zone[:3]+"@example.com", zone)
	f.assign(t, f.item.ID, u.ID)
	f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Floating: true, Rrule: rrule})
	if got := f.fire(t, fireAt); len(got) != 1 {
		t.Fatalf("setup: fired %v", got)
	}
	sc := f.reload(t, f.item.ID)
	if !sc.ReminderFired {
		t.Fatal("setup: not fired")
	}
	return sc
}

func wallIn(t *testing.T, zone string, y int, m time.Month, d, h, min, s int) (time.Time, *time.Location) {
	t.Helper()
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	return time.Date(y, m, d, h, min, s, 0, loc).UTC(), loc
}

// A floating row is walked by WALL in the reader's zone: O is next_wall resolved
// in the reader's zone, not the recipient's stored instant. RED (TASK-4103 C2a r1
// H-M1) when O is the stored instant: the walk's floor (the reader's dtstart) then
// lies after O and the dtstart instance, O's own wall, is counted.
func TestScheduleMissedFloatingTokyoFiredLARead(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := firedFloating(t, f, "Asia/Tokyo", "FREQ=DAILY", "2026-11-02T01:00:00Z")
	if sc.NextAt == nil || *sc.NextAt != "2026-11-02T00:00:00Z" {
		t.Fatalf("setup: next_at = %v, want Tokyo's 09:00", sc.NextAt)
	}
	laNine, la := wallIn(t, "America/Los_Angeles", 2026, 11, 2, 9, 0, 0)

	// Just after LA's 09:00 of the same date: O's own wall, not missed.
	n, trunc, err := ScheduleMissed(sc, la, laNine.Add(time.Second))
	if err != nil || n != 0 || trunc {
		t.Errorf("same date: count %d truncated %v err %v, want 0", n, trunc, err)
	}
	// One later instance (the next LA 09:00) has really passed: 1, so not stale.
	next, _ := wallIn(t, "America/Los_Angeles", 2026, 11, 3, 9, 0, 0)
	n, trunc, err = ScheduleMissed(sc, la, next.Add(time.Second))
	if err != nil || n != 1 || trunc {
		t.Errorf("one later: count %d truncated %v err %v, want 1 (stale is count >= 2)", n, trunc, err)
	}
}

// The reverse: the recipient is west of the reader. Walls after O's wall that
// resolve BEFORE O's stored instant in the reader's zone are counted, not
// dropped. RED when the walk starts at the stored instant.
func TestScheduleMissedFloatingWestRecipientEastReader(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := firedFloating(t, f, "America/Los_Angeles", "FREQ=HOURLY", "2026-11-02T18:00:00Z")
	if sc.NextAt == nil || *sc.NextAt != "2026-11-02T17:00:00Z" {
		t.Fatalf("setup: next_at = %v, want LA's 09:00", sc.NextAt)
	}
	// Tokyo's 10:00 of the same date, a wall one hour after O's wall.
	tenJST, tokyo := wallIn(t, "Asia/Tokyo", 2026, 11, 2, 10, 0, 0)
	n, _, err := ScheduleMissed(sc, tokyo, tenJST.Add(time.Second))
	if err != nil || n != 1 {
		t.Errorf("one passed: count %d err %v, want 1", n, err)
	}
	elevenJST, _ := wallIn(t, "Asia/Tokyo", 2026, 11, 2, 11, 0, 0)
	n, _, err = ScheduleMissed(sc, tokyo, elevenJST.Add(time.Second))
	if err != nil || n != 2 {
		t.Errorf("two passed: count %d err %v, want 2", n, err)
	}
}
