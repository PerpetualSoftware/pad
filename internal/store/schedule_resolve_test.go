package store

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4103 C2b: resolving the outstanding occurrence. Runs on SQLite in make
// test and on Postgres in make test-pg.

func rt(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// mk creates a schedule on a fresh item, armed from created.
func (f *schedFixture) mk(t *testing.T, in models.ScheduleCreate, created string) (*models.Item, *models.ItemSchedule) {
	t.Helper()
	item := f.another(t, fmt.Sprintf("Sched %d", time.Now().UnixNano()))
	sc, err := f.s.createScheduleAt(f.ws.ID, item.ID, in, rt(created))
	if err != nil {
		t.Fatalf("createScheduleAt: %v", err)
	}
	return item, sc
}

func (f *schedFixture) resolve(t *testing.T, item *models.Item, in ResolveInput, now string) (*ResolveResult, error) {
	t.Helper()
	return f.s.resolveOccurrenceAt(f.ws.ID, item.ID, in, rt(now))
}

func (f *schedFixture) mustResolve(t *testing.T, item *models.Item, in ResolveInput, now string) *ResolveResult {
	t.Helper()
	res, err := f.resolve(t, item, in, now)
	if err != nil {
		t.Fatalf("resolve %s at %s: %v", in.State, now, err)
	}
	return res
}

func (f *schedFixture) comment(item *models.Item, userID, body string) *ResolveComment {
	return &ResolveComment{
		UserID:   userID,
		Input:    models.CommentCreate{Body: body, CreatedBy: "user", Source: "web", Author: "Tester"},
		Activity: models.Activity{WorkspaceID: f.ws.ID, DocumentID: item.ID, Action: "commented", Actor: "user", Source: "web", UserID: userID},
	}
}

func (f *schedFixture) occRows(t *testing.T, scheduleID, state string) int {
	t.Helper()
	return f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id = ? AND state = ?`, scheduleID, state)
}

func (f *schedFixture) occTotal(t *testing.T, scheduleID string) int {
	t.Helper()
	return f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id = ?`, scheduleID)
}

func isStale(err error) *ErrStaleOccurrence {
	var st *ErrStaleOccurrence
	if errors.As(err, &st) {
		return st
	}
	return nil
}

var dailyUTC = models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}

// done advances the schedule, re-arms the SAME managed row, writes the
// occurrence row and the comment, and emits no event of its own: one event per
// fire. RED if resolve emits an event, inserts a second managed row, leaves
// fired_at set (the next event never comes) or skips the occurrence row.
func TestResolveAdvancesAndRearms(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	u := f.user(t, "res-adv@example.com", "")
	item, sc := f.mk(t, dailyUTC, "2026-10-29T12:00:00Z")
	rid := *sc.ReminderID
	f.fire(t, "2026-10-30T09:00:30Z")
	if n := f.events(t); n != 1 {
		t.Fatalf("events after the fire = %d", n)
	}
	if !f.reload(t, item.ID).ReminderFired {
		t.Fatal("setup: the managed row is not fired")
	}

	res := f.mustResolve(t, item, ResolveInput{OccurrenceAt: rt("2026-10-30T09:00:00Z"), State: "done", Actor: "user", Comment: f.comment(item, u.ID, "paid it")}, "2026-10-30T09:05:00Z")
	if res.Comment == nil || res.Comment.Body != "paid it" || res.MissedRecorded != 0 {
		t.Fatalf("result %+v", res)
	}
	got := res.Schedule
	if got.NextAt == nil || *got.NextAt != "2026-10-31T09:00:00Z" || *got.NextWall != "2026-10-31T09:00:00" || got.NextOverrideAt != nil || got.EndedAt != nil {
		t.Fatalf("schedule after done: %+v", got)
	}
	if got.ReminderID == nil || *got.ReminderID != rid {
		t.Fatalf("the managed row changed identity: %v != %s", got.ReminderID, rid)
	}
	if r := f.reminder(t, rid); r.FiredAt != nil || r.AckedAt != nil || r.RemindAt != "2026-10-31T09:00:00Z" {
		t.Fatalf("managed row after done: %+v", r)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM item_reminders WHERE schedule_id = ?`, sc.ID); n != 1 {
		t.Fatalf("managed rows = %d", n)
	}
	var cid, actor, state string
	if err := f.s.db.QueryRow(f.s.q(`SELECT state, comment_id, actor FROM schedule_occurrences WHERE schedule_id = ?`), sc.ID).Scan(&state, &cid, &actor); err != nil {
		t.Fatalf("occurrence row: %v", err)
	}
	if state != "done" || cid != res.Comment.ID || actor != "user" {
		t.Errorf("occurrence row = %s %s %s", state, cid, actor)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM comments WHERE item_id = ?`, item.ID); n != 1 {
		t.Errorf("comments = %d", n)
	}
	if n := f.events(t); n != 1 {
		t.Errorf("resolve emitted an event: %d", n)
	}
	f.fire(t, "2026-10-31T08:59:00Z")
	if n := f.events(t); n != 1 {
		t.Errorf("fired before the next due: %d", n)
	}
	f.fire(t, "2026-10-31T09:00:00Z")
	if n := f.events(t); n != 2 {
		t.Errorf("events after the next due = %d, want 2", n)
	}
}

