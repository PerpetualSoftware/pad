package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// TASK-4236 (calendar P0 F): the firing floor and the RDATE/EXDATE limits at
// create and update, and catch-up for a rule at the floor. Runs on SQLite in
// make test and on Postgres in make test-pg.

const floorMessage = "schedules fire at most every 15 minutes"

// hourWalls returns n walls an hour apart from 2031-01-01T00:00, so the dates
// themselves never fall inside the 15-minute floor.
func hourWalls(n int) []string {
	base := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	out := make([]string, n)
	for i := range out {
		out[i] = base.Add(time.Duration(i) * time.Hour).Format("2006-01-02T15:04")
	}
	return out
}

// seedRule stores rule on a schedule directly, past the firing floor that
// create and update enforce: the way a test models a schedule stored before the
// floor, or one a stored-state bug could leave behind.
func (f *schedFixture) seedRule(t *testing.T, scheduleID, rule string) {
	t.Helper()
	f.exec(t, `UPDATE item_schedules SET rrule = ? WHERE id = ?`, rule, scheduleID)
}

func (f *schedFixture) tryCreate(t *testing.T, in models.ScheduleCreate) error {
	t.Helper()
	_, err := f.s.createScheduleAt(f.ws.ID, f.another(t, "Floor "+t.Name()).ID, in, schedNow)
	return err
}

// A schedule that fires more often than every 15 minutes is refused, never
// thinned. The interval is the shortest gap between two consecutive instants of
// the rule plus its RDATEs.
func TestScheduleFloorRefusesSubFifteenMinuteSchedules(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	base := models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-10T09:00", Tz: "UTC"}
	with := func(mut func(*models.ScheduleCreate)) models.ScheduleCreate {
		in := base
		mut(&in)
		return in
	}
	for _, tc := range []struct {
		name    string
		in      models.ScheduleCreate
		refused bool
	}{
		{"minutely every 5", with(func(c *models.ScheduleCreate) { c.Rrule = "FREQ=MINUTELY;INTERVAL=5" }), true},
		{"minutely every 14", with(func(c *models.ScheduleCreate) { c.Rrule = "FREQ=MINUTELY;INTERVAL=14" }), true},
		{"minutely every minute", with(func(c *models.ScheduleCreate) { c.Rrule = "FREQ=MINUTELY" }), true},
		{"minutely every 15", with(func(c *models.ScheduleCreate) { c.Rrule = "FREQ=MINUTELY;INTERVAL=15" }), false},
		{"minutely every 90", with(func(c *models.ScheduleCreate) { c.Rrule = "FREQ=MINUTELY;INTERVAL=90" }), false},
		{"hourly", with(func(c *models.ScheduleCreate) { c.Rrule = "FREQ=HOURLY" }), false},
		{"daily with five minute byminute", with(func(c *models.ScheduleCreate) { c.Rrule = "FREQ=DAILY;BYHOUR=9;BYMINUTE=0,5" }), true},
		{"daily with fifteen minute byminute", with(func(c *models.ScheduleCreate) { c.Rrule = "FREQ=DAILY;BYHOUR=9;BYMINUTE=0,15,30,45" }), false},
		{"rdate five minutes after a rule instant", with(func(c *models.ScheduleCreate) {
			c.Rrule, c.RDates = "FREQ=DAILY", []string{"2031-03-10T09:05"}
		}), true},
		{"rdate fifteen minutes after a rule instant", with(func(c *models.ScheduleCreate) {
			c.Rrule, c.RDates = "FREQ=DAILY", []string{"2031-03-10T09:15"}
		}), false},
		{"two rdates five minutes apart", with(func(c *models.ScheduleCreate) {
			c.RDates = []string{"2031-03-11T09:00", "2031-03-11T09:05"}
		}), true},
		{"one-shot", base, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := f.tryCreate(t, tc.in)
			switch {
			case tc.refused && err == nil:
				t.Fatalf("created; want a refusal naming the floor")
			case tc.refused && !strings.Contains(err.Error(), floorMessage):
				t.Fatalf("refusal = %v; want it to say %q", err, floorMessage)
			case !tc.refused && err != nil:
				t.Fatalf("refused: %v", err)
			}
		})
	}
}

