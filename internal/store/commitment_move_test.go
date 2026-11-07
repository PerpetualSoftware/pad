package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

const moveNow = "2026-10-30T08:00:00Z"

func moveSchedule(t *testing.T, f *schedFixture, kind string, lead int, rule string) *models.ItemSchedule {
	t.Helper()
	return f.create(t, models.ScheduleCreate{Kind: kind, Dtstart: "2026-10-30T09:00", Tz: "UTC", LeadMinutes: lead, Rrule: rule})
}

func moveInput(sc *models.ItemSchedule, target string) ResolveInput {
	return ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "move", Until: ptr(rt(target)), Actor: "user"}
}

func moveEntries(t *testing.T, f *schedFixture, id string) []models.ScheduleChangeExport {
	t.Helper()
	rows, err := f.s.scheduleChangesQ(f.s.db, f.ws.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func assertMoveArm(t *testing.T, f *schedFixture, sc *models.ItemSchedule, want string) {
	t.Helper()
	if n := f.count(t, `SELECT COUNT(*) FROM item_reminders WHERE schedule_id=? AND fired_at IS NULL`, sc.ID); n != 1 {
		t.Fatalf("armed reminders=%d, want 1", n)
	}
	if r := f.managed(t, f.reload(t, sc.ItemID)); r.RemindAt != want {
		t.Fatalf("remind_at=%s, want %s", r.RemindAt, want)
	}
}

func TestMoveTargetSavesImmediatelyNoReason(t *testing.T) {
	for _, body := range []string{"nil", "", "   "} {
		t.Run(body, func(t *testing.T) {
			f := newSchedFixture(t)
			sc := moveSchedule(t, f, "deadline_soft", 30, "")
			in := moveInput(sc, "2026-10-31T10:00:00Z")
			if body != "nil" {
				in.Comment = f.comment(f.item, "", body)
			}
			f.mustResolve(t, f.item, in, moveNow)
			if n := f.count(t, `SELECT COUNT(*) FROM schedule_occurrence_overrides WHERE schedule_id=? AND role='target' AND value_at=?`, sc.ID, NormalizeInstant(*in.Until)); n != 1 {
				t.Fatal("target not saved")
			}
			entries := moveEntries(t, f, sc.ID)
			e := entries[len(entries)-1]
			if e.ChangeType != "move" || e.ReasonSource != "system" || e.Reason != "Rescheduled" {
				t.Fatalf("journal=%+v", e)
			}
			if f.count(t, `SELECT COUNT(*) FROM comments WHERE item_id=?`, f.item.ID) != 0 {
				t.Fatal("empty reason created a comment")
			}
			if f.events(t) != 0 {
				t.Fatal("move fired immediately")
			}
		})
	}
}

func TestMoveTargetWithReasonJournalsUser(t *testing.T) {
	f := newSchedFixture(t)
	sc := moveSchedule(t, f, "deadline_soft", 0, "")
	in := moveInput(sc, "2026-10-31T10:00:00Z")
	in.Comment = f.comment(f.item, "", "Waiting for a delivery")
	res := f.mustResolve(t, f.item, in, moveNow)
	if res.Comment == nil || res.Comment.Body != in.Comment.Input.Body {
		t.Fatal("reason comment missing")
	}
	entries := moveEntries(t, f, sc.ID)
	e := entries[len(entries)-1]
	if e.ReasonSource != "user" || e.Reason != in.Comment.Input.Body || e.BeforeJSON == nil || e.AfterJSON == nil {
		t.Fatalf("journal=%+v", e)
	}
}

func TestMoveArmsOneReminderAtNewTargetMinusLead(t *testing.T) {
	f := newSchedFixture(t)
	sc := moveSchedule(t, f, "deadline_soft", 30, "")
	f.mustResolve(t, f.item, moveInput(sc, "2026-10-31T10:00:00Z"), moveNow)
	assertMoveArm(t, f, sc, "2026-10-31T09:30:00Z")
	if len(f.fire(t, "2026-10-31T09:29:59Z")) != 0 || len(f.fire(t, "2026-10-31T09:30:00Z")) != 1 {
		t.Fatal("wrong fire instant")
	}
}

func TestMoveInsideLeadArmsAtTarget(t *testing.T) {
	for _, target := range []string{"2026-10-30T08:15:00Z", "2026-10-30T08:30:00Z"} {
		t.Run(target, func(t *testing.T) {
			f := newSchedFixture(t)
			sc := moveSchedule(t, f, "check", 30, "")
			f.mustResolve(t, f.item, moveInput(sc, target), moveNow)
			assertMoveArm(t, f, sc, target)
			if len(f.fire(t, moveNow)) != 0 || len(f.fire(t, target)) != 1 {
				t.Fatal("inside-lead fire must wait for target")
			}
		})
	}
}

func TestMoveToPastArmsNothingFiresNothing(t *testing.T) {
	for _, target := range []string{"2026-10-30T07:00:00Z", moveNow} {
		t.Run(target, func(t *testing.T) {
			f := newSchedFixture(t)
			sc := moveSchedule(t, f, "check", 30, "")
			f.mustResolve(t, f.item, moveInput(sc, target), moveNow)
			if f.count(t, `SELECT COUNT(*) FROM item_reminders WHERE schedule_id=? AND fired_at IS NULL`, sc.ID) != 0 {
				t.Fatal("past move armed a reminder")
			}
			if len(f.fire(t, "2026-11-01T12:00:00Z")) != 0 || f.events(t) != 0 {
				t.Fatal("past target fired")
			}
			if f.occRows(t, sc.ID, "done") != 0 || f.occRows(t, sc.ID, "skipped") != 0 {
				t.Fatal("move recorded a resolution")
			}
			if f.count(t, `SELECT COUNT(*) FROM schedule_occurrence_overrides WHERE schedule_id=? AND value_at=?`, sc.ID, target) != 1 {
				t.Fatal("past target not saved")
			}
		})
	}
}

func TestMoveToPastOnSeriesNextOccurrenceKeepsReminder(t *testing.T) {
	for _, floating := range []bool{false, true} {
		t.Run(map[bool]string{false: "pinned", true: "floating"}[floating], func(t *testing.T) {
			f := newSchedFixture(t)
			in := dailyUTC
			in.LeadMinutes, in.Floating = 30, floating
			if floating {
				in.Tz = ""
			}
			sc := f.create(t, in)
			original, _, err := OutstandingInstant(sc, time.UTC)
			if err != nil {
				t.Fatal(err)
			}
			input := moveInput(sc, "2026-10-30T07:00:00Z")
			input.OccurrenceAt = original
			f.mustResolve(t, f.item, input, "2026-10-30T10:00:00Z")
			if len(f.fire(t, "2026-10-30T10:00:00Z")) != 0 || f.events(t) != 0 {
				t.Fatal("moved occurrence fired")
			}
			assertMoveArm(t, f, sc, "2026-10-31T08:30:00Z")
			got := f.fire(t, "2026-10-31T08:30:00Z")
			if len(got) != 1 || got[0].OccurrenceAt == nil || *got[0].OccurrenceAt != "2026-10-31T09:00:00Z" {
				t.Fatalf("next fire=%+v", got)
			}
			if f.occRows(t, sc.ID, "moved") != 1 || f.occRows(t, sc.ID, "done") != 0 {
				t.Fatal("move became success")
			}
		})
	}
}

func TestMoveRearmSameTx(t *testing.T) {
	f := newSchedFixture(t)
	if f.s.dialect.Driver() != DriverSQLite {
		t.Skip("SQLite fault injection")
	}
	sc := moveSchedule(t, f, "check", 30, "")
	before := commitmentOldRows(t, f.s)
	entries := moveEntries(t, f, sc.ID)
	f.exec(t, `CREATE TRIGGER move_rearm_fail BEFORE UPDATE ON item_reminders BEGIN SELECT CASE WHEN NOT EXISTS (SELECT 1 FROM schedule_occurrence_overrides WHERE role='target') THEN RAISE(ABORT,'override not written') ELSE RAISE(ABORT,'rearm refused after override') END; END`)
	_, err := f.resolve(t, f.item, moveInput(sc, "2026-10-31T10:00:00Z"), moveNow)
	if err == nil || !strings.Contains(err.Error(), "rearm refused after override") {
		t.Fatalf("fault did not follow override: %v", err)
	}
	if !reflect.DeepEqual(before, commitmentOldRows(t, f.s)) || !reflect.DeepEqual(entries, moveEntries(t, f, sc.ID)) {
		t.Fatal("failed rearm persisted changes")
	}
	if f.count(t, `SELECT COUNT(*) FROM schedule_occurrence_overrides WHERE schedule_id=?`, sc.ID) != 0 {
		t.Fatal("override escaped rollback")
	}
}

func TestSnoozeDoesNotChangeTarget(t *testing.T) {
	f := newSchedFixture(t)
	sc := moveSchedule(t, f, "check", 30, "")
	in := moveInput(sc, "2026-10-31T10:00:00Z")
	f.mustResolve(t, f.item, in, moveNow)
	before, err := f.s.scheduleOverridesQ(f.s.db, f.ws.ID, sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	in.State, in.Until = "snooze", ptr(rt("2026-10-31T10:15:00Z"))
	f.mustResolve(t, f.item, in, "2026-10-31T09:45:00Z")
	after, err := f.s.scheduleOverridesQ(f.s.db, f.ws.ID, sc.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("snooze changed target: %v", err)
	}
	assertMoveArm(t, f, sc, "2026-10-31T10:15:00Z")
	entries := moveEntries(t, f, sc.ID)
	e := entries[len(entries)-1]
	if e.ChangeType != "snooze" || e.BeforeJSON != nil || e.AfterJSON != nil || e.Role != nil {
		t.Fatalf("snooze journaled a date change: %+v", e)
	}
}

func TestPushOneShotPerTargetTime(t *testing.T) {
	f := newSchedFixture(t)
	u := f.user(t, "move-push@example.com", "UTC")
	addDevice(t, f.s, u.ID)
	f.assign(t, f.item.ID, u.ID)
	sc := moveSchedule(t, f, "check", 0, "")
	for i, target := range []string{"2026-10-31T09:00:00Z", "2026-10-31T09:00:00Z", "2026-11-01T09:00:00Z", "2026-10-31T09:00:00Z"} {
		f.mustResolve(t, f.item, moveInput(sc, target), moveNow)
		f.fire(t, target)
		f.fire(t, target)
		want := 1
		if i >= 2 {
			want = 2
		}
		if n := len(f.notifications(t, u.ID)); n != want {
			t.Fatalf("move %d: notifications=%d, want %d", i, n, want)
		}
	}
}

func TestSnoozeAfterFiredMovePushesAgain(t *testing.T) {
	f := newSchedFixture(t)
	u := f.user(t, "move-snooze-push@example.com", "UTC")
	addDevice(t, f.s, u.ID)
	f.assign(t, f.item.ID, u.ID)
	sc := moveSchedule(t, f, "check", 0, "")
	in := moveInput(sc, "2026-10-31T09:00:00Z")
	f.mustResolve(t, f.item, in, moveNow)
	f.fire(t, "2026-10-31T09:00:00Z")
	if n := len(f.notifications(t, u.ID)); n != 1 {
		t.Fatalf("after the moved fire: notifications=%d, want 1", n)
	}
	in.State, in.Until = "snooze", ptr(rt("2026-10-31T09:15:00Z"))
	f.mustResolve(t, f.item, in, "2026-10-31T09:01:00Z")
	f.fire(t, "2026-10-31T09:15:00Z")
	f.fire(t, "2026-10-31T09:15:00Z")
	if n := len(f.notifications(t, u.ID)); n != 2 {
		t.Fatalf("after the snooze fire: notifications=%d, want 2", n)
	}
}

func TestWindowEndEditKeepsMovedTargetAndReminder(t *testing.T) {
	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{Kind: "window", Dtstart: "2026-10-30T09:00", Dtend: "2026-10-30T10:00", Tz: "UTC"})
	f.mustResolve(t, f.item, moveInput(sc, "2026-10-31T10:00:00Z"), moveNow)
	f.update(t, models.ScheduleUpdate{Dtend: ptr("2026-10-30T11:00")})
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_occurrence_overrides WHERE schedule_id=? AND role='target'`, sc.ID); n != 1 {
		t.Fatalf("the end edit dropped the moved target row: %d", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM item_schedules WHERE id=? AND next_override_at IS NOT NULL`, sc.ID); n != 1 {
		t.Fatal("the end edit cleared the stored moved target")
	}
	assertMoveArm(t, f, sc, "2026-10-31T10:00:00Z")
}

func TestMoveFloatingAndPinnedArm(t *testing.T) {
	for _, floating := range []bool{false, true} {
		t.Run(map[bool]string{false: "pinned", true: "floating"}[floating], func(t *testing.T) {
			f := newSchedFixture(t)
			u := f.user(t, "move-zone@example.com", "America/Los_Angeles")
			f.assign(t, f.item.ID, u.ID)
			in := models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "America/Los_Angeles", Floating: floating, LeadMinutes: 30}
			if floating {
				in.Tz = ""
			}
			sc := f.create(t, in)
			input := moveInput(sc, "2026-10-30T17:15:00Z")
			input.OccurrenceAt = rt("2026-10-30T16:00:00Z")
			f.mustResolve(t, f.item, input, "2026-10-30T17:00:00Z")
			assertMoveArm(t, f, sc, "2026-10-30T17:15:00Z")
			if len(f.fire(t, "2026-10-30T17:00:00Z")) != 0 || len(f.fire(t, "2026-10-30T17:15:00Z")) != 1 {
				t.Fatal("wrong zoned fire instant")
			}
		})
	}
}

