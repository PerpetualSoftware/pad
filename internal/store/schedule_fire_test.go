package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/kernelevents"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// TASK-4102 PR1b: firing a schedule's managed reminder row. Runs on SQLite in
// make test and on Postgres in make test-pg.

// force rewrites the schedule's outstanding occurrence and its managed row, so a
// test can put an occurrence in the past without waiting for it.
func (f *schedFixture) force(t *testing.T, sc *models.ItemSchedule, wall string, at, remindAt string) {
	t.Helper()
	f.exec(t, `UPDATE item_schedules SET next_at = ?, next_wall = ? WHERE id = ?`, at, wall, sc.ID)
	f.exec(t, `UPDATE item_reminders SET remind_at = ? WHERE schedule_id = ?`, remindAt, sc.ID)
}

func (f *schedFixture) events(t *testing.T) int {
	t.Helper()
	return f.count(t, `SELECT COUNT(*) FROM event_outbox WHERE event_type = ?`, kernelevents.ItemReminderDue)
}

func (f *schedFixture) reload(t *testing.T, itemID string) *models.ItemSchedule {
	t.Helper()
	sc, err := f.s.GetSchedule(f.ws.ID, itemID)
	if err != nil || sc == nil {
		t.Fatalf("GetSchedule: %v %v", sc, err)
	}
	return sc
}

func (f *schedFixture) reminder(t *testing.T, id string) *models.Reminder {
	t.Helper()
	r, err := f.s.GetReminder(f.ws.ID, id)
	if err != nil || r == nil {
		t.Fatalf("GetReminder(%s): %v %v", id, r, err)
	}
	return r
}

func (f *schedFixture) fire(t *testing.T, nowTS string) []*models.Reminder {
	t.Helper()
	fired, err := f.s.FireDueReminders(nowTS, 0)
	if err != nil {
		t.Fatalf("FireDueReminders(%s): %v", nowTS, err)
	}
	return fired
}

// user makes a workspace member with a time zone, "" for none.
func (f *schedFixture) user(t *testing.T, email, zone string) *models.User {
	t.Helper()
	u := createTestUser(t, f.s, email, email, "password123")
	if err := f.s.AddWorkspaceMember(f.ws.ID, u.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	if zone != "" {
		if _, err := f.s.SetUserTimezoneIfUnset(u.ID, zone); err != nil {
			t.Fatal(err)
		}
	}
	return u
}

func (f *schedFixture) assign(t *testing.T, itemID, userID string) {
	t.Helper()
	f.exec(t, `UPDATE items SET assigned_user_id = ? WHERE id = ?`, userID, itemID)
}

// Catch-up after downtime is one fire per schedule, oldest first, for the stale
// occurrence rather than for now.
func TestTickCatchUpAfterDowntime(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	other := f.another(t, "Second")
	a := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})
	b, err := f.s.createScheduleAt(f.ws.ID, other.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}, schedNow)
	if err != nil {
		t.Fatal(err)
	}
	f.force(t, a, "2026-10-26T09:00:00", "2026-10-26T09:00:00Z", "2026-10-26T09:00:00Z")
	f.force(t, b, "2026-10-27T09:00:00", "2026-10-27T09:00:00Z", "2026-10-27T09:00:00Z")

	fired := f.fire(t, "2026-10-29T12:00:00Z")
	if len(fired) != 2 {
		t.Fatalf("fired %d, want one per schedule", len(fired))
	}
	if *fired[0].ScheduleID != a.ID || *fired[1].ScheduleID != b.ID {
		t.Errorf("order = %s, %s; want the older schedule first", *fired[0].ScheduleID, *fired[1].ScheduleID)
	}
	if *fired[0].OccurrenceAt != "2026-10-26T09:00:00Z" || *fired[1].OccurrenceAt != "2026-10-27T09:00:00Z" {
		t.Errorf("occurrence_at = %s, %s; want the stale occurrences, not now", *fired[0].OccurrenceAt, *fired[1].OccurrenceAt)
	}
	if again := f.fire(t, "2026-10-29T12:00:30Z"); len(again) != 0 {
		t.Errorf("second tick fired %d", len(again))
	}
	if n := f.events(t); n != 2 {
		t.Errorf("outbox events = %d, want 2", n)
	}
}

