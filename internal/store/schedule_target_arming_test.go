package store

import (
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// TASK-4713: a managed reminder follows the occurrence's target (target minus
// the lead), not its start, on every writer of remind_at.

// targetCommit creates a native schedule on item with the given target offset,
// armed from the fixture clock.
func (f *schedFixture) targetCommit(t *testing.T, item *models.Item, in models.ScheduleCreate, days, secs int) *models.ItemSchedule {
	t.Helper()
	if in.Kind == "" {
		in.Kind = "check"
	}
	roles, err := rolesFromInput(models.CommitmentRolesInput{TargetDays: days, TargetSecs: secs})
	if err != nil {
		t.Fatal(err)
	}
	sc, err := f.s.createScheduleRolesAt(f.ws.ID, item.ID, in, &roles, schedNow)
	if err != nil {
		t.Fatalf("createScheduleRolesAt: %v", err)
	}
	return sc
}

// patchTarget is the roles PATCH naming only the target, with an optional lead.
func (f *schedFixture) patchTarget(t *testing.T, item *models.Item, days, secs int, lead *int, at string) *models.ItemSchedule {
	t.Helper()
	p := models.CommitmentRolesPatch{TargetDays: fullRolePatchValue(&days), TargetSecs: fullRolePatchValue(&secs)}
	sc, err := f.s.updateCommitmentAt(f.ws.ID, item.ID, models.ScheduleUpdate{LeadMinutes: lead}, &p, rt(at), "")
	if err != nil {
		t.Fatalf("updateCommitmentAt: %v", err)
	}
	return sc
}

func (f *schedFixture) remindAtOf(t *testing.T, sc *models.ItemSchedule) string {
	t.Helper()
	var at string
	if err := f.s.db.QueryRow(f.s.q(`SELECT remind_at FROM item_reminders WHERE schedule_id=?`), sc.ID).Scan(&at); err != nil {
		t.Fatalf("read remind_at: %v", err)
	}
	return at
}

func wantRemindAt(t *testing.T, f *schedFixture, sc *models.ItemSchedule, want, what string) {
	t.Helper()
	if got := f.remindAtOf(t, sc); got != want {
		t.Fatalf("%s: remind_at=%s, want %s", what, got, want)
	}
}

func TestCreateWithTargetOffsetArmsAtTarget(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	timed := f.targetCommit(t, f.item, models.ScheduleCreate{Dtstart: "2026-10-30T09:00", Tz: "UTC", Rrule: "FREQ=DAILY", LeadMinutes: 30}, 0, 7200)
	wantRemindAt(t, f, timed, "2026-10-30T10:30:00Z", "timed +2h lead 30")
	allDay := f.targetCommit(t, f.another(t, "All day"), models.ScheduleCreate{Dtstart: "2026-10-30", Tz: "UTC", Rrule: "FREQ=WEEKLY"}, 2, 0)
	wantRemindAt(t, f, allDay, "2026-11-01T09:00:00Z", "all-day +2d reads 09:00 of D+2")
	ny := f.targetCommit(t, f.another(t, "Earlier"), models.ScheduleCreate{Dtstart: "2026-10-30T09:00", Tz: "America/New_York", LeadMinutes: 60}, -1, 3*3600)
	// 2026-10-29 12:00 EDT is 16:00Z, less an hour.
	wantRemindAt(t, f, ny, "2026-10-29T15:00:00Z", "pinned -1d+3h")
}

func TestArmingAgreesWithAgendaTarget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		in         models.ScheduleCreate
		days, secs int
	}{
		{"timed New York +2h", models.ScheduleCreate{Dtstart: "2026-11-02T09:00", Tz: "America/New_York", Rrule: "FREQ=DAILY", LeadMinutes: 15}, 0, 7200},
		{"all-day +2d is date-only", models.ScheduleCreate{Dtstart: "2026-11-02", Tz: "Europe/London", Rrule: "FREQ=WEEKLY", LeadMinutes: 60}, 2, 0},
		{"all-day 10:00", models.ScheduleCreate{Dtstart: "2026-11-02", Tz: "America/New_York", LeadMinutes: 45}, 0, 36000},
		{"timed -1d", models.ScheduleCreate{Dtstart: "2026-11-03T08:00", Tz: "Asia/Tokyo", LeadMinutes: 5}, -1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newSchedFixture(t)
			sc := f.targetCommit(t, f.item, tc.in, tc.days, tc.secs)
			e, err := f.s.AgendaOutstanding(sc, time.UTC, schedNow)
			if err != nil || e == nil || e.Roles.Target == nil {
				t.Fatalf("outstanding: %+v %v", e, err)
			}
			loc, _ := time.LoadLocation(tc.in.Tz)
			var target time.Time
			if v := *e.Roles.Target; len(v) == 10 {
				w, err := schedule.ParseWall(v)
				if err != nil {
					t.Fatal(err)
				}
				target = wallInstant(loc, w) // a date-only target fires at the all-day hour
			} else if target, err = time.Parse(time.RFC3339, v); err != nil {
				t.Fatal(err)
			}
			want := NormalizeInstant(target.Add(-time.Duration(tc.in.LeadMinutes) * time.Minute))
			wantRemindAt(t, f, sc, want, "target "+*e.Roles.Target)
		})
	}
}