// The floor applies to an update too, and a refused update leaves the stored
// schedule as it was.
func TestScheduleFloorAppliesToUpdate(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-10T09:00", Tz: "UTC", Rrule: "FREQ=HOURLY"})
	bad := "FREQ=MINUTELY;INTERVAL=5"
	_, err := f.s.updateScheduleAt(f.ws.ID, f.item.ID, models.ScheduleUpdate{Rrule: &bad}, schedNow)
	if err == nil || !strings.Contains(err.Error(), floorMessage) {
		t.Fatalf("update to every 5 minutes = %v; want a refusal saying %q", err, floorMessage)
	}
	if got := f.reload(t, f.item.ID); got.Rrule == nil || *got.Rrule != *sc.Rrule {
		t.Fatalf("a refused update changed the rule: %v", got.Rrule)
	}
	ok := "FREQ=MINUTELY;INTERVAL=15"
	if _, err := f.s.updateScheduleAt(f.ws.ID, f.item.ID, models.ScheduleUpdate{Rrule: &ok}, schedNow); err != nil {
		t.Fatalf("update to every 15 minutes: %v", err)
	}
}

// At most 1000 RDATE and 1000 EXDATE entries per schedule, refused with a
// MalformedError that names the part.
func TestScheduleRDateAndExDateLimits(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	for _, part := range []string{"RDATE", "EXDATE"} {
		t.Run(part, func(t *testing.T) {
			mk := func(n int) models.ScheduleCreate {
				in := models.ScheduleCreate{Kind: "check", Dtstart: "2031-01-01T00:00", Tz: "UTC", Rrule: "FREQ=HOURLY"}
				if part == "RDATE" {
					in.RDates = hourWalls(n)
				} else {
					in.ExDates = hourWalls(n)
				}
				return in
			}
			if err := f.tryCreate(t, mk(1000)); err != nil {
				t.Fatalf("1000 %ss refused: %v", part, err)
			}
			err := f.tryCreate(t, mk(1001))
			var m *schedule.MalformedError
			if !errors.As(err, &m) {
				t.Fatalf("1001 %ss = %v; want a *schedule.MalformedError", part, err)
			}
			if !strings.EqualFold(m.Part, part) {
				t.Errorf("MalformedError.Part = %q; want it to name %s", m.Part, part)
			}
		})
	}
}

// Catch-up after downtime fires once per schedule, never once per missed
// instant, including a rule at the floor (a 15-minute rule down for 3 hours has
// 12 missed instants and one outbox event).
func TestTickCatchUpAfterDowntimeAtTheFloor(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-29T00:00", Tz: "UTC", Rrule: "FREQ=MINUTELY;INTERVAL=15"})
	f.force(t, sc, "2026-10-29T09:00:00", "2026-10-29T09:00:00Z", "2026-10-29T09:00:00Z")

	fired := f.fire(t, "2026-10-29T12:00:00Z")
	if len(fired) != 1 {
		t.Fatalf("fired %d after 3h of downtime, want exactly 1", len(fired))
	}
	if got := *fired[0].OccurrenceAt; got != "2026-10-29T09:00:00Z" {
		t.Errorf("occurrence_at = %s; want the stale occurrence, not now", got)
	}
	if again := f.fire(t, fmt.Sprintf("2026-10-29T12:00:%02dZ", 30)); len(again) != 0 {
		t.Errorf("second tick fired %d", len(again))
	}
	if n := f.events(t); n != 1 {
		t.Errorf("outbox events = %d, want 1", n)
	}
}