// One fire writes one event whose payload names the schedule and the
// occurrence, and a managed row's notification is keyed on the occurrence.
func TestScheduleFireWritesExactlyOneOutboxEvent(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	u := f.user(t, "recipient4102@example.com", "")
	addDevice(t, f.s, u.ID)
	f.assign(t, f.item.ID, u.ID)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})
	f.force(t, sc, "2026-10-28T09:00:00", "2026-10-28T09:00:00Z", "2026-10-28T09:00:00Z")

	fired := f.fire(t, "2026-10-29T12:00:00Z")
	if len(fired) != 1 || fired[0].ID != *sc.ReminderID {
		t.Fatalf("fired = %+v", fired)
	}
	var subject, payload string
	if err := f.s.db.QueryRow(f.s.q(`SELECT subject_id, payload FROM event_outbox WHERE event_type = ?`), kernelevents.ItemReminderDue).Scan(&subject, &payload); err != nil {
		t.Fatal(err)
	}
	var p struct {
		Reminder struct {
			ID           string `json:"id"`
			ScheduleID   string `json:"schedule_id"`
			OccurrenceAt string `json:"occurrence_at"`
		} `json:"reminder"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		t.Fatal(err)
	}
	if subject != *sc.ReminderID || p.Reminder.ScheduleID != sc.ID || p.Reminder.OccurrenceAt != "2026-10-28T09:00:00Z" {
		t.Errorf("subject %s payload %+v", subject, p.Reminder)
	}
	if n := f.events(t); n != 1 {
		t.Fatalf("events = %d", n)
	}

	// The notification key is the occurrence, so a row re-armed for another
	// occurrence that fires within the same second is its own notification.
	key := func(k string) int {
		return f.count(t, `SELECT COUNT(*) FROM user_notifications WHERE user_id = ? AND dedupe_key = ?`, u.ID, k)
	}
	if key("reminder:"+*sc.ReminderID+":2026-10-28T09:00:00Z") != 1 {
		t.Fatalf("no notification keyed on the occurrence")
	}
	f.exec(t, `UPDATE item_reminders SET fired_at = NULL, remind_at = ? WHERE id = ?`, "2026-10-28T10:00:00Z", *sc.ReminderID)
	f.exec(t, `UPDATE item_schedules SET next_at = ?, next_wall = ? WHERE id = ?`, "2026-10-28T10:00:00Z", "2026-10-28T10:00:00", sc.ID)
	f.fire(t, "2026-10-29T12:00:00Z")
	if key("reminder:"+*sc.ReminderID+":2026-10-28T10:00:00Z") != 1 {
		t.Errorf("the second occurrence was collapsed into the first notification")
	}
	if n := f.count(t, `SELECT COUNT(*) FROM user_notifications WHERE user_id = ? AND kind = 'reminder'`, u.ID); n != 2 {
		t.Errorf("notifications = %d, want 2", n)
	}
}

// Passes racing for one managed row (two ticks on two instances): exactly one
// wins, one event, one notification.
func TestScheduleConcurrentFiresWriteOneEvent(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	u := f.user(t, "racer4102@example.com", "")
	addDevice(t, f.s, u.ID)
	f.assign(t, f.item.ID, u.ID)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})
	f.force(t, sc, "2026-10-28T09:00:00", "2026-10-28T09:00:00Z", "2026-10-28T09:00:00Z")

	const racers = 6
	var wg sync.WaitGroup
	start := make(chan struct{})
	var mu sync.Mutex
	won, failed := 0, []error{}
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			fired, err := f.s.FireDueReminders("2026-10-29T12:00:00Z", 0)
			mu.Lock()
			defer mu.Unlock()
			won += len(fired)
			if err != nil {
				failed = append(failed, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if len(failed) != 0 {
		t.Fatalf("a racing pass failed: %v", failed)
	}
	if won != 1 {
		t.Errorf("passes that fired the row = %d, want 1", won)
	}
	if n := f.events(t); n != 1 {
		t.Errorf("events = %d, want 1", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM user_notifications WHERE user_id = ? AND kind = 'reminder'`, u.ID); n != 1 {
		t.Errorf("notifications = %d, want 1", n)
	}
}

// A schedule PATCH racing the tick: no deadlock, no error, exactly one event.
// The patch changes the lead on a row whose occurrence is long past, so the row
// is due whichever of the two commits first; a deadlock between the writers'
// lock orders, or a lost fire, would show.
func TestScheduleConcurrentPatchVsFire(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	const rounds = 25
	lead := 5
	for i := 0; i < rounds; i++ {
		item := f.another(t, fmt.Sprintf("Race %d", i))
		sc, err := f.s.createScheduleAt(f.ws.ID, item.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}, schedNow)
		if err != nil {
			t.Fatal(err)
		}
		f.force(t, sc, "2026-10-20T09:00:00", "2026-10-20T09:00:00Z", "2026-10-20T09:00:00Z")
		before := f.events(t)

		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make(chan error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, err := f.s.FireDueReminders("2026-10-29T12:00:00Z", 0)
			errs <- err
		}()
		go func() {
			defer wg.Done()
			<-start
			l := lead + i
			_, err := f.s.updateScheduleAt(f.ws.ID, item.ID, models.ScheduleUpdate{LeadMinutes: &l}, schedNow)
			errs <- err
		}()
		close(start)
		finished := make(chan struct{})
		go func() { wg.Wait(); close(finished) }()
		select {
		case <-finished:
		case <-time.After(45 * time.Second):
			t.Fatalf("round %d: the writers did not finish; a lock-order deadlock", i)
		}
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: %v", i, err)
			}
		}
		if got := f.events(t) - before; got != 1 {
			t.Fatalf("round %d: %d events, want exactly 1", i, got)
		}
		if r := f.reminder(t, *sc.ReminderID); r.FiredAt == nil {
			t.Fatalf("round %d: the row is not fired", i)
		}
	}
}

// Postgres: the lock order is deterministic, not just lucky. A writer holding
// the schedule row (as UpdateSchedule does) and then writing the reminder row
// must not deadlock with a fire that arrives in between: the fire waits on the
// schedule in its pin, before it has touched the reminder.
func TestScheduleFireWaitsOnTheScheduleBeforeTheReminder(t *testing.T) {
	pgURL := os.Getenv("PAD_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set: lock order only exists on Postgres")
	}
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})
	f.force(t, sc, "2026-10-20T09:00:00", "2026-10-20T09:00:00Z", "2026-10-20T09:00:00Z")

	tx, err := f.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(f.s.q(`SELECT 1 FROM item_schedules WHERE id = ? FOR UPDATE`), sc.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.s.fireOneReminder(*sc.ReminderID, "2026-10-29T12:00:00Z")
		done <- err
	}()
	waitForLockWait(t, f.s, "FOR NO KEY UPDATE OF", done)
	// The reminder row is still free: the fire has not reached it.
	if _, err := tx.Exec(f.s.q(`UPDATE item_reminders SET updated_at = ? WHERE schedule_id = ?`), now(), sc.ID); err != nil {
		t.Fatalf("the reminder row was already held by the waiting fire: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("fire: %v", err)
	}
	if n := f.events(t); n != 1 {
		t.Errorf("events = %d", n)
	}
}