func TestActualDoneLowerBound(t *testing.T) {
	for _, tc := range []struct {
		name, rule string
		actual     time.Time
		created    string
		wantError  bool
	}{
		{"zero", "FREQ=MINUTELY;INTERVAL=15", time.Time{}, "2026-10-29T12:00:00Z", true},
		{"before creation", "FREQ=HOURLY", rt("2026-10-29T11:59:59Z"), "2026-10-29T12:00:00Z", true},
		{"old minutely", "FREQ=MINUTELY;INTERVAL=15", rt("1970-01-01T00:00:00Z"), "2026-10-29T12:00:00Z", true},
		{"at creation", "FREQ=HOURLY", rt("2026-10-29T12:00:00Z"), "2026-10-29T12:00:00Z", false},
		{"future", "FREQ=HOURLY", rt("2026-10-31T00:00:00Z"), "2026-10-29T12:00:00Z", true},
		{"cap", "FREQ=MINUTELY", rt("1970-01-01T00:00:00Z"), "1970-01-01T00:00:00Z", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSchedFixture(t)
			in := dailyUTC
			in.Anchor, in.Rrule = "completion", "FREQ=HOURLY"
			sc := f.create(t, in)
			f.exec(t, `UPDATE item_schedules SET created_at=?,rrule=? WHERE id=?`, tc.created, tc.rule, sc.ID)
			input := ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "done", Actor: "user", ActualDoneAt: &tc.actual}
			resolveNow := "2026-10-30T10:00:00Z"
			if tc.name == "cap" {
				limits := schedule.DefaultLimits
				schedule.DefaultLimits.MaxDuration = time.Nanosecond
				defer func() { schedule.DefaultLimits = limits }()
				resolveNow = moveNow
			}
			_, err := f.resolve(t, f.item, input, resolveNow)
			if !tc.wantError {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var fe *ScheduleFieldError
			var capErr *schedule.CapError
			if !errors.As(err, &fe) || fe.Field != "actual_done_at" || errors.As(err, &capErr) {
				t.Fatalf("want actual_done_at field error, got %T: %v", err, err)
			}
			if f.occTotal(t, sc.ID) != 0 || len(moveEntries(t, f, sc.ID)) != 1 {
				t.Fatal("invalid actual persisted changes")
			}
		})
	}
}

