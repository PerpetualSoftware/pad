package store

import (
	"errors"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// TASK-4102 PR1a: the storage half of item schedules. Runs on SQLite in
// make test and on Postgres in make test-pg (testStore picks the dialect).

// schedNow is the clock every test here arms from: a Thursday noon UTC.
var schedNow = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

type schedFixture struct {
	s    *Store
	ws   *models.Workspace
	item *models.Item
}

func newSchedFixture(t *testing.T) *schedFixture {
	t.Helper()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Sched")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	return &schedFixture{s: s, ws: ws, item: createTestItem(t, s, ws.ID, col.ID, "Pay rent", "")}
}

func (f *schedFixture) another(t *testing.T, title string) *models.Item {
	t.Helper()
	cols, err := f.s.ListCollections(f.ws.ID)
	if err != nil || len(cols) == 0 {
		t.Fatalf("ListCollections: %v", err)
	}
	return createTestItem(t, f.s, f.ws.ID, cols[0].ID, title, "")
}

func (f *schedFixture) create(t *testing.T, in models.ScheduleCreate) *models.ItemSchedule {
	t.Helper()
	sc, err := f.s.createScheduleAt(f.ws.ID, f.item.ID, in, schedNow)
	if err != nil {
		t.Fatalf("createScheduleAt: %v", err)
	}
	return sc
}

func (f *schedFixture) update(t *testing.T, upd models.ScheduleUpdate) *models.ItemSchedule {
	t.Helper()
	sc, err := f.s.updateScheduleAt(f.ws.ID, f.item.ID, upd, schedNow)
	if err != nil {
		t.Fatalf("updateScheduleAt: %v", err)
	}
	return sc
}

// managed returns the schedule's managed reminder row.
func (f *schedFixture) managed(t *testing.T, sc *models.ItemSchedule) *models.Reminder {
	t.Helper()
	if sc.ReminderID == nil {
		t.Fatalf("schedule has no managed reminder: %+v", sc)
	}
	r, err := f.s.GetReminder(f.ws.ID, *sc.ReminderID)
	if err != nil || r == nil {
		t.Fatalf("GetReminder(%s): %v %v", *sc.ReminderID, r, err)
	}
	return r
}

func (f *schedFixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.s.db.Exec(f.s.q(q), args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func (f *schedFixture) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := f.s.db.QueryRow(f.s.q(q), args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

func TestScheduleCreateArmsOneManagedReminder(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{
		Kind: "check", Dtstart: "2026-11-02T09:00", Tz: "America/New_York", Rrule: "weekly", LeadMinutes: 30,
	})
	if sc.Dtstart != "2026-11-02T09:00:00" || sc.Rrule == nil || *sc.Rrule != "FREQ=WEEKLY" {
		t.Fatalf("not stored canonical: dtstart=%q rrule=%v", sc.Dtstart, sc.Rrule)
	}
	if sc.Anchor != "calendar" || sc.Audience != "both" || sc.Floating || sc.Tz == nil || *sc.Tz != "America/New_York" {
		t.Fatalf("defaults/zone wrong: %+v", sc)
	}
	// 09:00 EST on 2026-11-02 is 14:00Z; the lead takes 30 minutes off.
	if sc.NextAt == nil || *sc.NextAt != "2026-11-02T14:00:00Z" || sc.NextWall == nil || *sc.NextWall != "2026-11-02T09:00:00" {
		t.Fatalf("next = %v / %v", sc.NextAt, sc.NextWall)
	}
	r := f.managed(t, sc)
	if r.RemindAt != "2026-11-02T13:30:00Z" || r.FiredAt != nil || r.AckedAt != nil {
		t.Fatalf("managed reminder = %+v", r)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM item_reminders WHERE schedule_id = ?`, sc.ID); n != 1 {
		t.Fatalf("managed rows = %d, want 1", n)
	}
	got, err := f.s.GetSchedule(f.ws.ID, f.item.ID)
	if err != nil || got == nil || got.ID != sc.ID {
		t.Fatalf("GetSchedule: %v %v", got, err)
	}
	if got.RDates == nil || got.ExDates == nil {
		t.Fatal("rdates/exdates must read as empty arrays, not null")
	}
}

func TestScheduleAllDayFiresAtNineLocalMinusLead(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{Kind: "deadline_hard", Dtstart: "2026-11-02", Tz: "America/New_York", LeadMinutes: 60})
	// 09:00 EST = 14:00Z, minus the hour.
	if r := f.managed(t, sc); r.RemindAt != "2026-11-02T13:00:00Z" {
		t.Fatalf("all-day remind_at = %s", r.RemindAt)
	}
}

func TestScheduleFloatingArmsAtTheEarliestBound(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Floating: true})
	if sc.Tz != nil || !sc.Floating {
		t.Fatalf("floating schedule has a zone: %+v", sc)
	}
	// 09:00 read at UTC+14 is 19:00Z the day before: no zone can give it earlier.
	if sc.NextAt == nil || *sc.NextAt != "2026-11-01T19:00:00Z" {
		t.Fatalf("floating next_at = %v", sc.NextAt)
	}
	if r := f.managed(t, sc); r.RemindAt != "2026-11-01T19:00:00Z" {
		t.Fatalf("floating remind_at = %s", r.RemindAt)
	}
	for _, zone := range []string{"Pacific/Kiritimati", "America/Los_Angeles", "UTC", "Pacific/Pago_Pago"} {
		loc, _ := time.LoadLocation(zone)
		w, _ := schedule.ParseWall("2026-11-02T09:00")
		if got := schedule.Resolve(loc, w); got.Before(mustParseTime(t, *sc.NextAt)) {
			t.Errorf("%s gives %s, earlier than the bound %s", zone, got, *sc.NextAt)
		}
	}
}

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// The recipient's zone is the assignee's, else the creator's, else UTC.
func TestScheduleRecipientZone(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	read := func() string {
		loc, err := f.s.scheduleRecipientZone(f.s.db, f.item.ID)
		if err != nil {
			t.Fatal(err)
		}
		return loc.String()
	}
	if got := read(); got != "UTC" {
		t.Fatalf("no user: %s", got)
	}
	creator := createTestUser(t, f.s, "creator4102@example.com", "C", "password123")
	assignee := createTestUser(t, f.s, "assignee4102@example.com", "A", "password123")
	if _, err := f.s.SetUserTimezoneIfUnset(creator.ID, "Europe/London"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetUserTimezoneIfUnset(assignee.ID, "Pacific/Kiritimati"); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE items SET created_by_user_id = ? WHERE id = ?`, creator.ID, f.item.ID)
	if got := read(); got != "Europe/London" {
		t.Fatalf("creator: %s", got)
	}
	f.exec(t, `UPDATE items SET assigned_user_id = ? WHERE id = ?`, assignee.ID, f.item.ID)
	if got := read(); got != "Pacific/Kiritimati" {
		t.Fatalf("assignee: %s", got)
	}
	f.exec(t, `UPDATE users SET timezone = NULL WHERE id = ?`, assignee.ID)
	if got := read(); got != "UTC" {
		t.Fatalf("assignee without a zone is UTC, not the creator's: %s", got)
	}
}

func TestScheduleFloatingOccurrenceIsPickedInTheRecipientZone(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	u := createTestUser(t, f.s, "kiri4102@example.com", "K", "password123")
	if _, err := f.s.SetUserTimezoneIfUnset(u.ID, "Pacific/Kiritimati"); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE items SET assigned_user_id = ? WHERE id = ?`, u.ID, f.item.ID)
	// 12:00Z is already 02:00 on 10-30 in Kiritimati, so "daily 01:00" has
	// passed there on the 30th, and the next occurrence is the 31st. In UTC
	// it would be the 30th.
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T01:00", Floating: true, Rrule: "FREQ=DAILY"})
	if sc.NextWall == nil || *sc.NextWall != "2026-10-31T01:00:00" {
		t.Fatalf("next_wall = %v, want 2026-10-31T01:00:00", sc.NextWall)
	}
}

func TestScheduleOneShotInThePastIsExhausted(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	_, err := f.s.createScheduleAt(f.ws.ID, f.item.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-01-01T09:00", Tz: "UTC"}, schedNow)
	if !errors.Is(err, ErrScheduleExhausted) {
		t.Fatalf("err = %v, want ErrScheduleExhausted", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM item_schedules`); n != 0 {
		t.Fatalf("rows persisted: %d", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM item_reminders`); n != 0 {
		t.Fatalf("reminders persisted: %d", n)
	}
}

func TestScheduleRefusalsPersistNothing(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	valid := models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Tz: "UTC"}
	with := func(mut func(*models.ScheduleCreate)) models.ScheduleCreate { c := valid; mut(&c); return c }

	malformed := map[string]models.ScheduleCreate{
		"syntax":       with(func(c *models.ScheduleCreate) { c.Rrule = "FREQ=" }),
		"unknown_part": with(func(c *models.ScheduleCreate) { c.Rrule = "FREQ=DAILY;BOGUS=1" }),
		"zone":         with(func(c *models.ScheduleCreate) { c.Tz = "Not/AZone" }),
		"zone ":        with(func(c *models.ScheduleCreate) { c.Floating, c.Tz = true, "UTC" }),
		"wall":         with(func(c *models.ScheduleCreate) { c.Dtstart = "2026-11-02T09:00:00Z" }),
		"wall  ":       with(func(c *models.ScheduleCreate) { c.Dtstart = "2026-02-30" }),
	}
	for want, in := range malformed {
		_, err := f.s.createScheduleAt(f.ws.ID, f.item.ID, in, schedNow)
		var m *schedule.MalformedError
		if !errors.As(err, &m) {
			t.Errorf("%q: err = %v, want *schedule.MalformedError", want, err)
			continue
		}
		if ScheduleErrorCode(err) != string(m.Code) || (want[len(want)-1] != ' ' && string(m.Code) != want) {
			t.Errorf("%q: code = %s", want, m.Code)
		}
	}

	fields := map[string]models.ScheduleCreate{
		"kind":         with(func(c *models.ScheduleCreate) { c.Kind = "nope" }),
		"anchor":       with(func(c *models.ScheduleCreate) { c.Anchor = "nope" }),
		"audience":     with(func(c *models.ScheduleCreate) { c.Audience = "nope" }),
		"lead_minutes": with(func(c *models.ScheduleCreate) { c.LeadMinutes = maxScheduleLeadMinutes + 1 }),
	}
	for field, in := range fields {
		_, err := f.s.createScheduleAt(f.ws.ID, f.item.ID, in, schedNow)
		var fe *ScheduleFieldError
		if !errors.As(err, &fe) || fe.Field != field {
			t.Errorf("%s: err = %v", field, err)
		}
	}
	combos := []struct {
		name string
		in   models.ScheduleCreate
		code string
	}{
		{"window without dtend", with(func(c *models.ScheduleCreate) { c.Kind = "window" }), "required"},
		{"dtend on a check", with(func(c *models.ScheduleCreate) { c.Dtend = "2026-11-02T10:00" }), "combination"},
		{"window ends before it starts", with(func(c *models.ScheduleCreate) { c.Kind, c.Dtend = "window", "2026-11-02T09:00" }), "range"},
		{"window mixes all-day and timed", with(func(c *models.ScheduleCreate) { c.Kind, c.Dtend = "window", "2026-11-03" }), "combination"},
	}
	for _, tc := range combos {
		_, err := f.s.createScheduleAt(f.ws.ID, f.item.ID, tc.in, schedNow)
		var fe *ScheduleFieldError
		if !errors.As(err, &fe) || fe.Field != "dtend" || fe.Code != tc.code {
			t.Errorf("%s: err = %v, want dtend/%s", tc.name, err, tc.code)
		}
	}
	// A window that is valid, for contrast.
	ok := with(func(c *models.ScheduleCreate) { c.Kind, c.Dtend = "window", "2026-11-02T10:00" })
	if _, err := f.s.createScheduleAt(f.ws.ID, f.item.ID, ok, schedNow); err != nil {
		t.Fatalf("valid window refused: %v", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM item_schedules`); n != 1 {
		t.Fatalf("rows = %d, want only the valid window", n)
	}
}

func TestScheduleOnePerItemAndLiveItemOnly(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	in := models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Tz: "UTC"}
	f.create(t, in)
	if _, err := f.s.createScheduleAt(f.ws.ID, f.item.ID, in, schedNow); !errors.Is(err, ErrScheduleExists) {
		t.Fatalf("second create: %v, want ErrScheduleExists", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM item_reminders WHERE schedule_id IS NOT NULL`); n != 1 {
		t.Fatalf("managed rows = %d after a refused second create", n)
	}

	other := f.another(t, "other")
	if err := f.s.DeleteItem(other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.createScheduleAt(f.ws.ID, other.ID, in, schedNow); !errors.Is(err, ErrScheduleItemGone) {
		t.Fatalf("archived item: %v, want ErrScheduleItemGone", err)
	}
	if _, err := f.s.createScheduleAt("not-this-workspace", f.item.ID, in, schedNow); !errors.Is(err, ErrScheduleItemGone) {
		t.Fatalf("foreign workspace: %v, want ErrScheduleItemGone", err)
	}
}

// Both rows or neither: a failure inserting the managed reminder leaves no
// schedule behind. SQLite only: the injected failure is a trigger.
func TestScheduleCreateIsAtomic(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	if f.s.dialect.Driver() != DriverSQLite {
		t.Skip("failure injected with a SQLite trigger")
	}
	f.exec(t, `CREATE TRIGGER fail_managed BEFORE INSERT ON item_reminders WHEN NEW.schedule_id IS NOT NULL
		BEGIN SELECT RAISE(ABORT, 'injected'); END`)
	_, err := f.s.createScheduleAt(f.ws.ID, f.item.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Tz: "UTC"}, schedNow)
	if err == nil {
		t.Fatal("create succeeded through an injected failure")
	}
	if n := f.count(t, `SELECT COUNT(*) FROM item_schedules`); n != 0 {
		t.Fatalf("schedule row survived a failed reminder insert: %d", n)
	}
}

func TestScheduleUpdateTimingEditRearmsAFiredRow(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})
	rid := *sc.ReminderID
	f.exec(t, `UPDATE item_reminders SET fired_at = ?, acked_at = ? WHERE id = ?`, "2026-10-30T09:00:00Z", "2026-10-30T09:05:00Z", rid)
	f.exec(t, `UPDATE item_schedules SET next_override_at = ?, anchor_wall = ?, error_code = ? WHERE id = ?`,
		"2026-10-30T20:00:00Z", "2026-10-30T09:00:00", "range", sc.ID)

	rule := "FREQ=WEEKLY"
	got := f.update(t, models.ScheduleUpdate{Rrule: &rule})
	if got.Rrule == nil || *got.Rrule != "FREQ=WEEKLY" {
		t.Fatalf("rrule = %v", got.Rrule)
	}
	if got.NextOverrideAt != nil || got.AnchorWall != nil || got.ErrorCode != nil {
		t.Fatalf("a timing edit must clear override, anchor and error: %+v", got)
	}
	if got.ReminderID == nil || *got.ReminderID != rid {
		t.Fatalf("the managed row must be re-armed in place: %v vs %s", got.ReminderID, rid)
	}
	r := f.managed(t, got)
	if r.FiredAt != nil || r.AckedAt != nil || r.RemindAt != "2026-10-30T09:00:00Z" {
		t.Fatalf("not re-armed: %+v", r)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM item_reminders WHERE schedule_id = ?`, sc.ID); n != 1 {
		t.Fatalf("managed rows = %d", n)
	}
}

func TestScheduleUpdateNonTimingKeepsTheOutstandingOccurrence(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})
	f.exec(t, `UPDATE item_reminders SET fired_at = ? WHERE id = ?`, "2026-10-30T09:00:00Z", *sc.ReminderID)
	f.exec(t, `UPDATE item_schedules SET next_override_at = ?, anchor_wall = ? WHERE id = ?`, "2026-10-30T20:00:00Z", "2026-10-30T09:00:00", sc.ID)

	kind, aud, lead := "deadline_soft", "agent", 15
	got := f.update(t, models.ScheduleUpdate{Kind: &kind, Audience: &aud, LeadMinutes: &lead})
	if got.Kind != kind || got.Audience != aud || got.LeadMinutes != lead {
		t.Fatalf("not applied: %+v", got)
	}
	if got.NextOverrideAt == nil || got.AnchorWall == nil || got.NextWall == nil || *got.NextWall != "2026-10-30T09:00:00" {
		t.Fatalf("outstanding occurrence disturbed: %+v", got)
	}
	r := f.managed(t, got)
	if r.FiredAt == nil || r.RemindAt != "2026-10-30T09:00:00Z" {
		t.Fatalf("a fired row must be left alone: %+v", r)
	}
}

func TestScheduleUpdateLeadMovesAnArmedRowOnly(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "UTC"})
	lead := 60
	got := f.update(t, models.ScheduleUpdate{LeadMinutes: &lead})
	if r := f.managed(t, got); r.RemindAt != "2026-10-30T08:00:00Z" || r.FiredAt != nil {
		t.Fatalf("armed row = %+v", r)
	}
	// An override replaces the base instant before the lead comes off.
	f.exec(t, `UPDATE item_schedules SET next_override_at = ? WHERE id = ?`, "2026-10-30T15:00:00Z", sc.ID)
	lead = 30
	got = f.update(t, models.ScheduleUpdate{LeadMinutes: &lead})
	if r := f.managed(t, got); r.RemindAt != "2026-10-30T14:30:00Z" {
		t.Fatalf("override ignored: %s", r.RemindAt)
	}
}