// A floating schedule fires in the recipient's zone: armed at the UTC+14 bound,
// the tick finds it early for a recipient further west and defers it to the
// exact instant without an event, and without occupying the batch.
func TestScheduleFloatingFiresInTheRecipientZoneWithDeferral(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	la := f.user(t, "la4102@example.com", "America/Los_Angeles")
	f.assign(t, f.item.ID, la.ID)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Floating: true})
	rid := *sc.ReminderID
	if r := f.reminder(t, rid); r.RemindAt != "2026-11-01T19:00:00Z" {
		t.Fatalf("armed at %s, want the +14 bound", r.RemindAt)
	}

	// A live ordinary reminder due later than the bound, behind the floating one
	// in the candidate order.
	live := armReminder(t, f.s, f.ws.ID, f.another(t, "Live").ID, "2026-11-01T19:30:00Z")

	// 20:00Z on the 1st: past the bound, but 12:00 in Los Angeles. A batch of
	// one is taken by the floating row, which defers instead of firing...
	got, err := f.s.FireDueReminders("2026-11-01T20:00:00Z", 1)
	if err != nil || len(got) != 0 {
		t.Fatalf("first pass fired %v (%v), want a deferral and nothing else", got, err)
	}
	r := f.reminder(t, rid)
	if r.FiredAt != nil || r.RemindAt != "2026-11-02T17:00:00Z" {
		t.Fatalf("deferred row = fired %v remind_at %s, want armed at 2026-11-02T17:00:00Z", r.FiredAt, r.RemindAt)
	}
	if n := f.events(t); n != 0 {
		t.Fatalf("a deferral wrote %d events", n)
	}
	// ...and so the next batch of one reaches the live reminder.
	got, err = f.s.FireDueReminders("2026-11-01T20:00:00Z", 1)
	if err != nil || len(got) != 1 || got[0].ID != live {
		t.Fatalf("second pass fired %v (%v), want the live reminder: the deferral starved it", got, err)
	}
	// The exact instant fires once, with the occurrence in the recipient's zone.
	got = f.fire(t, "2026-11-02T17:00:00Z")
	if len(got) != 1 || got[0].ID != rid || *got[0].OccurrenceAt != "2026-11-02T17:00:00Z" {
		t.Fatalf("exact pass fired %+v", got)
	}

	// A recipient at UTC+14 is due at the bound itself.
	other := f.another(t, "East")
	kiri := f.user(t, "kiri24102@example.com", "Pacific/Kiritimati")
	f.assign(t, other.ID, kiri.ID)
	east, err := f.s.createScheduleAt(f.ws.ID, other.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Floating: true}, schedNow)
	if err != nil {
		t.Fatal(err)
	}
	got = f.fire(t, "2026-11-01T19:00:00Z")
	if len(got) != 1 || got[0].ID != *east.ReminderID || *got[0].OccurrenceAt != "2026-11-01T19:00:00Z" {
		t.Fatalf("east recipient fired %+v", got)
	}
}

// An unset recipient zone is UTC, and an all-day floating occurrence fires at
// 09:00 local minus the lead.
func TestScheduleFloatingUnsetZoneAndAllDay(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{Kind: "deadline_hard", Dtstart: "2026-11-02", Floating: true, LeadMinutes: 60})
	if got := f.fire(t, "2026-11-02T07:59:00Z"); len(got) != 0 {
		t.Fatalf("fired early: %v", got)
	}
	if r := f.reminder(t, *sc.ReminderID); r.RemindAt != "2026-11-02T08:00:00Z" {
		t.Fatalf("deferred to %s, want 08:00Z (09:00 UTC minus the lead)", r.RemindAt)
	}
	got := f.fire(t, "2026-11-02T08:00:00Z")
	if len(got) != 1 || *got[0].OccurrenceAt != "2026-11-02T00:00:00Z" {
		t.Fatalf("fired %+v", got)
	}
}

// Reassigning before the fire changes the zone the fire reads, and the
// assignee hook pulls an already-deferred row back to the bound.
func TestScheduleAssigneeHookResetsADeferredFloatingRow(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	la := f.user(t, "hook-la4102@example.com", "America/Los_Angeles")
	east := f.user(t, "hook-east4102@example.com", "Pacific/Kiritimati")
	f.assign(t, f.item.ID, la.ID)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Floating: true})
	rid := *sc.ReminderID
	f.fire(t, "2026-11-01T20:00:00Z") // defers to 17:00Z on the 2nd for Los Angeles
	if r := f.reminder(t, rid); r.RemindAt != "2026-11-02T17:00:00Z" {
		t.Fatalf("setup: %s", r.RemindAt)
	}

	if _, err := f.s.UpdateItem(f.item.ID, models.ItemUpdate{AssignedUserID: &east.ID}); err != nil {
		t.Fatal(err)
	}
	if r := f.reminder(t, rid); r.RemindAt != "2026-11-01T19:00:00Z" {
		t.Fatalf("after reassigning east, remind_at = %s, want the bound 2026-11-01T19:00:00Z", r.RemindAt)
	}
	if got := f.fire(t, "2026-11-01T20:00:00Z"); len(got) != 1 {
		t.Fatalf("the east assignee's occurrence did not fire on time: %v", got)
	}

	// An update that does not change the assignee leaves a deferred row alone.
	other := f.another(t, "Untouched")
	f.assign(t, other.ID, la.ID)
	sc2, err := f.s.createScheduleAt(f.ws.ID, other.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Floating: true}, schedNow)
	if err != nil {
		t.Fatal(err)
	}
	f.fire(t, "2026-11-01T20:00:00Z")
	title := "Renamed"
	if _, err := f.s.UpdateItem(other.ID, models.ItemUpdate{Title: &title}); err != nil {
		t.Fatal(err)
	}
	if r := f.reminder(t, *sc2.ReminderID); r.RemindAt != "2026-11-02T17:00:00Z" {
		t.Errorf("an unrelated update moved the row to %s", r.RemindAt)
	}
}