// A rule that never occurs is refused at create, and a rare one that does occur
// is not (TASK-4236). The refusals come from Compile's satisfiability check and
// from the create path's exhausted check; this pins which rows each way.
func TestScheduleCreateNeverOccursAndRareRules(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	for _, tc := range []struct {
		name, start, rule string
		refused           bool
	}{
		{"feb 29 from a non-leap dtstart", "2031-03-10T09:00", "FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=29", false},
		{"feb 29 from dtstart in 2101", "2101-03-10T09:00", "FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=29", false},
		{"feb 29 on a monday", "2031-03-10T09:00", "FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=29;BYDAY=MO", false},
		{"friday the 13th", "2031-03-10T09:00", "FREQ=MONTHLY;BYDAY=FR;BYMONTHDAY=13", false},
		{"second to last weekday", "2031-03-10T09:00", "FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-2", false},
		{"feb 30", "2031-03-10T09:00", "FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=30", true},
		{"apr 31", "2031-03-10T09:00", "FREQ=YEARLY;BYMONTH=4;BYMONTHDAY=31", true},
		{"week 53 in february", "2031-03-10T09:00", "FREQ=YEARLY;BYWEEKNO=53;BYDAY=MO;BYMONTH=2", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := f.tryCreate(t, models.ScheduleCreate{Kind: "check", Dtstart: tc.start, Tz: "UTC", Rrule: tc.rule})
			if tc.refused != (err != nil) {
				t.Fatalf("refused = %v (%v); want refused = %v", err != nil, err, tc.refused)
			}
		})
	}
}

// An RDATE far past the rule's first few thousand occurrences can still sit
// inside the floor of a rule instant: every RDATE is checked against its
// neighbours, not only the ones near dtstart. The refusal names rdates because
// the rule alone is fine.
func TestScheduleFloorSeesAnRDateBeyondTheRuleSample(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	err := f.tryCreate(t, models.ScheduleCreate{Kind: "check", Dtstart: "2031-01-01T00:00", Tz: "UTC", Rrule: "FREQ=HOURLY",
		RDates: []string{"2031-06-01T10:05"}}) // 5 minutes after the 10:00 rule instant, about 3,700 hours in
	var fe *ScheduleFieldError
	if !errors.As(err, &fe) || fe.Code != "floor" || fe.Field != "rdates" || !strings.Contains(err.Error(), floorMessage) {
		t.Fatalf("err = %v; want the floor naming rdates", err)
	}
	// An RDATE a full 15 minutes off is fine, far out as well.
	if err := f.tryCreate(t, models.ScheduleCreate{Kind: "check", Dtstart: "2031-01-01T00:00", Tz: "UTC", Rrule: "FREQ=HOURLY",
		RDates: []string{"2031-06-01T10:15"}}); err != nil {
		t.Fatalf("an RDATE 15 minutes off a rule instant was refused: %v", err)
	}
	// When the rule itself is under the floor, the refusal names rrule.
	err = f.tryCreate(t, models.ScheduleCreate{Kind: "check", Dtstart: "2031-01-01T00:00", Tz: "UTC", Rrule: "FREQ=MINUTELY;INTERVAL=5",
		RDates: []string{"2031-06-01T10:05"}})
	if !errors.As(err, &fe) || fe.Field != "rrule" {
		t.Fatalf("err = %v; want the floor naming rrule", err)
	}
}

// tryUpdate applies a PATCH to the fixture's schedule and returns the store's answer.
func (f *schedFixture) tryUpdate(t *testing.T, upd models.ScheduleUpdate) (*models.ItemSchedule, error) {
	t.Helper()
	return f.s.updateScheduleAt(f.ws.ID, f.item.ID, upd, schedNow)
}