// Passed-over instances become `missed` rows, the outstanding one is not among
// them, and the partition at now-1s has no gap and no overlap. A tick fires the
// schedule once however many instances pass. RED if the count includes O, is
// read from the table, a tick re-fires, or an early resolve returns X as next.
func TestResolveMissedSingleFireAndPartition(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	in := models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-27T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}
	item, sc := f.mk(t, in, "2026-10-27T00:00:00Z")
	for _, ts := range []string{"2026-10-27T09:00:30Z", "2026-10-28T09:00:30Z", "2026-10-29T09:00:30Z", "2026-10-30T09:00:30Z"} {
		f.fire(t, ts)
	}
	if n := f.events(t); n != 1 {
		t.Fatalf("events = %d, want a single fire", n)
	}
	cur := f.reload(t, item.ID)
	if n, trunc, err := ScheduleMissed(cur, nil, rt("2026-10-30T09:30:00Z")); err != nil || n != 3 || trunc {
		t.Fatalf("missed count = %d %v %v, want 3", n, trunc, err)
	}
	res := f.mustResolve(t, item, ResolveInput{OccurrenceAt: rt("2026-10-27T09:00:00Z"), State: "done", Actor: "user"}, "2026-10-30T09:30:00Z")
	if res.MissedRecorded != 3 || f.occRows(t, sc.ID, "missed") != 3 || f.occRows(t, sc.ID, "done") != 1 || f.occTotal(t, sc.ID) != 4 {
		t.Errorf("rows: missed %d done %d total %d recorded %d", f.occRows(t, sc.ID, "missed"), f.occRows(t, sc.ID, "done"), f.occTotal(t, sc.ID), res.MissedRecorded)
	}
	if n := *res.Schedule.NextAt; n != "2026-10-31T09:00:00Z" {
		t.Errorf("next = %s, want 2026-10-31T09:00:00Z", n)
	}

	// The boundary: an instance at exactly now-1s is missed, at now it is next.
	for _, c := range []struct {
		now         string
		missed      int
		wantNextDay string
	}{
		{"2026-10-28T09:00:01Z", 1, "2026-10-29T09:00:00Z"},
		{"2026-10-28T09:00:00Z", 0, "2026-10-28T09:00:00Z"},
	} {
		item, sc := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-27T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}, "2026-10-27T00:00:00Z")
		res := f.mustResolve(t, item, ResolveInput{OccurrenceAt: rt("2026-10-27T09:00:00Z"), State: "skip", Actor: "user"}, c.now)
		if f.occRows(t, sc.ID, "missed") != c.missed || *res.Schedule.NextAt != c.wantNextDay {
			t.Errorf("now %s: missed %d next %s, want %d / %s", c.now, f.occRows(t, sc.ID, "missed"), *res.Schedule.NextAt, c.missed, c.wantNextDay)
		}
		if f.occRows(t, sc.ID, "skipped") != 1 {
			t.Errorf("now %s: no skipped row", c.now)
		}
	}

	// Early resolve (pull-forward): nothing missed, and the next is the instance
	// AFTER X, not X again.
	item, sc = f.mk(t, in, "2026-10-27T00:00:00Z")
	res = f.mustResolve(t, item, ResolveInput{OccurrenceAt: rt("2026-10-27T09:00:00Z"), State: "done", Actor: "user"}, "2026-10-27T08:00:00Z")
	if f.occRows(t, sc.ID, "missed") != 0 || *res.Schedule.NextAt != "2026-10-28T09:00:00Z" {
		t.Errorf("early resolve: missed %d next %s", f.occRows(t, sc.ID, "missed"), *res.Schedule.NextAt)
	}
}