// The users.timezone writers pull the user's armed floating rows back to the
// bound and touch nothing else.
func TestScheduleTimezoneHooksResetArmedFloatingRows(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	la := f.user(t, "tz-la4102@example.com", "America/Los_Angeles")
	bystander := f.user(t, "tz-by4102@example.com", "America/Los_Angeles")
	f.assign(t, f.item.ID, la.ID)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Floating: true})
	pinnedItem := f.another(t, "Pinned")
	f.assign(t, pinnedItem.ID, la.ID)
	pinned, err := f.s.createScheduleAt(f.ws.ID, pinnedItem.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Tz: "UTC"}, schedNow)
	if err != nil {
		t.Fatal(err)
	}
	byItem := f.another(t, "Bystander")
	f.assign(t, byItem.ID, bystander.ID)
	by, err := f.s.createScheduleAt(f.ws.ID, byItem.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Floating: true}, schedNow)
	if err != nil {
		t.Fatal(err)
	}
	f.fire(t, "2026-11-01T20:00:00Z") // defers both floating rows
	if r := f.reminder(t, *sc.ReminderID); r.RemindAt != "2026-11-02T17:00:00Z" {
		t.Fatalf("setup: %s", r.RemindAt)
	}

	tz := "Pacific/Kiritimati"
	if _, err := f.s.UpdateUser(la.ID, models.UserUpdate{Timezone: &tz}); err != nil {
		t.Fatal(err)
	}
	if r := f.reminder(t, *sc.ReminderID); r.RemindAt != "2026-11-01T19:00:00Z" {
		t.Errorf("UpdateUser: remind_at = %s, want the bound", r.RemindAt)
	}
	if r := f.reminder(t, *by.ReminderID); r.RemindAt != "2026-11-02T17:00:00Z" {
		t.Errorf("another user's row moved to %s", r.RemindAt)
	}
	if r := f.reminder(t, *pinned.ReminderID); r.RemindAt != "2026-11-02T09:00:00Z" {
		t.Errorf("a pinned row moved to %s", r.RemindAt)
	}

	// A fired row is history and stays.
	f.fire(t, "2026-11-01T20:00:00Z")
	fired := f.reminder(t, *sc.ReminderID)
	if fired.FiredAt == nil {
		t.Fatal("setup: the east occurrence did not fire")
	}
	back := "America/Los_Angeles"
	if _, err := f.s.UpdateUser(la.ID, models.UserUpdate{Timezone: &back}); err != nil {
		t.Fatal(err)
	}
	if r := f.reminder(t, *sc.ReminderID); r.RemindAt != fired.RemindAt || r.FiredAt == nil {
		t.Errorf("a fired row was reset: %+v", r)
	}

	// SetUserTimezoneIfUnset: an unset zone reads as UTC, so the row is deferred
	// to 09:00Z; choosing Kiritimati pulls it back.
	late := f.user(t, "tz-late4102@example.com", "")
	lateItem := f.another(t, "Late")
	f.assign(t, lateItem.ID, late.ID)
	ls, err := f.s.createScheduleAt(f.ws.ID, lateItem.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-03T09:00", Floating: true}, schedNow)
	if err != nil {
		t.Fatal(err)
	}
	f.fire(t, "2026-11-02T20:00:00Z")
	if r := f.reminder(t, *ls.ReminderID); r.RemindAt != "2026-11-03T09:00:00Z" {
		t.Fatalf("setup: %s", r.RemindAt)
	}
	if _, err := f.s.SetUserTimezoneIfUnset(late.ID, "Pacific/Kiritimati"); err != nil {
		t.Fatal(err)
	}
	if r := f.reminder(t, *ls.ReminderID); r.RemindAt != "2026-11-02T19:00:00Z" {
		t.Errorf("SetUserTimezoneIfUnset: remind_at = %s, want the bound 2026-11-02T19:00:00Z", r.RemindAt)
	}
}

// A stored rule that stops compiling leaves the row fired with no event and an
// error_code, and the next tick does not touch it again.
func TestScheduleFireWithABrokenRuleSetsErrorCodeAndDoesNotLoop(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})
	f.force(t, sc, "2026-10-28T09:00:00", "2026-10-28T09:00:00Z", "2026-10-28T09:00:00Z")
	f.exec(t, `UPDATE item_schedules SET rrule = ? WHERE id = ?`, "FREQ=DAILY;BOGUS=1", sc.ID)

	if got := f.fire(t, "2026-10-29T12:00:00Z"); len(got) != 0 {
		t.Fatalf("a broken rule fired %v", got)
	}
	if n := f.events(t); n != 0 {
		t.Fatalf("a broken rule wrote %d events", n)
	}
	got := f.reload(t, f.item.ID)
	if got.ErrorCode == nil || *got.ErrorCode != string(schedule.CodeUnknownPart) {
		t.Fatalf("error_code = %v, want %s", got.ErrorCode, schedule.CodeUnknownPart)
	}
	if got.EndedAt != nil {
		t.Errorf("the schedule was ended")
	}
	if r := f.reminder(t, *sc.ReminderID); r.FiredAt == nil {
		t.Errorf("the row is not left fired")
	}
	before := f.reminder(t, *sc.ReminderID).UpdatedAt
	if again := f.fire(t, "2026-10-29T12:01:00Z"); len(again) != 0 {
		t.Errorf("the next tick fired %v", again)
	}
	if ids, _ := f.s.dueReminderCandidates("2026-10-29T12:01:00Z", 0); len(ids) != 0 {
		t.Errorf("the broken row is a candidate again: %v", ids)
	}
	if after := f.reminder(t, *sc.ReminderID).UpdatedAt; after != before {
		t.Errorf("the next tick rewrote the row")
	}
	// A timing edit that compiles clears the code and re-arms.
	good := "FREQ=WEEKLY"
	if fixed := f.update(t, models.ScheduleUpdate{Rrule: &good}); fixed.ErrorCode != nil {
		t.Errorf("error_code survived a fixing edit: %v", fixed.ErrorCode)
	}
}