func TestScheduleUpdateExhaustingEndsAndAReviveReArms(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "UTC"})
	past := "2026-01-01T09:00"
	ended := f.update(t, models.ScheduleUpdate{Dtstart: &past})
	if ended.EndedAt == nil || ended.NextAt != nil || ended.NextWall != nil || ended.ReminderID != nil {
		t.Fatalf("not ended: %+v", ended)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM item_reminders`); n != 0 {
		t.Fatalf("managed row survived the end: %d", n)
	}
	// A kind edit on an ended schedule leaves it ended.
	kind := "deadline_soft"
	if got := f.update(t, models.ScheduleUpdate{Kind: &kind}); got.EndedAt == nil || got.ReminderID != nil {
		t.Fatalf("non-timing edit revived an ended schedule: %+v", got)
	}
	future := "2026-12-01T09:00"
	revived := f.update(t, models.ScheduleUpdate{Dtstart: &future})
	if revived.EndedAt != nil || revived.NextAt == nil || revived.ReminderID == nil {
		t.Fatalf("not revived: %+v", revived)
	}
}

func TestScheduleUpdateClearsErrorOnlyOnATimingEditThatCompiles(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})
	f.exec(t, `UPDATE item_schedules SET error_code = ? WHERE id = ?`, "cap_scan", sc.ID)

	lead := 5
	if got := f.update(t, models.ScheduleUpdate{LeadMinutes: &lead}); got.ErrorCode == nil || *got.ErrorCode != "cap_scan" {
		t.Fatalf("a non-timing edit must not clear error_code: %v", got.ErrorCode)
	}
	bad := "FREQ=DAILY;BOGUS=1"
	if _, err := f.s.updateScheduleAt(f.ws.ID, f.item.ID, models.ScheduleUpdate{Rrule: &bad}, schedNow); err == nil {
		t.Fatal("an uncompilable rule was accepted")
	}
	if got, _ := f.s.GetSchedule(f.ws.ID, f.item.ID); got.ErrorCode == nil {
		t.Fatal("a refused edit cleared error_code")
	}
	// Re-sending the same rule counts as a timing edit while the schedule is broken.
	same := "FREQ=DAILY"
	if got := f.update(t, models.ScheduleUpdate{Rrule: &same}); got.ErrorCode != nil {
		t.Fatalf("error_code survived a compiling timing edit: %v", *got.ErrorCode)
	}
}

func TestScheduleUpdateAndDeleteOfNone(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	k := "check"
	if got, err := f.s.UpdateSchedule(f.ws.ID, f.item.ID, models.ScheduleUpdate{Kind: &k}); got != nil || err != nil {
		t.Fatalf("update of none = %v %v", got, err)
	}
	if ok, err := f.s.DeleteSchedule(f.ws.ID, f.item.ID); ok || err != nil {
		t.Fatalf("delete of none = %v %v", ok, err)
	}
}

func TestScheduleDeleteRemovesRowsAndOccurrences(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Tz: "UTC"})
	f.exec(t, `INSERT INTO schedule_occurrences (id, schedule_id, occurrence_at, occurrence_wall, state, resolved_at)
		VALUES (?, ?, ?, ?, 'done', ?)`, newID(), sc.ID, "2026-10-01T09:00:00Z", "2026-10-01T09:00:00", now())
	ok, err := f.s.DeleteSchedule(f.ws.ID, f.item.ID)
	if err != nil || !ok {
		t.Fatalf("DeleteSchedule = %v %v", ok, err)
	}
	for _, tbl := range []string{"item_schedules", "item_reminders", "schedule_occurrences"} {
		if n := f.count(t, `SELECT COUNT(*) FROM `+tbl); n != 0 {
			t.Errorf("%s has %d rows after delete", tbl, n)
		}
	}
}

// Hard-deleting the item cascades through the composite key to the schedule,
// its managed reminder and its occurrences.
func TestScheduleCascadeOnItemHardDelete(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Tz: "UTC"})
	f.exec(t, `INSERT INTO schedule_occurrences (id, schedule_id, occurrence_at, occurrence_wall, state, resolved_at)
		VALUES (?, ?, ?, ?, 'done', ?)`, newID(), sc.ID, "2026-10-01T09:00:00Z", "2026-10-01T09:00:00", now())
	f.exec(t, `DELETE FROM items WHERE id = ?`, f.item.ID)
	for _, tbl := range []string{"item_schedules", "item_reminders", "schedule_occurrences"} {
		if n := f.count(t, `SELECT COUNT(*) FROM `+tbl); n != 0 {
			t.Errorf("%s has %d rows after the item was deleted", tbl, n)
		}
	}
}

// Archiving keeps the rows; the tick's reminderFireable already gates the
// managed row, so nothing fires while the item is archived.
func TestScheduleSurvivesArchive(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Tz: "UTC"})
	if err := f.s.DeleteItem(f.item.ID); err != nil {
		t.Fatal(err)
	}
	if sc, _ := f.s.GetSchedule(f.ws.ID, f.item.ID); sc == nil || sc.ReminderID == nil {
		t.Fatalf("archive dropped the schedule: %+v", sc)
	}
	ids, err := f.s.dueReminderCandidates("2099-01-01T00:00:00Z", 0)
	if err != nil || len(ids) != 0 {
		t.Fatalf("an archived item's managed row is a candidate: %v %v", ids, err)
	}
}

func TestManagedReminderRefusesDirectStoreMutation(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Tz: "UTC"})
	rid := *sc.ReminderID
	f.exec(t, `UPDATE item_reminders SET fired_at = ? WHERE id = ?`, "2026-11-02T09:00:00Z", rid)
	before := f.managed(t, sc)

	if _, err := f.s.RearmReminder(f.ws.ID, rid, "2030-01-01T00:00:00Z"); !errors.Is(err, ErrReminderManaged) {
		t.Errorf("rearm: %v, want ErrReminderManaged", err)
	}
	if _, err := f.s.AckReminder(f.ws.ID, rid); !errors.Is(err, ErrReminderManaged) {
		t.Errorf("ack: %v, want ErrReminderManaged", err)
	}
	if ok, err := f.s.DeleteReminder(f.ws.ID, rid); !errors.Is(err, ErrReminderManaged) || ok {
		t.Errorf("delete: %v %v, want ErrReminderManaged", ok, err)
	}
	after := f.managed(t, sc)
	if *before != *after && (before.RemindAt != after.RemindAt || after.AckedAt != nil || after.FiredAt == nil) {
		t.Fatalf("a refused write changed the row: %+v -> %+v", before, after)
	}
}

// The unmanaged answers are unchanged: not found and not-yet-fired are still
// (nil, nil), never ErrReminderManaged.
func TestUnmanagedReminderWritesAreUnchanged(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	id := armReminder(t, f.s, f.ws.ID, f.item.ID, future)
	if r, err := f.s.AckReminder(f.ws.ID, id); r != nil || err != nil {
		t.Fatalf("ack of an armed reminder = %v %v", r, err)
	}
	if r, err := f.s.RearmReminder(f.ws.ID, "missing", future); r != nil || err != nil {
		t.Fatalf("rearm of a missing reminder = %v %v", r, err)
	}
	if r, err := f.s.RearmReminder(f.ws.ID, id, past); err != nil || r == nil {
		t.Fatalf("rearm of an unmanaged reminder = %v %v", r, err)
	}
	if ok, err := f.s.DeleteReminder(f.ws.ID, id); !ok || err != nil {
		t.Fatalf("delete of an unmanaged reminder = %v %v", ok, err)
	}
	if ok, err := f.s.DeleteReminder(f.ws.ID, id); ok || err != nil {
		t.Fatalf("second delete = %v %v", ok, err)
	}
}

func TestExportSkipsManagedReminders(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Tz: "UTC"})
	other := f.another(t, "plain")
	armReminder(t, f.s, f.ws.ID, other.ID, future)
	exp, err := f.s.ExportWorkspace(f.ws.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if len(exp.Reminders) != 1 || exp.Reminders[0].ItemID != other.ID {
		t.Fatalf("export reminders = %+v, want only the unmanaged one", exp.Reminders)
	}
}

// The occurrence identity is the UTC instant, not the wall: a bare HOURLY rule
// repeats a wall across a fall-back. An all-day date is an identity too.
func TestScheduleOccurrencesKeyedOnAt(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Tz: "America/New_York", Rrule: "FREQ=HOURLY"})
	ins := func(at, wall string) error {
		_, err := f.s.db.Exec(f.s.q(`INSERT INTO schedule_occurrences (id, schedule_id, occurrence_at, occurrence_wall, state, resolved_at)
			VALUES (?, ?, ?, ?, 'done', ?)`), newID(), sc.ID, at, wall, now())
		return err
	}
	if err := ins("2026-11-01T05:00:00Z", "2026-11-01T01:00:00"); err != nil {
		t.Fatal(err)
	}
	if err := ins("2026-11-01T06:00:00Z", "2026-11-01T01:00:00"); err != nil {
		t.Fatalf("same wall, different instant must be accepted: %v", err)
	}
	if err := ins("2026-11-01T06:00:00Z", "2026-11-01T01:00:00"); !IsUniqueViolation(err) {
		t.Fatalf("same instant twice: %v, want a unique violation", err)
	}
	if err := ins("2026-11-05T05:00:00Z", "2026-11-05"); err != nil {
		t.Fatal(err)
	}
	if err := ins("2026-11-05T09:00:00Z", "2026-11-05"); !IsUniqueViolation(err) {
		t.Fatalf("same all-day date at another instant: %v, want a unique violation", err)
	}
}

func TestFireInstant(t *testing.T) {
	t.Parallel()
	ny, _ := time.LoadLocation("America/New_York")
	timed, _ := schedule.ParseWall("2026-11-02T09:00")
	allDay, _ := schedule.ParseWall("2026-11-02")
	at := time.Date(2026, 11, 2, 14, 0, 0, 0, time.UTC)
	override := time.Date(2026, 11, 3, 18, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		got  time.Time
		want time.Time
	}{
		{"timed", fireInstant(at, timed, ny, 0, nil), at},
		{"timed with lead", fireInstant(at, timed, ny, 90, nil), at.Add(-90 * time.Minute)},
		{"all-day reads 09:00 local, not midnight", fireInstant(time.Date(2026, 11, 2, 5, 0, 0, 0, time.UTC), allDay, ny, 0, nil), at},
		{"override replaces the base, then the lead", fireInstant(at, timed, ny, 60, &override), override.Add(-time.Hour)},
	}
	for _, tc := range cases {
		if !tc.got.Equal(tc.want) {
			t.Errorf("%s: %s, want %s", tc.name, tc.got, tc.want)
		}
	}
}

func TestScheduleErrorCode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  error
		want string
	}{
		{&schedule.MalformedError{Code: schedule.CodeRange}, "range"},
		{&schedule.CapError{Kind: schedule.CapScan}, "cap_scan"},
		{&schedule.CapError{Kind: schedule.CapStall}, "cap_stall"},
		{&schedule.CapError{Kind: schedule.CapTime}, "cap_time"},
		{errors.New("boom with third-party text"), ""},
		{nil, ""},
	} {
		if got := ScheduleErrorCode(tc.err); got != tc.want {
			t.Errorf("ScheduleErrorCode(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