// TASK-4236, through PATCH. A rule with no occurrence from
// its dtstart is refused with "this schedule never occurs"; a cap hit with
// nothing found keeps its own cap error; a rule whose occurrences have all
// passed stays valid and ends the schedule.
func TestScheduleUpdateNoOccurrenceRows(t *testing.T) {
	t.Parallel()
	sp := func(s string) *string { return &s }
	const never = "this schedule never occurs"
	for _, tc := range []struct {
		name    string
		start   string
		upd     models.ScheduleUpdate
		wantErr string // substring; "" accepts
		cap     bool   // a *schedule.CapError, not the never-occurs message
		ends    bool
	}{
		{name: "feb 30", upd: models.ScheduleUpdate{Rrule: sp("FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=30")}, wantErr: "never produces an occurrence"},
		{name: "every occurrence excluded", upd: models.ScheduleUpdate{Rrule: sp("FREQ=DAILY;COUNT=2"), ExDates: &[]string{"2031-03-10T09:00", "2031-03-11T09:00"}}, wantErr: never},
		{name: "week 53 in february", upd: models.ScheduleUpdate{Rrule: sp("FREQ=YEARLY;BYWEEKNO=53;BYDAY=MO;BYMONTH=2")}, wantErr: never},
		{name: "cap hit with nothing found", upd: models.ScheduleUpdate{Dtstart: sp("2000-01-01T09:00"), Rrule: sp("FREQ=MINUTELY;BYHOUR=9;BYMINUTE=0")}, cap: true},
		{name: "feb 29 from a non-leap dtstart", upd: models.ScheduleUpdate{Rrule: sp("FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=29")}},
		{name: "until moved into the past ends the schedule", start: "2020-01-01T09:00", upd: models.ScheduleUpdate{Rrule: sp("FREQ=DAILY;UNTIL=20200601T000000Z")}, ends: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSchedFixture(t)
			start := tc.start
			if start == "" {
				start = "2031-03-10T09:00"
			}
			f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: start, Tz: "UTC", Rrule: "FREQ=DAILY"})
			before := f.reload(t, f.item.ID)
			sc, err := f.tryUpdate(t, tc.upd)
			var capErr *schedule.CapError
			switch {
			case tc.cap:
				if !errors.As(err, &capErr) || capErr.Kind != schedule.CapScan {
					t.Fatalf("err = %v; want a scan cap, not the never-occurs message", err)
				}
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v; want %q", err, tc.wantErr)
				}
			case err != nil:
				t.Fatalf("refused: %v", err)
			case tc.ends && sc.EndedAt == nil:
				t.Fatalf("schedule did not end: %+v", sc)
			}
			if err != nil {
				if got := f.reload(t, f.item.ID); got.Rrule == nil || *got.Rrule != *before.Rrule {
					t.Errorf("a refused update changed the rule: %v", got.Rrule)
				}
			}
		})
	}
}

// An existing schedule whose COUNT has run out stays valid: a title-level edit
// (no timing field) saves, and the checks do not run.
func TestScheduleUpdateOfAnExhaustedRuleSavesANonTimingEdit(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-10T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})
	f.exec(t, `UPDATE item_schedules SET dtstart = '2020-01-01T09:00', rrule = 'FREQ=DAILY;COUNT=2', ended_at = '2020-01-03T00:00:00Z', next_at = NULL, next_wall = NULL WHERE id = ?`, sc.ID)
	kind := "deadline_soft"
	got, err := f.tryUpdate(t, models.ScheduleUpdate{Kind: &kind})
	if err != nil || got.Kind != kind {
		t.Fatalf("a non-timing edit of an exhausted schedule: %+v %v", got, err)
	}
}

// Create keeps its documented refusal of a rule whose occurrences have all
// passed, with the exhausted message and not the never-occurs one.
func TestScheduleCreateOfAPassedRuleKeepsItsRefusal(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	err := f.tryCreate(t, models.ScheduleCreate{Kind: "check", Dtstart: "2020-03-10T09:00", Tz: "UTC", Rrule: "FREQ=DAILY;COUNT=3"})
	if err == nil || strings.Contains(err.Error(), "never occurs") {
		t.Fatalf("err = %v; want the exhausted refusal", err)
	}
}

// Copy, import and restore never run the floor or the no-occurrence check: a
// row stored under the floor, or one that never occurs, is copied as before.
func TestCopyIgnoresTheFloorAndTheNoOccurrenceCheck(t *testing.T) {
	t.Parallel()
	for _, rule := range []string{"FREQ=MINUTELY;INTERVAL=5", "FREQ=YEARLY;BYWEEKNO=53;BYDAY=MO;BYMONTH=2"} {
		t.Run(rule, func(t *testing.T) {
			c := newSchedCopy(t)
			sc := c.scheduleFuture(t, daily())
			c.exec(t, `UPDATE item_schedules SET rrule = ? WHERE id = ?`, rule, sc.ID)
			res := c.do(t, false)
			if res == nil || res.Item == nil {
				t.Fatalf("the item was not copied: %+v", res)
			}
			if s := res.Schedule.Skipped; strings.Contains(s, "15 minutes") || strings.Contains(s, "never occurs") {
				t.Errorf("copy ran a write-path check: skipped = %q", s)
			}
		})
	}
}