// The writer stores whatever ScheduleErrorCode says, and the set is the ten
// schedule codes plus the five caps.
func TestScheduleErrorCodeSetIsComplete(t *testing.T) {
	t.Parallel()
	codes := []schedule.Code{schedule.CodeSyntax, schedule.CodeUnknownPart, schedule.CodeDuplicatePart, schedule.CodeMissingPart,
		schedule.CodeRange, schedule.CodeCombination, schedule.CodeUnsupported, schedule.CodeUnsatisfiable, schedule.CodeZone, schedule.CodeWall}
	for _, c := range codes {
		if got := ScheduleErrorCode(fmt.Errorf("wrapped: %w", &schedule.MalformedError{Code: c})); got != string(c) {
			t.Errorf("%s -> %q", c, got)
		}
	}
	for _, k := range []schedule.CapKind{schedule.CapScan, schedule.CapStall, schedule.CapTime, schedule.CapWindow, schedule.CapInstances} {
		if got := ScheduleErrorCode(&schedule.CapError{Kind: k}); got != "cap_"+string(k) {
			t.Errorf("%s -> %q", k, got)
		}
	}
	if got := ScheduleErrorCode(errors.New("x")); got != "" {
		t.Errorf("a foreign error -> %q", got)
	}
}

// Archiving stops the fire; restoring resumes it with one catch-up fire.
func TestScheduleArchiveStopsFireAndRestoreCatchesUp(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})
	f.force(t, sc, "2026-10-26T09:00:00", "2026-10-26T09:00:00Z", "2026-10-26T09:00:00Z")
	if err := f.s.DeleteItem(f.item.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.fire(t, "2026-10-29T12:00:00Z"); len(got) != 0 {
		t.Fatalf("an archived item fired %v", got)
	}
	if _, err := f.s.RestoreItem(f.item.ID); err != nil {
		t.Fatal(err)
	}
	got := f.fire(t, "2026-10-29T12:00:00Z")
	if len(got) != 1 || *got[0].OccurrenceAt != "2026-10-26T09:00:00Z" {
		t.Fatalf("after restore fired %+v", got)
	}
	if again := f.fire(t, "2026-10-29T12:00:30Z"); len(again) != 0 {
		t.Errorf("restore fired twice")
	}
}

// Deleting the item removes the schedule, its managed row and its occurrences,
// and nothing fires afterwards.
func TestScheduleCascadeOnItemDeleteStopsFiring(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})
	f.force(t, sc, "2026-10-26T09:00:00", "2026-10-26T09:00:00Z", "2026-10-26T09:00:00Z")
	f.exec(t, `DELETE FROM items WHERE id = ?`, f.item.ID)
	if got := f.fire(t, "2026-10-29T12:00:00Z"); len(got) != 0 {
		t.Fatalf("fired %v after the item was deleted", got)
	}
	for _, tbl := range []string{"item_schedules", "item_reminders", "schedule_occurrences"} {
		if n := f.count(t, `SELECT COUNT(*) FROM `+tbl); n != 0 {
			t.Errorf("%s has %d rows", tbl, n)
		}
	}
	if n := f.events(t); n != 0 {
		t.Errorf("events = %d", n)
	}
}

// The reads fill occurrence_at: stored for a pinned schedule, read in the
// recipient's zone for an armed floating one (a fired floating row carries none).
func TestScheduleReadsCarryScheduleIDAndOccurrenceAt(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	la := f.user(t, "reads4102@example.com", "America/Los_Angeles")
	f.assign(t, f.item.ID, la.ID)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Floating: true})
	rs, err := f.s.ListRemindersForItem(f.ws.ID, f.item.ID)
	if err != nil || len(rs) != 1 {
		t.Fatalf("ListRemindersForItem: %v %v", rs, err)
	}
	if rs[0].ScheduleID == nil || *rs[0].ScheduleID != sc.ID || rs[0].OccurrenceAt == nil || *rs[0].OccurrenceAt != "2026-11-02T17:00:00Z" {
		t.Errorf("floating read = %+v", rs[0])
	}

	other := f.another(t, "Pinned reads")
	_, err = f.s.createScheduleAt(f.ws.ID, other.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Tz: "America/New_York"}, schedNow)
	if err != nil {
		t.Fatal(err)
	}
	f.fire(t, "2026-11-02T20:00:00Z")
	f.fire(t, "2026-11-02T20:00:00Z")
	pending, _, err := f.s.ListPendingReminders(f.ws.ID, PendingReminderScope{}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, pr := range pending {
		if pr.ScheduleID == nil {
			t.Errorf("a managed pending reminder has no schedule_id: %+v", pr.Reminder)
			continue
		}
		seen++
		if *pr.ScheduleID == sc.ID {
			// Fired and floating: no reading (the H-L2 fallback, TASK-4102 PR2).
			if pr.OccurrenceAt != nil {
				t.Errorf("pending fired floating occurrence_at = %v, want none", *pr.OccurrenceAt)
			}
			continue
		}
		if pr.OccurrenceAt == nil || *pr.OccurrenceAt != "2026-11-02T14:00:00Z" {
			t.Errorf("pending pinned occurrence_at = %v, want 2026-11-02T14:00:00Z", pr.OccurrenceAt)
		}
	}
	if seen != 2 {
		t.Errorf("pending managed rows = %d, want 2", seen)
	}
	// An ordinary reminder carries neither member.
	plain := armReminder(t, f.s, f.ws.ID, f.another(t, "Plain").ID, past)
	if r := f.reminder(t, plain); r.ScheduleID != nil || r.OccurrenceAt != nil {
		t.Errorf("ordinary reminder carries schedule members: %+v", r)
	}
}