// completion: the next one is a recurrence interval after the DONE moment, at
// the schedule's time of day; calendar is the control. RED if the calendar path
// is used, if month addition overflows (Mar 3), or if UNTIL is ignored.
func TestCompletionAnchor(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	cases := []struct {
		name        string
		in          models.ScheduleCreate
		created     string
		x, done     string
		wantAt      string // "" = the schedule ends
		wantWall    string
		wantAnchor  string
		floatingTZ  string
		controlWall string
	}{
		{name: "weekly Monday done Thursday", in: models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Tz: "UTC", Rrule: "FREQ=WEEKLY", Anchor: "completion"},
			created: "2026-10-29T12:00:00Z", x: "2026-11-02T09:00:00Z", done: "2026-11-05T15:00:00Z", wantAt: "2026-11-12T09:00:00Z", wantWall: "2026-11-12T09:00:00", controlWall: "2026-11-09T09:00:00"},
		{name: "monthly Jan 31", in: models.ScheduleCreate{Kind: "check", Dtstart: "2027-01-31T09:00", Tz: "UTC", Rrule: "FREQ=MONTHLY", Anchor: "completion"},
			created: "2027-01-01T00:00:00Z", x: "2027-01-31T09:00:00Z", done: "2027-01-31T10:00:00Z", wantAt: "2027-02-28T09:00:00Z", wantWall: "2027-02-28T09:00:00"},
		{name: "monthly Jan 31 leap", in: models.ScheduleCreate{Kind: "check", Dtstart: "2028-01-31T09:00", Tz: "UTC", Rrule: "FREQ=MONTHLY", Anchor: "completion"},
			created: "2028-01-01T00:00:00Z", x: "2028-01-31T09:00:00Z", done: "2028-01-31T10:00:00Z", wantAt: "2028-02-29T09:00:00Z", wantWall: "2028-02-29T09:00:00"},
		{name: "quarterly", in: models.ScheduleCreate{Kind: "check", Dtstart: "2027-01-31T09:00", Tz: "UTC", Rrule: "FREQ=MONTHLY;INTERVAL=3", Anchor: "completion"},
			created: "2027-01-01T00:00:00Z", x: "2027-01-31T09:00:00Z", done: "2027-01-31T10:00:00Z", wantAt: "2027-04-30T09:00:00Z", wantWall: "2027-04-30T09:00:00"},
		{name: "yearly Feb 29", in: models.ScheduleCreate{Kind: "check", Dtstart: "2028-02-29T09:00", Tz: "UTC", Rrule: "FREQ=YEARLY", Anchor: "completion"},
			created: "2028-01-01T00:00:00Z", x: "2028-02-29T09:00:00Z", done: "2028-02-29T10:00:00Z", wantAt: "2029-02-28T09:00:00Z", wantWall: "2029-02-28T09:00:00"},
		{name: "spring-forward gap shifts forward", in: models.ScheduleCreate{Kind: "check", Dtstart: "2027-03-12T02:30", Tz: "America/New_York", Rrule: "FREQ=DAILY", Anchor: "completion"},
			created: "2027-03-01T00:00:00Z", x: "2027-03-12T07:30:00Z", done: "2027-03-13T12:00:00Z", wantAt: "2027-03-14T07:30:00Z", wantWall: "2027-03-14T02:30:00"},
		{name: "all-day stays a date", in: models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02", Tz: "UTC", Rrule: "FREQ=DAILY", Anchor: "completion"},
			created: "2026-10-29T12:00:00Z", x: "2026-11-02T00:00:00Z", done: "2026-11-05T15:00:00Z", wantAt: "2026-11-06T00:00:00Z", wantWall: "2026-11-06"},
		{name: "hourly is exact", in: models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Tz: "UTC", Rrule: "FREQ=HOURLY;INTERVAL=2", Anchor: "completion"},
			created: "2026-10-29T12:00:00Z", x: "2026-11-02T09:00:00Z", done: "2026-11-02T10:17:23Z", wantAt: "2026-11-02T12:17:23Z", wantWall: "2026-11-02T12:17:23"},
		{name: "UNTIL passed ends it", in: models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Tz: "UTC", Rrule: "FREQ=WEEKLY;UNTIL=20261110T000000Z", Anchor: "completion"},
			created: "2026-10-29T12:00:00Z", x: "2026-11-02T09:00:00Z", done: "2026-11-05T15:00:00Z"},
		{name: "floating recipient zone", in: models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Floating: true, Rrule: "FREQ=WEEKLY", Anchor: "completion"},
			created: "2026-10-29T12:00:00Z", x: "2026-11-02T17:00:00Z", done: "2026-11-06T03:00:00Z", floatingTZ: "America/Los_Angeles", wantWall: "2026-11-12T09:00:00"},
	}
	for _, c := range cases {
		item, sc := f.mk(t, c.in, c.created)
		if c.floatingTZ != "" {
			u := f.user(t, "completion-la@example.com", c.floatingTZ)
			f.assign(t, item.ID, u.ID)
		}
		res := f.mustResolve(t, item, ResolveInput{OccurrenceAt: rt(c.x), State: "done", Actor: "user"}, c.done)
		got := res.Schedule
		if c.wantAt == "" && c.wantWall == "" {
			if got.EndedAt == nil || got.NextAt != nil || got.ReminderID != nil {
				t.Errorf("%s: schedule not ended: %+v", c.name, got)
			}
			continue
		}
		if got.NextWall == nil || *got.NextWall != c.wantWall {
			t.Errorf("%s: next_wall = %v, want %s", c.name, deref(got.NextWall), c.wantWall)
		}
		if c.wantAt != "" && (got.NextAt == nil || *got.NextAt != c.wantAt) {
			t.Errorf("%s: next_at = %v, want %s", c.name, deref(got.NextAt), c.wantAt)
		}
		if got.AnchorWall == nil || *got.AnchorWall != c.wantWall {
			t.Errorf("%s: anchor_wall = %v, want the candidate %s", c.name, deref(got.AnchorWall), c.wantWall)
		}
		if c.controlWall != "" {
			cal := c.in
			cal.Anchor = "calendar"
			item2, _ := f.mk(t, cal, c.created)
			r2 := f.mustResolve(t, item2, ResolveInput{OccurrenceAt: rt(c.x), State: "done", Actor: "user"}, c.done)
			if *r2.Schedule.NextWall != c.controlWall {
				t.Errorf("%s: calendar control next_wall = %s, want %s", c.name, *r2.Schedule.NextWall, c.controlWall)
			}
		}
		_ = sc
	}
}