func TestRolesPatchRetimesArmedRow(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	daily := models.ScheduleCreate{Dtstart: "2026-10-30T09:00", Tz: "UTC", Rrule: "FREQ=DAILY", LeadMinutes: 30}
	sc := f.targetCommit(t, f.item, daily, 0, 0)
	wantRemindAt(t, f, sc, "2026-10-30T08:30:00Z", "zero offset")
	f.patchTarget(t, f.item, 0, 7200, nil, "2026-10-29T13:00:00Z")
	wantRemindAt(t, f, sc, "2026-10-30T10:30:00Z", "after the target PATCH")

	// A fired row has gone out and stays where it fired.
	other := f.another(t, "Fired")
	fired := f.targetCommit(t, other, daily, 0, 0)
	if got := f.fire(t, "2026-10-30T08:30:00Z"); len(got) != 1 || got[0].ScheduleID == nil || *got[0].ScheduleID != fired.ID {
		t.Fatalf("fired %+v", got)
	}
	before := f.managed(t, f.reload(t, other.ID))
	f.patchTarget(t, other, 0, 7200, nil, "2026-10-30T08:45:00Z")
	after := f.managed(t, f.reload(t, other.ID))
	if after.RemindAt != before.RemindAt || after.FiredAt == nil || *after.FiredAt != *before.FiredAt {
		t.Fatalf("a fired row moved: before %+v after %+v", before, after)
	}
}

func TestRolesPatchAndLeadChangeTogether(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.targetCommit(t, f.item, models.ScheduleCreate{Dtstart: "2026-10-30T09:00", Tz: "UTC", Rrule: "FREQ=DAILY", LeadMinutes: 30}, 0, 0)
	lead := 60
	f.patchTarget(t, f.item, 0, 7200, &lead, "2026-10-29T13:00:00Z")
	wantRemindAt(t, f, sc, "2026-10-30T10:00:00Z", "target and lead together")
	lead = 15
	if _, err := f.s.updateCommitmentAt(f.ws.ID, f.item.ID, models.ScheduleUpdate{LeadMinutes: &lead}, nil, rt("2026-10-29T13:05:00Z"), ""); err != nil {
		t.Fatal(err)
	}
	wantRemindAt(t, f, sc, "2026-10-30T10:45:00Z", "a lead-only edit keeps the offset")
}

func TestTimingEditHonoursStoredOffset(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.targetCommit(t, f.item, models.ScheduleCreate{Dtstart: "2026-10-30T09:00", Tz: "UTC", Rrule: "FREQ=DAILY", LeadMinutes: 30}, 0, 7200)
	start := "2026-10-30T11:00"
	if _, err := f.s.updateCommitmentAt(f.ws.ID, f.item.ID, models.ScheduleUpdate{Dtstart: &start}, nil, schedNow, ""); err != nil {
		t.Fatal(err)
	}
	wantRemindAt(t, f, sc, "2026-10-30T12:30:00Z", "timing edit with the stored offset")
	// A timing edit and a target PATCH together arm at the merged target.
	start = "2026-10-30T10:00"
	p := models.CommitmentRolesPatch{TargetDays: fullRolePatchValue(ptr(0)), TargetSecs: fullRolePatchValue(ptr(3600))}
	if _, err := f.s.updateCommitmentAt(f.ws.ID, f.item.ID, models.ScheduleUpdate{Dtstart: &start}, &p, schedNow, ""); err != nil {
		t.Fatal(err)
	}
	wantRemindAt(t, f, sc, "2026-10-30T10:30:00Z", "timing edit with the merged offset")
}