// Re-anchoring: after a fire the start moves to the fired occurrence where that
// provably changes nothing, and the rules that cannot move it do not.
func TestScheduleReanchorOnFire(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		dtstart string
		tz      string
		rule    string
		rdates  []string
		fire    string // the outstanding occurrence wall to force
		anchors bool
	}{
		{"daily", "2026-10-01T09:00", "UTC", "FREQ=DAILY", nil, "2026-10-26T09:00:00", true},
		{"weekly byday", "2026-09-01T09:00", "Europe/London", "FREQ=WEEKLY;BYDAY=MO,WE", nil, "2026-10-26T09:00:00", true},
		{"interval phase", "2026-09-01T09:00", "America/New_York", "FREQ=WEEKLY;INTERVAL=3;BYDAY=TU", nil, "2026-10-13T09:00:00", true},
		{"monthly 31st", "2026-01-31T10:00", "UTC", "FREQ=MONTHLY;BYMONTHDAY=31", nil, "2026-08-31T10:00:00", true},
		{"monthly last friday", "2026-01-01T08:30", "Asia/Tokyo", "FREQ=MONTHLY;BYDAY=-1FR", nil, "2026-09-25T08:30:00", true},
		{"yearly", "2020-02-29T09:00", "UTC", "FREQ=YEARLY", nil, "2024-02-29T09:00:00", true},
		{"all-day weekly", "2026-09-01", "Australia/Sydney", "FREQ=WEEKLY;BYDAY=FR", nil, "2026-10-23", true},
		{"unsynced start", "2026-10-06T09:00", "UTC", "FREQ=WEEKLY;BYDAY=FR", nil, "2026-10-23T09:00:00", true},
		{"count rule", "2026-10-01T09:00", "UTC", "FREQ=DAILY;COUNT=40", nil, "2026-10-26T09:00:00", false},
		{"rdate occurrence", "2026-10-01T09:00", "UTC", "FREQ=WEEKLY", []string{"2026-10-26T12:00:00"}, "2026-10-26T12:00:00", false},
		{"no rule", "2026-10-26T09:00", "UTC", "", nil, "2026-10-26T09:00:00", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSchedFixture(t)
			// Create from a clock just before the occurrence so the fixture is valid,
			// then force the outstanding occurrence to fire.
			created, err := f.s.createScheduleAt(f.ws.ID, f.item.ID, models.ScheduleCreate{
				Kind: "check", Dtstart: tc.dtstart, Tz: tc.tz, Rrule: tc.rule, RDates: tc.rdates,
			}, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatal(err)
			}
			wall, err := schedule.ParseWall(tc.fire)
			if err != nil {
				t.Fatal(err)
			}
			loc, _ := time.LoadLocation(tc.tz)
			at := schedule.Resolve(loc, wall)
			f.force(t, created, tc.fire, at.UTC().Format(time.RFC3339), at.UTC().Format(time.RFC3339))
			if got := f.fire(t, "2026-12-31T00:00:00Z"); len(got) != 1 {
				t.Fatalf("fired %d", len(got))
			}
			sc := f.reload(t, f.item.ID)
			if !tc.anchors {
				if sc.AnchorWall != nil {
					t.Fatalf("anchor_wall = %s, want none", *sc.AnchorWall)
				}
				return
			}
			if sc.AnchorWall == nil || *sc.AnchorWall != tc.fire {
				t.Fatalf("anchor_wall = %v, want %s", sc.AnchorWall, tc.fire)
			}

			// Differential: the same occurrences from the original start and from
			// the re-anchored one, walking forward from the fired instant.
			orig := mustCompile(t, tc.dtstart, tc.tz, tc.rule, tc.rdates)
			moved := mustCompile(t, *sc.AnchorWall, tc.tz, tc.rule, tc.rdates)
			q := at
			for i := 0; i < 8; i++ {
				a, aok, aerr := orig.NextAfter(q, nil, schedule.DefaultLimits)
				b, bok, berr := moved.NextAfter(q, nil, schedule.DefaultLimits)
				if aerr != nil || berr != nil {
					t.Fatalf("step %d: %v / %v", i, aerr, berr)
				}
				if aok != bok || (aok && (!a.At.Equal(b.At) || a.Wall != b.Wall)) {
					t.Fatalf("step %d diverged: original %v/%v, re-anchored %v/%v", i, a, aok, b, bok)
				}
				if !aok {
					break
				}
				q = a.At
			}
			from, to := at, at.Add(60*24*time.Hour)
			ba, err1 := orig.Between(from, to, nil, schedule.DefaultLimits)
			bb, err2 := moved.Between(from, to, nil, schedule.DefaultLimits)
			if err1 != nil || err2 != nil || len(ba) != len(bb) {
				t.Fatalf("Between: %v %v %d vs %d", err1, err2, len(ba), len(bb))
			}
			for i := range ba {
				if !ba[i].At.Equal(bb[i].At) {
					t.Fatalf("Between[%d]: %s vs %s", i, ba[i].At, bb[i].At)
				}
			}

			// Any timing edit clears it.
			w := "FREQ=MONTHLY"
			if upd := f.update(t, models.ScheduleUpdate{Rrule: &w}); upd.AnchorWall != nil {
				t.Errorf("a rule edit kept anchor_wall %s", *upd.AnchorWall)
			}
		})
	}
}