// completion is valid only for a plain interval (or none): BYxxx, COUNT, RDATES
// and EXDATES are refused on create and on update; a plain interval, an UNTIL
// and a one-shot are accepted.
func TestCompletionAnchorRuleSet(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	bad := []models.ScheduleCreate{
		{Kind: "check", Dtstart: "2026-11-02T09:00", Tz: "UTC", Rrule: "FREQ=WEEKLY;BYDAY=MO,TH", Anchor: "completion"},
		{Kind: "check", Dtstart: "2026-11-02T09:00", Tz: "UTC", Rrule: "FREQ=MONTHLY;BYMONTHDAY=1", Anchor: "completion"},
		{Kind: "check", Dtstart: "2026-11-02T09:00", Tz: "UTC", Rrule: "FREQ=DAILY;COUNT=5", Anchor: "completion"},
		{Kind: "check", Dtstart: "2026-11-02T09:00", Tz: "UTC", Rrule: "FREQ=DAILY", RDates: []string{"2026-11-20T09:00"}, Anchor: "completion"},
		{Kind: "check", Dtstart: "2026-11-02T09:00", Tz: "UTC", Rrule: "FREQ=DAILY", ExDates: []string{"2026-11-03T09:00"}, Anchor: "completion"},
	}
	for i, in := range bad {
		item := f.another(t, fmt.Sprintf("Bad %d", i))
		_, err := f.s.createScheduleAt(f.ws.ID, item.ID, in, schedNow)
		var fe *ScheduleFieldError
		if !errors.As(err, &fe) || fe.Field != "anchor" || fe.Code != "combination" {
			t.Errorf("create %d: %v, want anchor/combination", i, err)
		}
		if n := f.count(t, `SELECT COUNT(*) FROM item_schedules WHERE item_id = ?`, item.ID); n != 0 {
			t.Errorf("create %d stored a schedule", i)
		}
	}
	for i, in := range []models.ScheduleCreate{
		{Kind: "check", Dtstart: "2026-11-02T09:00", Tz: "UTC", Rrule: "FREQ=MONTHLY;INTERVAL=3", Anchor: "completion"},
		{Kind: "check", Dtstart: "2026-11-02T09:00", Tz: "UTC", Rrule: "FREQ=WEEKLY;UNTIL=20271231T000000Z", Anchor: "completion"},
		{Kind: "check", Dtstart: "2026-11-02T09:00", Tz: "UTC", Anchor: "completion"},
	} {
		item := f.another(t, fmt.Sprintf("Good %d", i))
		if _, err := f.s.createScheduleAt(f.ws.ID, item.ID, in, schedNow); err != nil {
			t.Errorf("accepted create %d: %v", i, err)
		}
	}

	// Update: calendar BYDAY -> completion is refused; completion plain -> BYDAY too.
	cal, _ := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Tz: "UTC", Rrule: "FREQ=WEEKLY;BYDAY=MO,TH"}, "2026-10-29T12:00:00Z")
	completion := "completion"
	var fe *ScheduleFieldError
	if _, err := f.s.updateScheduleAt(f.ws.ID, cal.ID, models.ScheduleUpdate{Anchor: &completion}, schedNow); !errors.As(err, &fe) || fe.Field != "anchor" {
		t.Errorf("update anchor on a BYDAY rule: %v", err)
	}
	plain, _ := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Tz: "UTC", Rrule: "FREQ=WEEKLY", Anchor: "completion"}, "2026-10-29T12:00:00Z")
	rule := "FREQ=WEEKLY;BYDAY=MO,TH"
	fe = nil
	if _, err := f.s.updateScheduleAt(f.ws.ID, plain.ID, models.ScheduleUpdate{Rrule: &rule}, schedNow); !errors.As(err, &fe) || fe.Field != "anchor" {
		t.Errorf("update rule on a completion schedule: %v", err)
	}
	// A non-timing edit never trips the rule.
	lead := 5
	if _, err := f.s.updateScheduleAt(f.ws.ID, plain.ID, models.ScheduleUpdate{LeadMinutes: &lead}, schedNow); err != nil {
		t.Errorf("lead edit on a completion schedule: %v", err)
	}
}

// A soft deadline moves only with a reason; a hard one is snoozed, never moved.
// The refusal runs before any write. RED if the check is missing or runs late.
func TestResolveSoftDeadlineMoveNeedsReason(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	u := f.user(t, "res-soft@example.com", "")
	mv := func(kind string) (*models.Item, *models.ItemSchedule) {
		in := dailyUTC
		in.Kind = kind
		if kind == "window" {
			in.Dtend = "2026-10-30T10:00"
		}
		return f.mk(t, in, "2026-10-29T12:00:00Z")
	}
	until := rt("2026-10-31T15:00:00Z")
	base := ResolveInput{OccurrenceAt: rt("2026-10-30T09:00:00Z"), State: "move", Until: &until, Actor: "user"}

	item, sc := mv("deadline_soft")
	snap := func() string {
		cur := f.reload(t, item.ID)
		r := f.reminder(t, *sc.ReminderID)
		return fmt.Sprint(deref(cur.NextAt), deref(cur.NextOverrideAt), cur.UpdatedAt, r.RemindAt, r.UpdatedAt,
			f.occTotal(t, sc.ID), f.count(t, `SELECT COUNT(*) FROM comments WHERE item_id = ?`, item.ID))
	}
	before := snap()
	for _, body := range []string{"", "   "} {
		in := base
		in.Comment = f.comment(item, u.ID, body)
		var fe *ScheduleFieldError
		if _, err := f.resolve(t, item, in, "2026-10-30T08:00:00Z"); !errors.As(err, &fe) || fe.Field != "comment" || fe.Code != "required" {
			t.Errorf("body %q: %v, want comment/required", body, err)
		}
	}
	in := base
	if _, err := f.resolve(t, item, in, "2026-10-30T08:00:00Z"); err == nil {
		t.Error("no comment at all: accepted")
	}
	if after := snap(); after != before {
		t.Errorf("a refused move wrote: %s -> %s", before, after)
	}
	in.Comment = f.comment(item, u.ID, "waiting on the vendor")
	res := f.mustResolve(t, item, in, "2026-10-30T08:00:00Z")
	if res.Schedule.NextOverrideAt == nil || *res.Schedule.NextOverrideAt != "2026-10-31T15:00:00Z" {
		t.Errorf("override = %v", deref(res.Schedule.NextOverrideAt))
	}

	// check and window need no reason; hard: move refused, snooze fine.
	for _, kind := range []string{"check", "window"} {
		item, _ := mv(kind)
		if _, err := f.resolve(t, item, base, "2026-10-30T08:00:00Z"); err != nil {
			t.Errorf("%s move without a reason: %v", kind, err)
		}
	}
	hard, _ := mv("deadline_hard")
	var fe *ScheduleFieldError
	if _, err := f.resolve(t, hard, base, "2026-10-30T08:00:00Z"); !errors.As(err, &fe) || fe.Field != "state" || fe.Code != "combination" {
		t.Errorf("hard move: %v", err)
	}
	sn := base
	sn.State = "snooze"
	if _, err := f.resolve(t, hard, sn, "2026-10-30T08:00:00Z"); err != nil {
		t.Errorf("hard snooze: %v", err)
	}
}