// The floor names rdates when the RDATEs close the gap, even when the rule
// alone cannot be measured (a rule that never fires needs an RDATE to compile).
func TestScheduleFloorNamesRDatesWhenTheRuleAloneCannotBeMeasured(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	err := f.tryCreate(t, models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-10T09:00", Tz: "UTC",
		Rrule: "FREQ=MINUTELY;BYMONTH=1;BYYEARDAY=200", RDates: []string{"2031-03-10T09:00", "2031-03-10T09:05"}})
	var fe *ScheduleFieldError
	if !errors.As(err, &fe) || fe.Code != "floor" || fe.Field != "rdates" {
		t.Fatalf("err = %v; want the floor naming rdates", err)
	}
}

// The checks re-run only when the rule, dtstart, RDATEs or EXDATEs change. A
// tz-only or anchor-only PATCH of a row stored under the floor, or one that
// never occurs, saves (TASK-4236).
func TestScheduleUpdateOfLegacyRowsOnlyRechecksTheRuleFields(t *testing.T) {
	t.Parallel()
	for _, rule := range []string{"FREQ=MINUTELY;INTERVAL=5", "FREQ=YEARLY;BYWEEKNO=53;BYDAY=MO;BYMONTH=2"} {
		t.Run(rule, func(t *testing.T) {
			f := newSchedFixture(t)
			sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-10T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})
			f.seedRule(t, sc.ID, rule)
			tz := "America/New_York"
			if _, err := f.tryUpdate(t, models.ScheduleUpdate{Tz: &tz}); err != nil {
				t.Errorf("a tz-only PATCH of a legacy row was refused: %v", err)
			}
			// The row is handled cleanly after the gate is skipped: a never-occurring
			// one ends (or carries an error_code); it does not stay armed on nothing.
			got := f.reload(t, f.item.ID)
			if strings.Contains(rule, "BYWEEKNO") && got.EndedAt == nil && got.ErrorCode == nil {
				t.Errorf("a never-occurring legacy row stayed armed after a tz PATCH: %+v", got)
			}
			// Naming the rule again does re-run the checks, and it is the check that refuses.
			want := "floor"
			if strings.Contains(rule, "BYWEEKNO") {
				want = "never_occurs"
			}
			_, err := f.tryUpdate(t, models.ScheduleUpdate{Rrule: &rule})
			var fe *ScheduleFieldError
			if !errors.As(err, &fe) || fe.Code != want {
				t.Errorf("re-sending the rule %q = %v; want the %s check", rule, err, want)
			}
		})
	}
}