func mustCompile(t *testing.T, start, tz, rule string, rdates []string) *schedule.Compiled {
	t.Helper()
	c, err := compileScheduleSpec(scheduleSpecInput{Dtstart: start, Tz: tz, Rrule: rule, RDates: rdates})
	if err != nil {
		t.Fatalf("compile %s %s: %v", start, rule, err)
	}
	return c
}

// A wall-clock sub-daily rule started years back cannot be walked from its
// dtstart (CapScan); the re-anchored start can.
func TestScheduleReanchorAvoidsCapScan(t *testing.T) {
	t.Parallel()
	const rule = "FREQ=MINUTELY;BYHOUR=9;BYMINUTE=0,15,30,45"
	old := mustCompile(t, "2000-01-01T09:00", "UTC", rule, nil)
	nowT := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	var capErr *schedule.CapError
	if _, _, err := old.NextAfter(nowT, nil, schedule.DefaultLimits); !errors.As(err, &capErr) || capErr.Kind != schedule.CapScan {
		t.Fatalf("the original start did not hit CapScan: %v", err)
	}

	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-28T09:00", Tz: "UTC", Rrule: rule})
	f.force(t, sc, "2026-10-29T09:15:00", "2026-10-29T09:15:00Z", "2026-10-29T09:15:00Z")
	if got := f.fire(t, "2026-10-29T09:20:00Z"); len(got) != 1 {
		t.Fatalf("fired %d", len(got))
	}
	got := f.reload(t, f.item.ID)
	if got.AnchorWall == nil || *got.AnchorWall != "2026-10-29T09:15:00" {
		t.Fatalf("anchor_wall = %v", got.AnchorWall)
	}
	moved := mustCompile(t, *got.AnchorWall, "UTC", rule, nil)
	next, ok, err := moved.NextAfter(nowT.Add(-3*time.Hour+20*time.Minute), nil, schedule.DefaultLimits)
	if err != nil || !ok || next.Wall.String() != "2026-10-29T09:30:00" {
		t.Fatalf("re-anchored walk: %v %v %v", next, ok, err)
	}
}

// The recipient hook is an optimisation, not a dependency: with it failing (or
// never running, e.g. a crash between the user commit and the hook), the zone
// write still lands and an armed floating row fires with its occurrence in the
// NEW zone, because the fire re-resolves the zone.
func TestScheduleFloatingFiresInTheNewZoneWhenTheTimezoneHookFails(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	la := f.user(t, "tz-fault4102@example.com", "America/Los_Angeles")
	f.assign(t, f.item.ID, la.ID)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Floating: true})
	f.fire(t, "2026-11-01T20:00:00Z") // defers to 09:00 in Los Angeles
	if r := f.reminder(t, *sc.ReminderID); r.RemindAt != "2026-11-02T17:00:00Z" {
		t.Fatalf("setup: %s", r.RemindAt)
	}

	f.s.resetFloatingFault = func() error { return errors.New("injected hook failure") }
	tz := "Pacific/Kiritimati"
	if _, err := f.s.UpdateUser(la.ID, models.UserUpdate{Timezone: &tz}); err != nil {
		t.Fatalf("the user write failed with the hook: %v", err)
	}
	u, err := f.s.GetUser(la.ID)
	if err != nil || u == nil || u.Timezone == nil || *u.Timezone != tz {
		t.Fatalf("the zone was not persisted: %+v (%v)", u, err)
	}
	if r := f.reminder(t, *sc.ReminderID); r.RemindAt != "2026-11-02T17:00:00Z" {
		t.Fatalf("the failed hook still moved the row to %s", r.RemindAt)
	}

	fired := f.fire(t, "2026-11-02T18:00:00Z")
	if len(fired) != 1 {
		t.Fatalf("fired %d, want 1", len(fired))
	}
	if got := fired[0].OccurrenceAt; got == nil || *got != "2026-11-01T19:00:00Z" {
		t.Errorf("occurrence = %v, want 2026-11-01T19:00:00Z (09:00 in Kiritimati)", got)
	}
}

// Postgres: UpdateSchedule takes the item before the schedule, like every other
// writer. With another transaction holding the item FOR UPDATE it waits at the
// item and the schedule row is still free.
func TestScheduleUpdateWaitsOnTheItemBeforeTheSchedule(t *testing.T) {
	pgURL := os.Getenv("PAD_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set: lock order only exists on Postgres")
	}
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})

	holder, err := f.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	if _, err := holder.Exec(f.s.q(`SELECT 1 FROM items WHERE id = ? FOR UPDATE`), f.item.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		l := sc.LeadMinutes + 5
		_, err := f.s.updateScheduleAt(f.ws.ID, f.item.ID, models.ScheduleUpdate{LeadMinutes: &l}, schedNow)
		done <- err
	}()
	waitForLockWait(t, f.s, "FOR KEY SHARE", done)

	// The schedule row is still free: the update has not reached it.
	probe, err := f.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := probe.Exec(f.s.q(`SELECT 1 FROM item_schedules WHERE id = ? FOR UPDATE NOWAIT`), sc.ID); err != nil {
		probe.Rollback()
		t.Fatalf("the schedule row was already held by the waiting update: %v", err)
	}
	probe.Rollback()

	if err := holder.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := f.reload(t, f.item.ID); got.LeadMinutes != sc.LeadMinutes+5 {
		t.Errorf("lead_minutes = %d, want %d", got.LeadMinutes, sc.LeadMinutes+5)
	}
}