func TestDoneAdvanceArmsNextAtTargetMinusLead(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"done", "skip"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			f := newSchedFixture(t)
			sc := f.targetCommit(t, f.item, models.ScheduleCreate{Dtstart: "2026-10-30T09:00", Tz: "UTC", Rrule: "FREQ=DAILY", LeadMinutes: 30}, 0, 7200)
			f.mustResolve(t, f.item, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: state, Actor: "user"}, "2026-10-30T12:00:00Z")
			wantRemindAt(t, f, sc, "2026-10-31T10:30:00Z", state+" advance")
		})
	}
}

func TestPastMoveArmsNextFutureAtTarget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		lead int
		want string
	}{
		{"target minus lead", 30, "2026-10-31T10:30:00Z"},
		// 2026-10-31T11:00 less 26h is before the clock: the fallback is the target.
		{"lead inside the clock falls back to the target", 26 * 60, "2026-10-31T11:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newSchedFixture(t)
			sc := f.targetCommit(t, f.item, models.ScheduleCreate{Dtstart: "2026-10-30T09:00", Tz: "UTC", Rrule: "FREQ=DAILY", LeadMinutes: tc.lead}, 0, 7200)
			input := moveInput(sc, "2026-10-30T07:00:00Z")
			f.mustResolve(t, f.item, input, "2026-10-30T10:00:00Z")
			assertMoveArm(t, f, sc, tc.want)
		})
	}
}

func TestOverrideStillWinsOverOffset(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.targetCommit(t, f.item, models.ScheduleCreate{Dtstart: "2026-10-30T09:00", Tz: "UTC", Rrule: "FREQ=DAILY", LeadMinutes: 30}, 0, 7200)
	in := moveInput(sc, "2026-10-31T15:00:00Z")
	f.mustResolve(t, f.item, in, moveNow)
	assertMoveArm(t, f, sc, "2026-10-31T14:30:00Z")
	in.State, in.Until = "snooze", ptr(rt("2026-10-31T14:50:00Z"))
	f.mustResolve(t, f.item, in, "2026-10-31T14:35:00Z")
	assertMoveArm(t, f, sc, "2026-10-31T14:50:00Z")
	// A lead edit recomputes from the override, never from the offset.
	lead := 10
	if _, err := f.s.updateCommitmentAt(f.ws.ID, f.item.ID, models.ScheduleUpdate{LeadMinutes: &lead}, nil, rt("2026-10-31T14:36:00Z"), ""); err != nil {
		t.Fatal(err)
	}
	cur := f.reload(t, f.item.ID)
	ov, _ := time.Parse(time.RFC3339, *cur.NextOverrideAt)
	assertMoveArm(t, f, sc, NormalizeInstant(ov.Add(-10*time.Minute)))
}

func TestFloatingOffsetBoundEarlyAndExactAtFire(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	ny := f.user(t, "ny4713@example.com", "America/New_York")
	f.assign(t, f.item.ID, ny.ID)
	sc := f.targetCommit(t, f.item, models.ScheduleCreate{Dtstart: "2026-10-30T09:00", Floating: true, Rrule: "FREQ=DAILY", LeadMinutes: 30}, 0, 7200)
	// 11:00 at +14 is 21:00Z the day before, less the lead.
	bound := "2026-10-29T20:30:00Z"
	// 11:00 EDT is 15:00Z, less the lead.
	exact := "2026-10-30T14:30:00Z"
	wantRemindAt(t, f, sc, bound, "the +14 bound")
	if bound > exact {
		t.Fatalf("bound %s after exact %s", bound, exact)
	}
	if got := f.fire(t, bound); len(got) != 0 {
		t.Fatalf("the bound fired %+v, want a deferral", got)
	}
	wantRemindAt(t, f, sc, exact, "deferred to the recipient-zone target")
	// A restart leaves the deferred exact instant alone: it is not the old
	// start-minus-lead bound the repair exists for.
	if n, err := f.s.RetimeTargetOffsetReminders(); err != nil || n != 0 {
		t.Fatalf("repair after deferral: n=%d err=%v, want 0", n, err)
	}
	wantRemindAt(t, f, sc, exact, "deferred instant after the repair")
	if got := f.fire(t, "2026-10-30T14:29:59Z"); len(got) != 0 {
		t.Fatalf("fired early %+v", got)
	}
	if got := f.fire(t, exact); len(got) != 1 {
		t.Fatalf("exact pass fired %+v", got)
	}
}

