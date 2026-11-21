package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4714: a resolved occurrence keeps the role offsets in force when it was
// resolved, so a later series offset change does not move history.

// occStamp is one occurrence row's stored offset snapshot; nil is SQL NULL.
type occStamp struct {
	TargetDays, TargetSecs, EarliestDays, EarliestSecs *int
	DeadlineDays, DeadlineSecs, OccupiedDays, OccupiedSecs *int
	Behavior                                               *string
}

func (o occStamp) String() string {
	p := func(v *int) string {
		if v == nil {
			return "null"
		}
		return fmt.Sprint(*v)
	}
	b := "null"
	if o.Behavior != nil {
		b = *o.Behavior
	}
	return fmt.Sprintf("target=%s/%s earliest=%s/%s deadline=%s/%s/%s occupied=%s/%s", p(o.TargetDays), p(o.TargetSecs), p(o.EarliestDays), p(o.EarliestSecs), p(o.DeadlineDays), p(o.DeadlineSecs), b, p(o.OccupiedDays), p(o.OccupiedSecs))
}

func (f *schedFixture) stampOf(t *testing.T, scheduleID, at string) occStamp {
	t.Helper()
	var n [8]sql.NullInt64
	var b sql.NullString
	err := f.s.db.QueryRow(f.s.q(`SELECT roles_target_days, roles_target_secs, roles_earliest_days, roles_earliest_secs, roles_deadline_days, roles_deadline_secs, roles_occupied_days, roles_occupied_secs, roles_deadline_behavior
		FROM schedule_occurrences WHERE schedule_id=? AND occurrence_at=?`), scheduleID, at).Scan(&n[0], &n[1], &n[2], &n[3], &n[4], &n[5], &n[6], &n[7], &b)
	if err != nil {
		t.Fatalf("read stamp %s: %v", at, err)
	}
	var out occStamp
	for i, dst := range []**int{&out.TargetDays, &out.TargetSecs, &out.EarliestDays, &out.EarliestSecs, &out.DeadlineDays, &out.DeadlineSecs, &out.OccupiedDays, &out.OccupiedSecs} {
		if n[i].Valid {
			*dst = ptr(int(n[i].Int64))
		}
	}
	if b.Valid {
		out.Behavior = ptr(b.String)
	}
	return out
}

func entryAtWall(t *testing.T, es []AgendaEntry, wall string) AgendaEntry {
	t.Helper()
	for _, e := range es {
		if e.Wall.String() == wall {
			return e
		}
	}
	t.Fatalf("no entry with wall %s in %d entries: %+v", wall, len(es), es)
	return AgendaEntry{}
}

func nowRFC() string { return time.Now().UTC().Format(time.RFC3339) }

func TestSeriesMoveKeepsDoneRowTarget(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 1)
	f.seriesSchedule(t, 0, models.ScheduleCreate{Dtstart: "2030-03-04T09:00", Tz: "UTC"})
	f.resolve(t, 0, "done")
	if _, _, err := f.seriesMove(0, "2030-03-05T14:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	if got := f.targetOffset(t, 0); got.TargetSecs != 5*3600 {
		t.Fatalf("series move did not change the offset: %+v", got)
	}
	if got := f.stampOf(t, f.scheds[0].ID, "2030-03-04T09:00:00Z"); got.String() != (occStamp{TargetDays: ptr(0), TargetSecs: ptr(0)}).String() {
		t.Fatalf("done row stamp = %s", got)
	}
	es := f.expand(t, f.items[0], time.UTC, "2030-03-04T00:00:00Z", "2030-03-06T00:00:00Z", nowRFC())
	done := entryAtWall(t, es, "2030-03-04T09:00:00")
	if done.State != "done" || done.Roles.Target == nil || *done.Roles.Target != "2030-03-04T09:00:00Z" || !done.At.Equal(rt("2030-03-04T09:00:00Z")) {
		t.Fatalf("done row moved with the series: state=%s at=%s target=%v", done.State, done.At, str(done.Roles.Target))
	}
	next := entryAtWall(t, es, "2030-03-05T09:00:00")
	if next.Roles.Target == nil || *next.Roles.Target != "2030-03-05T14:00:00Z" {
		t.Fatalf("outstanding target = %v, want the new series offset", str(next.Roles.Target))
	}
}