// Input shape: the until rules and the enum.
func TestResolveInputShape(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	item, sc := f.mk(t, dailyUTC, "2026-10-29T12:00:00Z")
	now := "2026-10-30T08:00:00Z"
	x := rt("2026-10-30T09:00:00Z")
	past, far, ok := rt("2026-10-30T07:00:00Z"), rt("2027-12-30T08:00:00Z"), rt("2026-10-31T08:00:00Z")
	for _, c := range []struct {
		name        string
		in          ResolveInput
		field, code string
	}{
		{"bad state", ResolveInput{OccurrenceAt: x, State: "later"}, "state", "enum"},
		{"empty state", ResolveInput{OccurrenceAt: x}, "state", "enum"},
		{"snooze needs until", ResolveInput{OccurrenceAt: x, State: "snooze"}, "until", "required"},
		{"move needs until", ResolveInput{OccurrenceAt: x, State: "move"}, "until", "required"},
		{"done refuses until", ResolveInput{OccurrenceAt: x, State: "done", Until: &ok}, "until", "combination"},
		{"skip refuses until", ResolveInput{OccurrenceAt: x, State: "skip", Until: &ok}, "until", "combination"},
		{"until in the past", ResolveInput{OccurrenceAt: x, State: "snooze", Until: &past}, "until", "range"},
		{"until beyond 366 days", ResolveInput{OccurrenceAt: x, State: "snooze", Until: &far}, "until", "range"},
	} {
		var fe *ScheduleFieldError
		if _, err := f.resolve(t, item, c.in, now); !errors.As(err, &fe) || fe.Field != c.field || fe.Code != c.code {
			t.Errorf("%s: %v, want %s/%s", c.name, err, c.field, c.code)
		}
	}
	if n := f.occTotal(t, sc.ID); n != 0 {
		t.Errorf("occurrence rows = %d", n)
	}
}

// Stale: a wrong instant, an already-resolved instant, an ended schedule and a
// floating schedule whose recipient zone moved after the read each answer
// ErrStaleOccurrence with the current schedule and write nothing. A fired
// floating row resolves with the instant its event carried, zone change or not.
func TestResolveStale(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	item, sc := f.mk(t, dailyUTC, "2026-10-29T12:00:00Z")
	now := "2026-10-30T09:05:00Z"
	if _, err := f.resolve(t, item, ResolveInput{OccurrenceAt: rt("2026-10-30T10:00:00Z"), State: "done", Actor: "user"}, now); isStale(err) == nil {
		t.Errorf("wrong instant: %v", err)
	}
	if f.occTotal(t, sc.ID) != 0 {
		t.Error("a stale resolve wrote")
	}
	in := ResolveInput{OccurrenceAt: rt("2026-10-30T09:00:00Z"), State: "done", Actor: "user"}
	f.mustResolve(t, item, in, now)
	_, err := f.resolve(t, item, in, now)
	st := isStale(err)
	if st == nil || st.Ended() || st.Schedule.NextAt == nil || *st.Schedule.NextAt != "2026-10-31T09:00:00Z" {
		t.Errorf("second submit: %v (%+v)", err, st)
	}

	// Ended.
	one, _ := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "UTC"}, "2026-10-29T12:00:00Z")
	f.mustResolve(t, one, in, now)
	if _, err := f.resolve(t, one, in, now); isStale(err) == nil || !isStale(err).Ended() {
		t.Errorf("ended: %v", err)
	}

	// Floating, armed: the zone moved after the read.
	la := f.user(t, "res-stale-la@example.com", "America/Los_Angeles")
	fl, _ := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Floating: true, Rrule: "FREQ=DAILY"}, "2026-10-29T12:00:00Z")
	f.assign(t, fl.ID, la.ID)
	read := rt("2026-11-02T17:00:00Z") // the LA reading
	f.exec(t, `UPDATE users SET timezone = ? WHERE id = ?`, "Asia/Tokyo", la.ID)
	if _, err := f.resolve(t, fl, ResolveInput{OccurrenceAt: read, State: "done", Actor: "user"}, "2026-11-02T18:00:00Z"); isStale(err) == nil {
		t.Errorf("floating after a zone change: %v", err)
	}
	// ...but the caller's own reading is accepted when it names its zone.
	loc, _ := time.LoadLocation("America/Los_Angeles")
	f.mustResolve(t, fl, ResolveInput{OccurrenceAt: read, State: "done", Actor: "user", View: loc}, "2026-11-02T18:00:00Z")

	// Floating, FIRED: the event's instant resolves after the zone moves.
	ff, fsc := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-09T09:00", Floating: true, Rrule: "FREQ=DAILY"}, "2026-10-29T12:00:00Z")
	f.assign(t, ff.ID, la.ID) // Tokyo now
	f.fire(t, "2026-11-08T20:00:00Z")
	got := f.fire(t, "2026-11-09T00:00:00Z")
	if len(got) != 1 || got[0].OccurrenceAt == nil || *got[0].OccurrenceAt != "2026-11-09T00:00:00Z" {
		t.Fatalf("fired %+v", got)
	}
	f.exec(t, `UPDATE users SET timezone = ? WHERE id = ?`, "America/Los_Angeles", la.ID)
	f.mustResolve(t, ff, ResolveInput{OccurrenceAt: rt(*got[0].OccurrenceAt), State: "done", Actor: "user"}, "2026-11-09T01:00:00Z")
	_ = fsc
}