func TestResetFloatingBoundKeepsOffset(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	ny := f.user(t, "nyreset4713@example.com", "America/New_York")
	f.assign(t, f.item.ID, ny.ID)
	sc := f.targetCommit(t, f.item, models.ScheduleCreate{Dtstart: "2026-10-30T09:00", Floating: true, Rrule: "FREQ=DAILY", LeadMinutes: 30}, 0, 7200)
	f.fire(t, "2026-10-29T20:30:00Z") // defers to the exact instant
	wantRemindAt(t, f, sc, "2026-10-30T14:30:00Z", "deferred")
	utc := f.user(t, "utcreset4713@example.com", "UTC")
	f.assign(t, f.item.ID, utc.ID)
	f.s.resetFloatingForItem(f.item.ID)
	wantRemindAt(t, f, sc, "2026-10-29T20:30:00Z", "reset to the +14 bound of the target")
}

func TestCarryArmsAtTargetOffset(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	f.targetCommit(t, f.item, models.ScheduleCreate{Dtstart: "2030-03-04T09:00", Tz: "UTC", Rrule: "FREQ=DAILY", LeadMinutes: 30}, 0, 7200)
	ws, _ := f.importBundle(t, f.exportBundle(t), "Carried")
	item := f.itemByTitle(t, ws.ID, f.item.Title)
	sc, err := f.s.GetSchedule(ws.ID, item.ID)
	if err != nil || sc == nil {
		t.Fatalf("GetSchedule: %v %v", sc, err)
	}
	wantRemindAt(t, f, sc, "2030-03-04T10:30:00Z", "carried")
}