func TestJournalOrderedByRecordedAtThenID(t *testing.T) {
	f := newSchedFixture(t)
	sc := moveSchedule(t, f, "check", 0, "")
	stamps := []struct{ id, at string }{{"journal-z", "2026-10-30T08:00:00Z"}, {"journal-a", "2026-10-30T08:00:00.5Z"}, {"journal-b", "2026-10-30T08:00:00.5Z"}, {"journal-nano", "2026-10-30T08:00:00.000000001Z"}}
	for _, v := range stamps {
		f.exec(t, `INSERT INTO schedule_changes (id,workspace_id,item_id,schedule_id,change_type,root_change_id,recorded_at) VALUES (?,?,?,?,'update',?,?)`, v.id, f.ws.ID, f.item.ID, sc.ID, v.id, v.at)
	}
	for _, id := range []string{sc.ID, ""} {
		entries := moveEntries(t, f, id)
		var got []string
		for _, e := range entries {
			if strings.HasPrefix(e.ID, "journal-") {
				got = append(got, e.ID)
			}
		}
		if want := []string{"journal-z", "journal-nano", "journal-a", "journal-b"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("journal order=%v, want %v", got, want)
		}
	}
}

func TestMissedRecordNoSnapshot(t *testing.T) {
	f := newSchedFixture(t)
	sc := moveSchedule(t, f, "check", 0, "FREQ=DAILY")
	res := f.mustResolve(t, f.item, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "skip", Actor: "user"}, "2026-11-03T10:00:00Z")
	entries := moveEntries(t, f, sc.ID)
	missed := 0
	for _, e := range entries {
		if e.ChangeType == "missed_recorded" {
			missed++
			if e.BeforeJSON != nil || e.AfterJSON != nil || e.OccurrenceKey == nil {
				t.Fatalf("miss journal=%+v", e)
			}
		}
	}
	if missed != 4 || res.MissedRecorded != missed {
		t.Fatalf("missed=%d, recorded=%d", missed, res.MissedRecorded)
	}
	if f.occRows(t, sc.ID, "done") != 0 || f.occRows(t, sc.ID, "skipped") != 1 {
		t.Fatal("skip counted as success")
	}
}