// A cap on an RDATE window is never a pass. The scan cap is deterministic, so it
// is the cap's own refusal; the work budget depends on load, so it is a
// CapTime (the HTTP edge answers 503 schedule_busy) and nothing is written.
func TestScheduleRDateWindowScanCapIsRefused(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	// Daily at 09:00 written as MINUTELY (its gap is a day), with an RDATE in 2100:
	// past the sampled occurrences and the 2,000,000 minute-period scan limit.
	err := f.tryCreate(t, models.ScheduleCreate{Kind: "check", Dtstart: "2031-01-01T09:00", Tz: "UTC",
		Rrule: "FREQ=MINUTELY;BYHOUR=9;BYMINUTE=0", RDates: []string{"2100-06-01T09:05"}})
	var capErr *schedule.CapError
	if !errors.As(err, &capErr) || capErr.Kind != schedule.CapScan {
		t.Fatalf("err = %v; want a scan cap", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM item_schedules WHERE workspace_id = ?`, f.ws.ID); n != 0 {
		t.Errorf("%d schedules stored after a refusal", n)
	}
}

func TestScheduleRDateWorkBudgetIsBusyNotInvalid(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	// RDATEs two years out, past the 10000 occurrences the sample reads, so each
	// needs a window of its own.
	farRDates := []string{"2033-01-01T10:15", "2033-01-01T11:15", "2033-01-01T12:15"}
	f.s.scheduleWriteLimits = schedule.Limits{MaxGapWork: time.Nanosecond}
	err := f.tryCreate(t, models.ScheduleCreate{Kind: "check", Dtstart: "2031-01-01T00:00", Tz: "UTC", Rrule: "FREQ=HOURLY",
		RDates: farRDates})
	var capErr *schedule.CapError
	if !errors.As(err, &capErr) || capErr.Kind != schedule.CapTime {
		t.Fatalf("err = %v; want CapTime (503 schedule_busy at the edge), not a validation error", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM item_schedules WHERE workspace_id = ?`, f.ws.ID); n != 0 {
		t.Errorf("%d schedules stored after a busy refusal", n)
	}
	// The same input on a host with the default budget is accepted.
	f.s.scheduleWriteLimits = schedule.Limits{}
	if err := f.tryCreate(t, models.ScheduleCreate{Kind: "check", Dtstart: "2031-01-01T00:00", Tz: "UTC", Rrule: "FREQ=HOURLY",
		RDates: farRDates}); err != nil {
		t.Fatalf("the same schedule with the default budget: %v", err)
	}
}

// hourWallsOffset is n walls an hour apart from 2031-01-01T00:00, each off the
// hour by the given minutes.
func hourWallsOffset(n, minutes int) []string {
	base := time.Date(2031, 1, 1, 0, minutes, 0, 0, time.UTC)
	out := make([]string, n)
	for i := range out {
		out[i] = base.Add(time.Duration(i) * time.Hour).Format("2006-01-02T15:04")
	}
	return out
}

// A plain daily rule with an RDATE a century out is fine: the daily scan limit
// is thousands of years, and the walk is cheap. The time of the floor check is
// logged so the slowest accepted case can be read off an idle run (-v).
func TestScheduleFarRDateOnADailyRuleIsAccepted(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	for _, tc := range []struct {
		name string
		in   models.ScheduleCreate
	}{
		{"daily, rdate in 2150", models.ScheduleCreate{Kind: "check", Dtstart: "2031-01-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY", RDates: []string{"2150-01-01T09:15"}}},
		{"hourly, 1000 rdates 15 minutes off", models.ScheduleCreate{Kind: "check", Dtstart: "2031-01-01T00:00", Tz: "UTC", Rrule: "FREQ=HOURLY", RDates: hourWallsOffset(1000, 15)}},
		{"daily new york, 1000 rdates", models.ScheduleCreate{Kind: "check", Dtstart: "2031-01-01T09:00", Tz: "America/New_York", Rrule: "FREQ=DAILY", RDates: hourWallsOffset(1000, 30)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := compileScheduleSpec(scheduleSpecInput{Dtstart: tc.in.Dtstart, Tz: tc.in.Tz, Rrule: tc.in.Rrule, RDates: tc.in.RDates})
			if err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			err = f.s.checkScheduleTiming(c)
			t.Logf("floor check %s: %v", tc.name, time.Since(started))
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
		})
	}
}

// The floor sees an RDATE one minute BEFORE a rule instant when it is the last
// RDATE and lies past the first 2000 occurrences.
func TestScheduleFloorSeesTheGapAfterTheLastRDate(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	err := f.tryCreate(t, models.ScheduleCreate{Kind: "check", Dtstart: "2031-01-01T00:00", Tz: "UTC", Rrule: "FREQ=HOURLY",
		RDates: []string{"2031-04-15T15:59"}})
	var fe *ScheduleFieldError
	if !errors.As(err, &fe) || fe.Code != "floor" || fe.Field != "rdates" {
		t.Fatalf("err = %v; want the floor naming rdates", err)
	}
}