// snooze re-fires the same occurrence at until; move changes its due instant; a
// later done overwrites the `moved` row; done after a snooze clears the
// override; an all-day move across a zone change does not hit the wall unique
// index.
func TestResolveSnoozeAndMove(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	u := f.user(t, "res-snooze@example.com", "")
	addDevice(t, f.s, u.ID)
	f.assign(t, f.item.ID, u.ID)

	// Snooze: the lead is added back so the refire lands on until.
	in := dailyUTC
	in.LeadMinutes = 30
	sc := f.create(t, in)
	item := f.item
	first := f.fire(t, "2026-10-30T08:30:00Z")
	if len(first) != 1 {
		t.Fatalf("fire: %v", first)
	}
	until := rt("2026-10-30T11:00:00Z")
	res := f.mustResolve(t, item, ResolveInput{OccurrenceAt: rt("2026-10-30T09:00:00Z"), State: "snooze", Until: &until, Actor: "user"}, "2026-10-30T09:10:00Z")
	got := res.Schedule
	if got.NextOverrideAt == nil || *got.NextOverrideAt != "2026-10-30T11:30:00Z" || *got.NextAt != "2026-10-30T09:00:00Z" || *got.NextWall != "2026-10-30T09:00:00" {
		t.Fatalf("snoozed: override %v next_at %v", deref(got.NextOverrideAt), deref(got.NextAt))
	}
	if r := f.reminder(t, *sc.ReminderID); r.FiredAt != nil || r.RemindAt != "2026-10-30T11:00:00Z" {
		t.Fatalf("row after snooze: %+v", r)
	}
	if f.occTotal(t, sc.ID) != 0 {
		t.Error("snooze wrote an occurrence row")
	}
	f.fire(t, "2026-10-30T10:59:59Z")
	if n := f.events(t); n != 1 {
		t.Fatalf("fired before until: %d events", n)
	}
	second := f.fire(t, "2026-10-30T11:00:00Z")
	if len(second) != 1 || *second[0].OccurrenceAt != "2026-10-30T09:00:00Z" {
		t.Fatalf("re-fire %+v, want the original occurrence", second)
	}
	if n := f.events(t); n != 2 {
		t.Errorf("events = %d", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM user_notifications WHERE user_id = ? AND kind = 'reminder'`, u.ID); n != 2 {
		t.Errorf("notifications = %d, want 2", n)
	}
	// done after the snooze clears the override and advances.
	res = f.mustResolve(t, item, ResolveInput{OccurrenceAt: rt("2026-10-30T09:00:00Z"), State: "done", Actor: "user"}, "2026-10-30T11:05:00Z")
	if res.Schedule.NextOverrideAt != nil || *res.Schedule.NextAt != "2026-10-31T09:00:00Z" {
		t.Errorf("after done: %+v", res.Schedule)
	}

	// Move: the new due instant (lead subtracted), a `moved` row, replaced by the
	// final done.
	in = dailyUTC
	in.LeadMinutes = 30
	mItem, msc := f.mk(t, in, "2026-10-29T12:00:00Z")
	mvUntil := rt("2026-10-31T15:00:00Z")
	res = f.mustResolve(t, mItem, ResolveInput{OccurrenceAt: rt("2026-10-30T09:00:00Z"), State: "move", Until: &mvUntil, Actor: "user"}, "2026-10-30T08:00:00Z")
	if r := f.reminder(t, *msc.ReminderID); r.RemindAt != "2026-10-31T14:30:00Z" {
		t.Errorf("moved row remind_at = %s, want 14:30 (due 15:00 less the lead)", r.RemindAt)
	}
	if *res.Schedule.NextOverrideAt != "2026-10-31T15:00:00Z" || f.occRows(t, msc.ID, "moved") != 1 {
		t.Errorf("moved: override %v rows %d", deref(res.Schedule.NextOverrideAt), f.occRows(t, msc.ID, "moved"))
	}
	f.mustResolve(t, mItem, ResolveInput{OccurrenceAt: rt("2026-10-30T09:00:00Z"), State: "done", Actor: "user"}, "2026-10-31T15:10:00Z")
	if f.occRows(t, msc.ID, "moved") != 0 || f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id = ? AND state = 'done' AND occurrence_at = ?`, msc.ID, "2026-10-30T09:00:00Z") != 1 {
		t.Errorf("the done did not replace the moved row: %d total", f.occTotal(t, msc.ID))
	}

	// All-day: a moved row whose instant no longer matches (the zone shifted)
	// is still replaced, through the date.
	la := f.user(t, "res-allday-la@example.com", "America/Los_Angeles")
	aItem, asc := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02", Floating: true, Rrule: "FREQ=DAILY"}, "2026-10-29T12:00:00Z")
	f.assign(t, aItem.ID, la.ID)
	aUntil := rt("2026-11-03T20:00:00Z")
	f.mustResolve(t, aItem, ResolveInput{OccurrenceAt: rt("2026-11-02T08:00:00Z"), State: "move", Until: &aUntil, Actor: "user"}, "2026-11-01T12:00:00Z")
	f.exec(t, `UPDATE schedule_occurrences SET occurrence_at = ? WHERE schedule_id = ?`, "2026-11-01T15:00:00Z", asc.ID)
	f.mustResolve(t, aItem, ResolveInput{OccurrenceAt: rt("2026-11-02T08:00:00Z"), State: "done", Actor: "user"}, "2026-11-03T20:10:00Z")
	if f.occRows(t, asc.ID, "moved") != 0 || f.occRows(t, asc.ID, "done") != 1 {
		t.Errorf("all-day: moved %d done %d", f.occRows(t, asc.ID, "moved"), f.occRows(t, asc.ID, "done"))
	}
}

// The walk starts at the anchor, never at a dtstart years back: RED if resolve
// computes from dtstart (cap_scan) or ignores the floor. A floating variant with
// the recipient west of UTC pins the same clamp.
func TestResolveNeverWalksBeforeAnchor(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	for _, floating := range []bool{false, true} {
		in := models.ScheduleCreate{Kind: "check", Dtstart: "2000-01-01T09:00", Tz: "UTC", Rrule: "FREQ=MINUTELY;BYHOUR=9"}
		o := "2026-10-28T09:00:00Z"
		if floating {
			in.Tz, in.Floating = "", true
			o = "2026-10-28T16:00:00Z" // 09:00 in Los Angeles
		}
		item, sc := f.mk(t, in, "2000-01-01T00:00:00Z")
		if floating {
			la := f.user(t, "res-anchor-la@example.com", "America/Los_Angeles")
			f.assign(t, item.ID, la.ID)
		}
		f.exec(t, `UPDATE item_schedules SET next_at = ?, next_wall = ?, anchor_wall = ? WHERE id = ?`, o, "2026-10-28T09:00:00", "2026-10-28T09:00:00", sc.ID)
		now := rt(o).Add(10*time.Minute + time.Second).Format(time.RFC3339)
		res := f.mustResolve(t, item, ResolveInput{OccurrenceAt: rt(o), State: "done", Actor: "user"}, now)
		if res.MissedRecorded != 10 || *res.Schedule.NextWall != "2026-10-28T09:11:00" {
			t.Errorf("floating=%v: missed %d next %s, want 10 / 09:11", floating, res.MissedRecorded, deref(res.Schedule.NextWall))
		}
	}
}

// One-shot done ends the schedule and removes the managed row; PATCH timing
// revives it. A schedule with an error_code answers ErrScheduleBroken and
// writes nothing.
func TestResolveEndsOneShotAndBrokenRule(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	u := f.user(t, "res-end@example.com", "")
	item, sc := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "UTC"}, "2026-10-29T12:00:00Z")
	f.fire(t, "2026-10-30T09:00:30Z")
	res := f.mustResolve(t, item, ResolveInput{OccurrenceAt: rt("2026-10-30T09:00:00Z"), State: "done", Actor: "user"}, "2026-10-30T09:05:00Z")
	got := res.Schedule
	if got.EndedAt == nil || got.NextAt != nil || got.NextWall != nil || got.ReminderID != nil {
		t.Fatalf("ended schedule: %+v", got)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM item_reminders WHERE schedule_id = ?`, sc.ID); n != 0 {
		t.Errorf("managed rows = %d", n)
	}
	pending, _, err := f.s.ListPendingReminders(f.ws.ID, PendingReminderScope{}, 10, 0)
	if err != nil || len(pending) != 0 {
		t.Errorf("pending %v %v", pending, err)
	}
	dt := "2026-11-20T09:00"
	rev, err := f.s.updateScheduleAt(f.ws.ID, item.ID, models.ScheduleUpdate{Dtstart: &dt}, schedNow)
	if err != nil || rev.EndedAt != nil || rev.ReminderID == nil {
		t.Errorf("revive: %+v %v", rev, err)
	}

	bItem, bsc := f.mk(t, dailyUTC, "2026-10-29T12:00:00Z")
	f.exec(t, `UPDATE item_schedules SET error_code = 'cap_scan' WHERE id = ?`, bsc.ID)
	_, err = f.resolve(t, bItem, ResolveInput{OccurrenceAt: rt("2026-10-30T09:00:00Z"), State: "done", Actor: "user", Comment: f.comment(bItem, u.ID, "x")}, "2026-10-30T09:05:00Z")
	var br *ErrScheduleBroken
	if !errors.As(err, &br) || br.Code != "cap_scan" {
		t.Fatalf("broken: %v", err)
	}
	if f.occTotal(t, bsc.ID) != 0 || f.count(t, `SELECT COUNT(*) FROM comments WHERE item_id = ?`, bItem.ID) != 0 {
		t.Error("a refused resolve wrote")
	}
}

// Missed history is best-effort and bounded: 92 days, 500 instances, no error.
func TestMissedRowsAreBounded(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	item, sc := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2025-09-25T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}, "2025-09-25T00:00:00Z")
	res := f.mustResolve(t, item, ResolveInput{OccurrenceAt: rt("2025-09-25T09:00:00Z"), State: "done", Actor: "user"}, "2026-10-30T12:00:00Z")
	if n := f.occRows(t, sc.ID, "missed"); n != 92 || res.MissedRecorded != 92 {
		t.Errorf("daily ignored 400 days: %d missed rows (%d recorded), want 92", n, res.MissedRecorded)
	}
	if *res.Schedule.NextAt != "2026-10-31T09:00:00Z" {
		t.Errorf("next = %s", *res.Schedule.NextAt)
	}
	mItem, msc := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=MINUTELY"}, "2026-10-01T00:00:00Z")
	if _, err := f.resolve(t, mItem, ResolveInput{OccurrenceAt: rt("2026-10-01T09:00:00Z"), State: "skip", Actor: "user"}, "2026-10-30T12:00:00Z"); err != nil {
		t.Fatalf("minutely: %v", err)
	}
	if n := f.occRows(t, msc.ID, "missed"); n != 500 {
		t.Errorf("minutely missed rows = %d, want 500", n)
	}
}

// C2b-3: the missed count walks from O's WALL whatever the override, in the
// reader's zone, and next_occurrence.at is the frozen instant for every reader.
func TestSnoozedFloatingMissedCountIsReadInEachReadersZone(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	la := f.user(t, "c2b3-la@example.com", "America/Los_Angeles")
	item, _ := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Floating: true, Rrule: "FREQ=DAILY"}, "2026-10-29T12:00:00Z")
	f.assign(t, item.ID, la.ID)
	until := rt("2026-11-08T17:00:00Z")
	res := f.mustResolve(t, item, ResolveInput{OccurrenceAt: rt("2026-11-02T17:00:00Z"), State: "snooze", Until: &until, Actor: "user"}, "2026-10-29T12:00:00Z")
	sc := res.Schedule
	if sc.NextOverrideAt == nil {
		t.Fatal("setup: not snoozed")
	}
	now := rt("2026-11-05T18:00:00Z")
	for _, c := range []struct {
		zone string
		want int
	}{{"America/Los_Angeles", 3}, {"Asia/Tokyo", 3}, {"Pacific/Honolulu", 2}} {
		loc, err := time.LoadLocation(c.zone)
		if err != nil {
			t.Fatal(err)
		}
		n, _, err := ScheduleMissed(sc, loc, now)
		if err != nil || n != c.want {
			t.Errorf("%s: missed %d (%v), want %d", c.zone, n, err, c.want)
		}
		if occ, ok, err := OutstandingInstant(sc, loc); err != nil || !ok || occ.Format(time.RFC3339) != "2026-11-02T17:00:00Z" {
			t.Errorf("%s: next_occurrence.at = %v (%v), want the frozen instant", c.zone, occ, err)
		}
	}
}

// K goroutines resolve one occurrence: exactly one wins, the rest are stale, and
// one comment, one occurrence row and one advance result. A fire racing the
// resolve neither deadlocks nor emits more than one event. RED without the pin
// and the stale check inside the transaction.
func TestResolveConcurrentConflict(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	u := f.user(t, "res-race@example.com", "")
	const racers, rounds = 6, 10
	for round := 0; round < rounds; round++ {
		item, sc := f.mk(t, dailyUTC, "2026-10-29T12:00:00Z")
		before := f.events(t)
		var wg sync.WaitGroup
		start := make(chan struct{})
		var mu sync.Mutex
		wins, stales := 0, 0
		var other []error
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := f.s.resolveOccurrenceAt(f.ws.ID, item.ID, ResolveInput{OccurrenceAt: rt("2026-10-30T09:00:00Z"), State: "done", Actor: "user", Comment: f.comment(item, u.ID, "racing")}, rt("2026-10-30T09:05:00Z"))
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					wins++
				case isStale(err) != nil:
					stales++
				default:
					other = append(other, err)
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := f.s.FireDueReminders("2026-10-30T09:05:00Z", 0); err != nil {
				mu.Lock()
				other = append(other, err)
				mu.Unlock()
			}
		}()
		close(start)
		finished := make(chan struct{})
		go func() { wg.Wait(); close(finished) }()
		select {
		case <-finished:
		case <-time.After(45 * time.Second):
			t.Fatalf("round %d: the writers did not finish; a lock-order deadlock", round)
		}
		if len(other) != 0 {
			t.Fatalf("round %d: %v", round, other)
		}
		if wins != 1 || stales != racers-1 {
			t.Fatalf("round %d: %d wins, %d stale", round, wins, stales)
		}
		if n := f.occRows(t, sc.ID, "done"); n != 1 {
			t.Fatalf("round %d: %d done rows", round, n)
		}
		if n := f.count(t, `SELECT COUNT(*) FROM comments WHERE item_id = ?`, item.ID); n != 1 {
			t.Fatalf("round %d: %d comments", round, n)
		}
		if r := f.reminder(t, *sc.ReminderID); r.RemindAt != "2026-10-31T09:00:00Z" {
			t.Fatalf("round %d: remind_at %s, advanced more or less than once", round, r.RemindAt)
		}
		if got := f.events(t) - before; got > 1 {
			t.Fatalf("round %d: %d events from one occurrence", round, got)
		}
	}
}

// Postgres: resolve enters through the same pin as a fire, so a writer holding
// the schedule makes it wait in the pin before it touches the comment table or
// the reminder row.
func TestResolveWaitsOnTheScheduleBeforeTheReminder(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set: lock order only exists on Postgres")
	}
	t.Parallel()
	f := newSchedFixture(t)
	u := f.user(t, "res-pg@example.com", "")
	item, sc := f.mk(t, dailyUTC, "2026-10-29T12:00:00Z")

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
		_, err := f.s.resolveOccurrenceAt(f.ws.ID, item.ID, ResolveInput{OccurrenceAt: rt("2026-10-30T09:00:00Z"), State: "done", Actor: "user", Comment: f.comment(item, u.ID, "waits")}, rt("2026-10-30T09:05:00Z"))
		done <- err
	}()
	waitForLockWait(t, f.s, "FOR NO KEY UPDATE OF", done)
	if n := f.count(t, `SELECT COUNT(*) FROM comments WHERE item_id = ?`, item.ID); n != 0 {
		t.Errorf("the waiting resolve already wrote %d comments", n)
	}
	if _, err := tx.Exec(f.s.q(`UPDATE item_reminders SET updated_at = ? WHERE schedule_id = ?`), now(), sc.ID); err != nil {
		t.Fatalf("the reminder row was already held by the waiting resolve: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM comments WHERE item_id = ?`, item.ID); n != 1 {
		t.Errorf("comments after release = %d", n)
	}
}