func TestTimingEditClearsOverrides(t *testing.T) {
	for _, floating := range []bool{false, true} {
		for _, edit := range []string{"start", "end", "rule", "floating", "zone", "rdates", "exdates", "anchor", "lead", "noop"} {
			t.Run(map[bool]string{false: "pinned", true: "floating"}[floating]+"/"+edit, func(t *testing.T) {
				if floating && edit == "zone" {
					t.Skip("floating schedules have no zone")
				}
				f := newSchedFixture(t)
				in := dailyUTC
				if edit == "end" {
					in.Kind, in.Dtend = "window", "2026-10-30T10:00"
				}
				in.Floating = floating
				if floating {
					in.Tz = ""
				}
				sc := f.create(t, in)
				at, _, err := OutstandingInstant(sc, time.UTC)
				if err != nil {
					t.Fatal(err)
				}
				input := moveInput(sc, "2026-10-31T10:00:00Z")
				input.OccurrenceAt = at
				f.mustResolve(t, f.item, input, moveNow)
				upd := models.ScheduleUpdate{}
				switch edit {
				case "start":
					upd.Dtstart = ptr("2026-11-01T09:00")
				case "end":
					upd.Dtend = ptr("2026-10-30T11:00")
				case "rule":
					upd.Rrule = ptr("FREQ=WEEKLY")
				case "floating":
					upd.Floating = ptr(!floating)
					if floating {
						upd.Tz = ptr("UTC")
					}
				case "zone":
					upd.Tz = ptr("America/Los_Angeles")
				case "rdates":
					upd.RDates = ptr([]string{"2026-11-02T10:00:00"})
				case "exdates":
					upd.ExDates = ptr([]string{"2026-11-02T09:00:00"})
				case "anchor":
					upd.Anchor = ptr("completion")
				case "lead":
					upd.LeadMinutes = ptr(15)
				case "noop":
					upd.Rrule = sc.Rrule
				}
				f.update(t, upd)
				want := 0
				if edit == "lead" || edit == "noop" || edit == "end" {
					want = 1
				}
				if n := f.count(t, `SELECT COUNT(*) FROM schedule_occurrence_overrides WHERE schedule_id=?`, sc.ID); n != want {
					t.Fatalf("overrides=%d, want %d", n, want)
				}
				if edit != "lead" && edit != "noop" && edit != "end" {
					entries := moveEntries(t, f, sc.ID)
					e := entries[len(entries)-1]
					if e.ChangeType != "update" || e.ReasonSource != "system" || e.Reason != "Occurrence overrides cleared" {
						t.Fatalf("clear not journaled: %+v", e)
					}
				}
			})
		}
	}
	t.Run("rollback", func(t *testing.T) {
		f := newSchedFixture(t)
		if f.s.dialect.Driver() != DriverSQLite {
			t.Skip("SQLite fault injection")
		}
		sc := moveSchedule(t, f, "check", 0, "FREQ=DAILY")
		f.mustResolve(t, f.item, moveInput(sc, "2026-10-31T10:00:00Z"), moveNow)
		before := commitmentOldRows(t, f.s)
		overrides, err := f.s.scheduleOverridesQ(f.s.db, f.ws.ID, sc.ID)
		if err != nil {
			t.Fatal(err)
		}
		entries := moveEntries(t, f, sc.ID)
		f.exec(t, `CREATE TRIGGER clear_overrides_fail BEFORE INSERT ON schedule_changes WHEN NEW.change_type='update' BEGIN SELECT CASE WHEN EXISTS (SELECT 1 FROM schedule_occurrence_overrides) THEN RAISE(ABORT,'overrides not cleared') ELSE RAISE(ABORT,'journal refused after clear') END; END`)
		_, err = f.s.updateScheduleAt(f.ws.ID, f.item.ID, models.ScheduleUpdate{Floating: ptr(true)}, schedNow)
		if err == nil || !strings.Contains(err.Error(), "journal refused after clear") {
			t.Fatalf("fault did not follow clear: %v", err)
		}
		after, err := f.s.scheduleOverridesQ(f.s.db, f.ws.ID, sc.ID)
		if err != nil || !reflect.DeepEqual(overrides, after) || !reflect.DeepEqual(before, commitmentOldRows(t, f.s)) || !reflect.DeepEqual(entries, moveEntries(t, f, sc.ID)) {
			t.Fatal("failed timing edit persisted changes")
		}
	})
}

func TestHardDeadlineMoveStillRefused(t *testing.T) {
	f := newSchedFixture(t)
	sc := moveSchedule(t, f, "deadline_hard", 0, "")
	before := commitmentOldRows(t, f.s)
	_, err := f.resolve(t, f.item, moveInput(sc, "2026-10-31T10:00:00Z"), moveNow)
	var fe *ScheduleFieldError
	if !errors.As(err, &fe) || fe.Field != "state" || fe.Code != "combination" || fe.Message != "a hard deadline cannot be moved; snooze it, or change its kind" {
		t.Fatalf("hard deadline error=%v", err)
	}
	if !reflect.DeepEqual(before, commitmentOldRows(t, f.s)) || len(moveEntries(t, f, sc.ID)) != 1 {
		t.Fatal("hard move wrote changes")
	}
}