func TestRenegotiateSeriesKeepsPastDeadline(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 1)
	f.exec(t, `DELETE FROM item_schedules WHERE id=?`, f.scheds[0].ID)
	sc, err := f.s.CreateCommitmentSchedule(f.ws.ID, f.items[0].ID, models.ScheduleCreate{Kind: "check", Dtstart: "2030-03-04T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"},
		models.CommitmentRolesInput{DeadlineDays: i(0), DeadlineSecs: i(3600), DeadlineBehavior: ptr("stays_owed")})
	if err != nil {
		t.Fatal(err)
	}
	f.scheds[0] = sc
	f.resolve(t, 0, "done")
	in := seriesRequest()
	in.DeadlineOffset = &models.CommitmentDeadlineOffset{DeadlineDays: i(0), DeadlineSecs: i(7200)}
	in.DeadlineBehavior = ptr("lapses")
	applyRenegotiation(t, f.schedFixture, in, time.Now())
	es := f.expand(t, f.items[0], time.UTC, "2030-03-04T00:00:00Z", "2030-03-06T00:00:00Z", nowRFC())
	done := entryAtWall(t, es, "2030-03-04T09:00:00")
	if done.State != "done" || str(done.Roles.Deadline) != "2030-03-04T10:00:00Z" || str(done.Roles.DeadlineBehavior) != "stays_owed" {
		t.Fatalf("past deadline renegotiated: deadline=%s behavior=%s", str(done.Roles.Deadline), str(done.Roles.DeadlineBehavior))
	}
	next := entryAtWall(t, es, "2030-03-05T09:00:00")
	if str(next.Roles.Deadline) != "2030-03-05T11:00:00Z" || str(next.Roles.DeadlineBehavior) != "lapses" {
		t.Fatalf("outstanding deadline=%s behavior=%s, want the renegotiated series", str(next.Roles.Deadline), str(next.Roles.DeadlineBehavior))
	}
}