// A broken floating rule records error_code on the FIRST fire attempt: the
// compile runs before the floating deferral, so the row is not deferred (and
// remind_at not moved) once before the failure is recorded.
func TestScheduleBrokenFloatingRuleRecordsErrorCodeWithoutDeferral(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	la := f.user(t, "labroken4102@example.com", "America/Los_Angeles")
	f.assign(t, f.item.ID, la.ID)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Floating: true, Rrule: "FREQ=DAILY"})
	rid := *sc.ReminderID
	armed := f.reminder(t, rid).RemindAt
	if armed != "2026-11-01T19:00:00Z" {
		t.Fatalf("armed at %s, want the +14 bound", armed)
	}
	f.exec(t, `UPDATE item_schedules SET rrule = ? WHERE id = ?`, "FREQ=DAILY;BOGUS=1", sc.ID)

	// 20:00Z on the 1st is past the bound, but the exact instant (17:00Z on the
	// 2nd, in Los Angeles) is still ahead.
	if got := f.fire(t, "2026-11-01T20:00:00Z"); len(got) != 0 {
		t.Fatalf("a broken rule fired %v", got)
	}
	if n := f.events(t); n != 0 {
		t.Fatalf("a broken rule wrote %d events", n)
	}
	got := f.reload(t, f.item.ID)
	if got.ErrorCode == nil || *got.ErrorCode != string(schedule.CodeUnknownPart) {
		t.Fatalf("error_code = %v, want %s on the first attempt", got.ErrorCode, schedule.CodeUnknownPart)
	}
	r := f.reminder(t, rid)
	if r.RemindAt != armed {
		t.Errorf("remind_at = %s, want %s: the row was deferred", r.RemindAt, armed)
	}
	if r.FiredAt == nil {
		t.Errorf("the row is not left fired")
	}
}

// H-L2 fallback (TASK-4102 PR2): a FIRED floating managed row has no
// occurrence_at on a read. The zone it fired in is gone from the row, so
// recomputing from next_wall in the recipient's CURRENT zone would disagree
// with the event payload and the dedupe key after a zone change. Fails if the
// guard in fillOccurrenceAt is removed (the read then shows the Tokyo reading).
func TestFiredFloatingReminderOmitsOccurrenceAfterZoneChange(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	la := f.user(t, "hl2-la4102@example.com", "America/Los_Angeles")
	f.assign(t, f.item.ID, la.ID)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Floating: true})
	rid := *sc.ReminderID

	// Armed: the reading is in the recipient's zone.
	if r := f.reminder(t, rid); r.OccurrenceAt == nil || *r.OccurrenceAt != "2026-11-02T17:00:00Z" {
		t.Fatalf("armed floating row occurrence_at = %v, want 2026-11-02T17:00:00Z", r.OccurrenceAt)
	}

	f.fire(t, "2026-11-01T20:00:00Z") // defers to the exact instant
	got := f.fire(t, "2026-11-02T17:00:00Z")
	if len(got) != 1 || got[0].OccurrenceAt == nil || *got[0].OccurrenceAt != "2026-11-02T17:00:00Z" {
		t.Fatalf("fired %+v", got)
	}
	f.exec(t, `UPDATE users SET timezone = ? WHERE id = ?`, "Asia/Tokyo", la.ID)

	check := func(name string, r *models.Reminder) {
		t.Helper()
		if r == nil || r.ScheduleID == nil || *r.ScheduleID != sc.ID {
			t.Fatalf("%s: managed row missing or lost schedule_id: %+v", name, r)
		}
		if r.OccurrenceAt != nil {
			t.Errorf("%s: fired floating row reports occurrence_at %s after a zone change; want none", name, *r.OccurrenceAt)
		}
	}
	check("GetReminder", f.reminder(t, rid))
	list, err := f.s.ListRemindersForItem(f.ws.ID, f.item.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListRemindersForItem: %v %v", list, err)
	}
	check("ListRemindersForItem", list[0])
	pending, _, err := f.s.ListPendingReminders(f.ws.ID, PendingReminderScope{}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range pending {
		if p.ID == rid {
			found = true
			check("ListPendingReminders", &p.Reminder)
		}
	}
	if !found {
		t.Fatalf("the fired managed row is not pending: %+v", pending)
	}

	// The event payload still carries the reading it fired with.
	var payload string
	if err := f.s.db.QueryRow(f.s.q(`SELECT payload FROM event_outbox WHERE event_type = ?`), kernelevents.ItemReminderDue).Scan(&payload); err != nil {
		t.Fatalf("outbox: %v", err)
	}
	var pl struct {
		Reminder struct {
			OccurrenceAt *string `json:"occurrence_at"`
		} `json:"reminder"`
	}
	if err := json.Unmarshal([]byte(payload), &pl); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if pl.Reminder.OccurrenceAt == nil || *pl.Reminder.OccurrenceAt != "2026-11-02T17:00:00Z" {
		t.Errorf("payload lost the fired occurrence: %s", payload)
	}

	// Controls: an armed floating row follows the recipient's zone, and a
	// fired pinned row keeps its stored instant.
	armed := f.another(t, "Armed")
	f.assign(t, armed.ID, la.ID)
	asc, err := f.s.createScheduleAt(f.ws.ID, armed.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-09T09:00", Floating: true}, schedNow)
	if err != nil {
		t.Fatal(err)
	}
	if r := f.reminder(t, *asc.ReminderID); r.OccurrenceAt == nil || *r.OccurrenceAt != "2026-11-09T00:00:00Z" {
		t.Errorf("armed floating row = %v, want the Tokyo reading 2026-11-09T00:00:00Z", r.OccurrenceAt)
	}
	pinned := f.another(t, "Pinned")
	psc, err := f.s.createScheduleAt(f.ws.ID, pinned.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-03T09:00", Tz: "UTC"}, schedNow)
	if err != nil {
		t.Fatal(err)
	}
	f.fire(t, "2026-11-03T09:00:00Z")
	if r := f.reminder(t, *psc.ReminderID); r.FiredAt == nil || r.OccurrenceAt == nil || *r.OccurrenceAt != "2026-11-03T09:00:00Z" {
		t.Errorf("fired pinned row = %+v, want its stored instant", r)
	}
}