func TestRetimeTargetOffsetRemindersRepairs(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	daily := models.ScheduleCreate{Dtstart: "2026-10-30T09:00", Tz: "UTC", Rrule: "FREQ=DAILY", LeadMinutes: 30}
	seed := func(sc *models.ItemSchedule, at string) {
		f.exec(t, `UPDATE item_reminders SET remind_at=? WHERE schedule_id=?`, at, sc.ID)
	}

	// Fired: a row that went out at the old instant stays.
	firedItem := f.another(t, "Fired")
	fired := f.targetCommit(t, firedItem, daily, 0, 7200)
	seed(fired, "2026-10-30T08:30:00Z")
	if got := f.fire(t, "2026-10-30T08:30:00Z"); len(got) != 1 {
		t.Fatalf("fired %+v", got)
	}
	firedBefore := f.managed(t, f.reload(t, firedItem.ID))

	// Overridden: the override is the base, not the offset.
	movedItem := f.another(t, "Moved")
	moved := f.targetCommit(t, movedItem, daily, 0, 7200)
	f.mustResolve(t, movedItem, moveInput(moved, "2026-10-31T15:00:00Z"), moveNow)
	seed(moved, "2026-10-31T13:00:00Z")

	// Ended: no outstanding occurrence and no armed row.
	endedItem := f.another(t, "Ended")
	ended := f.targetCommit(t, endedItem, models.ScheduleCreate{Dtstart: "2026-10-30T09:00", Tz: "UTC"}, 0, 7200)
	f.mustResolve(t, endedItem, ResolveInput{OccurrenceAt: rt(*ended.NextAt), State: "done", Actor: "user", EndSchedule: true}, "2026-10-30T12:00:00Z")

	// Zero offset: not this repair's row.
	zeroItem := f.another(t, "Zero")
	zero := f.targetCommit(t, zeroItem, daily, 0, 0)
	seed(zero, "2026-10-30T07:00:00Z")

	// The broken row: armed at the start minus the lead.
	broken := f.targetCommit(t, f.item, daily, 0, 7200)
	seed(broken, "2026-10-30T08:30:00Z")

	// Inside the lead after a past move: armed at the target itself (11:00,
	// target minus 26h is already past). A restart must leave it there.
	insideItem := f.another(t, "Inside")
	inside := f.targetCommit(t, insideItem, models.ScheduleCreate{Dtstart: "2026-10-30T09:00", Tz: "UTC", Rrule: "FREQ=DAILY", LeadMinutes: 26 * 60}, 0, 7200)
	f.mustResolve(t, insideItem, moveInput(inside, "2026-10-30T07:00:00Z"), "2026-10-30T10:00:00Z")
	wantRemindAt(t, f, inside, "2026-10-31T11:00:00Z", "inside-lead fallback before the repair")

	// Already right and due: armed by the current code at target minus the
	// lead (10:30), unfired, its instant past the wall clock. A restart inside
	// the lead window must not delay it to the target.
	dueItem := f.another(t, "Due")
	due := f.targetCommit(t, dueItem, daily, 0, 7200)
	wantRemindAt(t, f, due, "2026-10-30T10:30:00Z", "correct row before the repair")

	n, err := f.s.RetimeTargetOffsetReminders()
	if err != nil || n != 1 {
		t.Fatalf("first run: n=%d err=%v, want 1", n, err)
	}
	wantRemindAt(t, f, broken, "2026-10-30T10:30:00Z", "repaired")
	wantRemindAt(t, f, moved, "2026-10-31T13:00:00Z", "overridden row")
	wantRemindAt(t, f, zero, "2026-10-30T07:00:00Z", "zero-offset row")
	wantRemindAt(t, f, inside, "2026-10-31T11:00:00Z", "inside-lead fallback after the repair")
	wantRemindAt(t, f, due, "2026-10-30T10:30:00Z", "correct due row after the repair")
	if after := f.managed(t, f.reload(t, firedItem.ID)); after.RemindAt != firedBefore.RemindAt || after.FiredAt == nil {
		t.Fatalf("fired row moved: %+v -> %+v", firedBefore, after)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM item_reminders WHERE schedule_id=?`, ended.ID); got != 0 {
		t.Fatalf("ended schedule has %d reminder rows", got)
	}
	if n, err := f.s.RetimeTargetOffsetReminders(); err != nil || n != 0 {
		t.Fatalf("second run: n=%d err=%v, want 0", n, err)
	}
}

// A schedule whose instant cannot be computed is skipped and reported; the
// rest are still repaired, so one bad row cannot block the repair every boot.
func TestRetimeTargetOffsetRemindersSkipsUncomputableRow(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	daily := models.ScheduleCreate{Dtstart: "2026-10-30T09:00", Tz: "UTC", Rrule: "FREQ=DAILY", LeadMinutes: 30}
	badItem := f.another(t, "Bad zone")
	bad := f.targetCommit(t, badItem, daily, 0, 7200)
	f.exec(t, `UPDATE item_reminders SET remind_at='2026-10-30T08:30:00Z' WHERE schedule_id=?`, bad.ID)
	f.exec(t, `UPDATE item_schedules SET tz='Nowhere/Unloadable' WHERE id=?`, bad.ID)
	good := f.targetCommit(t, f.item, daily, 0, 7200)
	f.exec(t, `UPDATE item_reminders SET remind_at='2026-10-30T08:30:00Z' WHERE schedule_id=?`, good.ID)

	n, err := f.s.RetimeTargetOffsetReminders()
	if err == nil || !strings.Contains(err.Error(), bad.ID) {
		t.Fatalf("want an error naming the skipped schedule %s, got %v", bad.ID, err)
	}
	if n != 1 {
		t.Fatalf("retimed %d, want 1", n)
	}
	wantRemindAt(t, f, good, "2026-10-30T10:30:00Z", "good row repaired beside a bad one")
	wantRemindAt(t, f, bad, "2026-10-30T08:30:00Z", "bad row left as it was")
}