// historyFarTarget resolves item 0 done while its target sits ten days after
// the seed, then puts the target back on the seed.
func historyFarTarget(t *testing.T) *linkFix {
	t.Helper()
	f := newLinkFix(t, 1)
	f.exec(t, `DELETE FROM item_schedules WHERE id=?`, f.scheds[0].ID)
	sc, err := f.s.CreateCommitmentSchedule(f.ws.ID, f.items[0].ID, models.ScheduleCreate{Kind: "check", Dtstart: "2030-03-04T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}, models.CommitmentRolesInput{TargetDays: 10})
	if err != nil {
		t.Fatal(err)
	}
	f.scheds[0] = sc
	f.resolve(t, 0, "done")
	if _, err := f.s.SetCommitmentRoles(f.ws.ID, f.items[0].ID, models.CommitmentRolesInput{}); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestHistoryPassFindsRowOutsideCurrentSeedWindow(t *testing.T) {
	t.Parallel()
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "open item", true: "terminal item"}[terminal], func(t *testing.T) {
			t.Parallel()
			f := historyFarTarget(t)
			if terminal {
				f.setStatus(t, 0, "done")
			}
			es := f.expand(t, f.items[0], time.UTC, "2030-03-14T00:00:00Z", "2030-03-15T00:00:00Z", nowRFC())
			done := entryAtWall(t, es, "2030-03-04T09:00:00")
			if done.State != "done" || str(done.Roles.Target) != "2030-03-14T09:00:00Z" {
				t.Fatalf("history row state=%s target=%s", done.State, str(done.Roles.Target))
			}
			// The row is listed once, by its own key.
			n := 0
			for _, e := range es {
				if e.OccurrenceKey == done.OccurrenceKey {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("history row listed %d times", n)
			}
			// Its seed day no longer shows it: its target is not there.
			for _, e := range f.expand(t, f.items[0], time.UTC, "2030-03-04T00:00:00Z", "2030-03-05T00:00:00Z", nowRFC()) {
				if e.Wall.String() == "2030-03-04T09:00:00" {
					t.Fatalf("history row shown at its seed: %+v", e)
				}
			}
		})
	}
}

func TestMovedRowUnstampedAndReopenRederives(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 1)
	f.seriesSchedule(t, 0, models.ScheduleCreate{Dtstart: "2030-03-04T09:00", Tz: "UTC"})
	if _, err := f.move(0, "target", "2030-03-04T11:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	const at = "2030-03-04T09:00:00Z"
	if got := f.stampOf(t, f.scheds[0].ID, at); got.String() != (occStamp{}).String() {
		t.Fatalf("moved row stamped: %s", got)
	}
	f.resolve(t, 0, "done")
	if got := f.stampOf(t, f.scheds[0].ID, at); got.String() != (occStamp{TargetDays: ptr(0), TargetSecs: ptr(0)}).String() {
		t.Fatalf("done row stamp = %s", got)
	}
	view, err := f.s.GetCommitment(f.ws.ID, f.items[0].ID)
	if err != nil || view.LastResolution == nil {
		t.Fatalf("no resolution to reopen: %v", err)
	}
	if _, err := f.s.ReopenOccurrence(f.ws.ID, f.items[0].ID, ReopenInput{ResolutionID: view.LastResolution.ResolutionID, Actor: "user"}); err != nil {
		t.Fatal(err)
	}
	if got := f.stampOf(t, f.scheds[0].ID, at); got.String() != (occStamp{}).String() {
		t.Fatalf("reopened moved row stamped: %s", got)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=? AND state='moved'`, f.scheds[0].ID); n != 1 {
		t.Fatalf("moved rows after reopen = %d", n)
	}
	// Outstanding again, the occurrence reads the current offsets: an offset
	// change now reaches it, and its next resolution stamps that offset.
	f.exec(t, `UPDATE item_schedules SET target_secs=1800 WHERE id=?`, f.scheds[0].ID)
	f.exec(t, `DELETE FROM schedule_occurrence_overrides WHERE schedule_id=?`, f.scheds[0].ID)
	f.resolve(t, 0, "done")
	if got := f.stampOf(t, f.scheds[0].ID, at); got.String() != (occStamp{TargetDays: ptr(0), TargetSecs: ptr(1800)}).String() {
		t.Fatalf("re-resolved row stamp = %s, want the offset in force now", got)
	}
}

func TestMissedAndLapsedRowsStamped(t *testing.T) {
	t.Run("missed", func(t *testing.T) {
		t.Parallel()
		f := newSchedFixture(t)
		item, sc := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}, "2026-10-01T00:00:00Z")
		f.exec(t, `UPDATE item_schedules SET roles_origin='native', target_secs=600, earliest_days=0, earliest_secs=-600 WHERE id=?`, sc.ID)
		f.mustResolve(t, item, ResolveInput{OccurrenceAt: rt("2026-10-01T09:00:00Z"), State: "done", Actor: "user"}, "2026-10-03T12:00:00Z")
		if f.occRows(t, sc.ID, "missed") == 0 {
			t.Fatal("no missed rows recorded")
		}
		want := (occStamp{TargetDays: ptr(0), TargetSecs: ptr(600), EarliestDays: ptr(0), EarliestSecs: ptr(-600)}).String()
		for _, at := range []string{"2026-10-01T09:00:00Z", "2026-10-02T09:00:00Z"} {
			if got := f.stampOf(t, sc.ID, at); got.String() != want {
				t.Fatalf("%s stamp = %s, want %s", at, got, want)
			}
		}
		if n := f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=? AND roles_target_days IS NULL`, sc.ID); n != 0 {
			t.Fatalf("%d unstamped rows", n)
		}
	})
	t.Run("lapsed then late done", func(t *testing.T) {
		t.Parallel()
		f := newSchedFixture(t)
		sc := lapseSchedule(t, f, true)
		f.eval(t, lapseNow, 0)
		want := (occStamp{TargetDays: ptr(0), TargetSecs: ptr(0), DeadlineDays: ptr(0), DeadlineSecs: ptr(3600), Behavior: ptr("lapses")}).String()
		if got := f.stampOf(t, sc.ID, deadlineOccAt); got.String() != want {
			t.Fatalf("lapsed stamp = %s, want %s", got, want)
		}
		// The late Done keeps the stamp the lapse wrote, whatever the offsets are now.
		f.exec(t, `UPDATE item_schedules SET deadline_secs=7200 WHERE id=?`, sc.ID)
		f.lateDoneForReopenTest(t, sc, ResolveInput{OccurrenceAt: rt(deadlineOccAt)}, "2026-10-26T12:30:00Z")
		if n := f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=? AND occurrence_at=? AND state='done'`, sc.ID, deadlineOccAt); n != 1 {
			t.Fatal("late done did not land on the lapsed row")
		}
		if got := f.stampOf(t, sc.ID, deadlineOccAt); got.String() != want {
			t.Fatalf("late done stamp = %s, want %s", got, want)
		}
	})
}

func TestOccurrenceRolesCheckRefusesMovedStamp(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.create(t, dailyUTC)
	ins := func(state string, target any) error {
		_, err := f.s.db.Exec(f.s.q(`INSERT INTO schedule_occurrences (id, schedule_id, occurrence_at, occurrence_wall, state, actor, resolved_at, roles_target_days, roles_target_secs) VALUES (?, ?, ?, ?, ?, '', ?, ?, ?)`),
			newID(), sc.ID, "2026-10-30T09:00:00Z", "2026-10-30T09:00:00", state, "2026-10-30T10:00:00Z", target, target)
		return err
	}
	if err := ins("moved", 0); err == nil {
		t.Fatal("a stamped moved row was accepted")
	}
	if err := ins("moved", nil); err != nil {
		t.Fatalf("an unstamped moved row was refused: %v", err)
	}
	f.exec(t, `DELETE FROM schedule_occurrences WHERE schedule_id=?`, sc.ID)
	if err := ins("done", 0); err != nil {
		t.Fatalf("a stamped done row was refused: %v", err)
	}
	if _, err := f.s.db.Exec(f.s.q(`UPDATE schedule_occurrences SET state='moved' WHERE schedule_id=?`), sc.ID); err == nil {
		t.Fatal("a stamped row was turned moved")
	}
}

// A checkpoint written before the migration has no roles_* keys in its
// images: the undo refuses honestly and leaves the row as it is.
func TestPreUpgradeCheckpointRefusesReopen(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	item, sc := f.mk(t, dailyUTC, "2026-10-29T00:00:00Z")
	res := f.resolveForReopenTest(t, item, ResolveInput{OccurrenceAt: rt("2026-10-30T09:00:00Z"), State: "done", Actor: "user"}, "2026-10-30T10:00:00Z")
	if res.ScheduleLastResolution == nil {
		t.Fatal("no checkpoint")
	}
	strip := func(raw string) string {
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatal(err)
		}
		var walk func(any, bool)
		walk = func(v any, inOcc bool) {
			switch x := v.(type) {
			case map[string]any:
				for k, c := range x {
					if inOcc && strings.HasPrefix(k, "roles_") {
						delete(x, k)
						continue
					}
					walk(c, inOcc || k == "schedule_occurrences")
				}
			case []any:
				for _, c := range x {
					walk(c, inOcc)
				}
			}
		}
		walk(v, false)
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	var before, after string
	if err := f.s.db.QueryRow(f.s.q(`SELECT before_json, after_json FROM schedule_reopen_checkpoints WHERE schedule_id=?`), sc.ID).Scan(&before, &after); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(after, "roles_target_days") {
		t.Fatal("the premise failed: a current checkpoint carries the snapshot columns")
	}
	f.exec(t, `UPDATE schedule_reopen_checkpoints SET before_json=?, after_json=? WHERE schedule_id=?`, strip(before), strip(after), sc.ID)
	rows := renegotiateRows(t, f.s, "schedule_occurrences", "WHERE schedule_id=?", sc.ID)
	_, err := f.s.reopenOccurrenceAt(f.ws.ID, item.ID, ReopenInput{ResolutionID: res.ResolutionID, Actor: "user"}, rt("2026-10-30T10:01:00Z"))
	var re *ReopenError
	if !errors.As(err, &re) || re.Code != "reopen_conflict" {
		t.Fatalf("err=%v, want reopen_conflict", err)
	}
	if got := renegotiateRows(t, f.s, "schedule_occurrences", "WHERE schedule_id=?", sc.ID); got != rows {
		t.Fatalf("refused reopen changed the row\nbefore %s\nafter  %s", rows, got)
	}
}
