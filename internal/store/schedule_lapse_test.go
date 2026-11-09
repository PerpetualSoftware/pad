package store

import (
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

const lapseNow = "2026-10-26T12:00:00Z"

func lapseSchedule(t *testing.T, f *schedFixture, recurring bool) *models.ItemSchedule {
	t.Helper()
	sc := f.overdueSchedule(t, "Lapsing", "lapses")
	// Reported completion must be after creation, even in these historical fixtures.
	f.exec(t, `UPDATE item_schedules SET created_at=? WHERE id=?`, "2026-10-01T00:00:00Z", sc.ID)
	if !recurring {
		f.exec(t, `UPDATE item_schedules SET rrule=NULL,dtstart=? WHERE id=?`, "2026-10-26T09:00:00", sc.ID)
	}
	return f.reload(t, sc.ItemID)
}

func assertLapsed(t *testing.T, f *schedFixture, sc *models.ItemSchedule) {
	t.Helper()
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=? AND occurrence_at=? AND state='lapsed' AND actor='system' AND actual_done_at IS NULL`, sc.ID, deadlineOccAt); n != 1 {
		t.Fatalf("lapsed history rows = %d, want 1", n)
	}
	if f.deadlineEvidence(t) != 1 || f.deadlineNotices(t) != 1 {
		t.Fatalf("miss evidence/notices = %d/%d, want 1/1", f.deadlineEvidence(t), f.deadlineNotices(t))
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=? AND state='missed'`, sc.ID); n != 0 {
		t.Fatalf("legacy missed rows = %d, want none", n)
	}
}

func TestLapseOneOff(t *testing.T) {
	f := newSchedFixture(t)
	sc := lapseSchedule(t, f, false)
	f.eval(t, lapseNow, 0)
	got := f.reload(t, sc.ItemID)
	if got.EndedAt == nil || got.NextAt != nil || got.NextWall != nil {
		t.Fatalf("one-off did not end: %+v", got)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM item_reminders WHERE schedule_id=?`, sc.ID); n != 0 {
		t.Fatalf("reminders = %d", n)
	}
	assertLapsed(t, f, sc)
}

func TestLapseRecurringAndRestart(t *testing.T) {
	f := newSchedFixture(t)
	sc := lapseSchedule(t, f, true)
	for _, ts := range []string{lapseNow, "2026-10-26T12:01:00Z"} {
		f.eval(t, ts, 0)
	}
	got := f.reload(t, sc.ItemID)
	if got.NextAt == nil || *got.NextAt != "2026-10-27T09:00:00Z" || got.EndedAt != nil {
		t.Fatalf("next occurrence: %+v", got)
	}
	reminder := f.managed(t, got)
	if reminder.RemindAt != *got.NextAt || reminder.FiredAt != nil || reminder.AckedAt != nil {
		t.Fatalf("next reminder: %+v", reminder)
	}
	assertLapsed(t, f, sc)
	// A fresh Store has no evaluator memory. SQLite additionally closes and reopens the DB.
	restarted := &Store{db: f.s.db, dialect: f.s.dialect}
	if f.s.dialect.Driver() == DriverSQLite {
		path := f.s.dbPath
		if err := f.s.Close(); err != nil {
			t.Fatal(err)
		}
		var err error
		restarted, err = New(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { restarted.Close() })
	}
	f.s = restarted
	f.eval(t, "2026-10-26T12:02:00Z", 0)
	after := f.reload(t, sc.ItemID)
	if !reflect.DeepEqual(got, after) || !reflect.DeepEqual(reminder, f.managed(t, after)) {
		t.Fatal("restart advanced or rearmed twice")
	}
	assertLapsed(t, f, sc)
	entries, _, unavailable, err := f.s.ExpandAgenda(after, time.UTC, rt("2026-10-26T00:00:00Z"), rt("2026-10-28T00:00:00Z"), rt(lapseNow))
	if err != nil || unavailable != "" {
		t.Fatalf("agenda: %s %v", unavailable, err)
	}
	if len(entries) != 2 || entries[0].State != "lapsed" || entries[1].State != "outstanding" {
		t.Fatalf("agenda states: %+v", entries)
	}
	if n, _, err := ScheduleMissed(after, time.UTC, rt(lapseNow)); err != nil || n != 0 {
		t.Fatalf("missed count %d: %v", n, err)
	}
}

func TestLapseStaysOwed(t *testing.T) {
	f := newSchedFixture(t)
	sc := f.overdueSchedule(t, "Owed", "stays_owed")
	before := f.reload(t, sc.ItemID)
	reminder := f.managed(t, before)
	f.eval(t, lapseNow, 0)
	after := f.reload(t, sc.ItemID)
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(reminder, f.managed(t, after)) {
		t.Fatal("stays owed changed")
	}
	if f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=?`, sc.ID) != 0 || f.deadlineEvidence(t) != 1 {
		t.Fatal("stays owed history changed")
	}
}

func TestLapseLateDone(t *testing.T) {
	for _, recurring := range []bool{false, true} {
		for _, reported := range []bool{false, true} {
			t.Run(strconv.FormatBool(recurring)+"/"+strconv.FormatBool(reported), func(t *testing.T) {
				f := newSchedFixture(t)
				sc := lapseSchedule(t, f, recurring)
				f.eval(t, lapseNow, 0)
				before := f.reload(t, sc.ItemID)
				var reminder *models.Reminder
				if recurring {
					reminder = f.managed(t, before)
				}
				in := ResolveInput{OccurrenceAt: rt(deadlineOccAt), State: "done", Actor: "user"}
				actual, source := "2026-10-26T13:00:00Z", "recorded_now"
				if reported {
					actual, source = "2026-10-26T09:30:00Z", "reported"
					v := rt(actual)
					in.ActualDoneAt = &v
				}
				res, err := f.s.resolveOccurrenceAt(f.ws.ID, sc.ItemID, in, rt("2026-10-26T13:00:00Z"))
				if err != nil {
					t.Fatalf("late Done: %v", err)
				}
				if res.MissedRecorded != 0 || !reflect.DeepEqual(before, res.Schedule) {
					t.Fatal("late Done advanced schedule")
				}
				if recurring && !reflect.DeepEqual(reminder, f.managed(t, res.Schedule)) {
					t.Fatal("late Done touched reminder")
				}
				if n := f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=? AND occurrence_at=? AND state='done' AND actual_done_at=? AND actual_source=?`, sc.ID, deadlineOccAt, actual, source); n != 1 {
					t.Fatal("actual completion not recorded")
				}
				if f.deadlineEvidence(t) != 1 {
					t.Fatal("miss evidence lost")
				}
				wantCorrection := 0
				if reported {
					wantCorrection = 1
				}
				if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=? AND change_type='done' AND role='deadline'`, sc.ID); n != wantCorrection {
					t.Fatalf("corrections=%d, want %d", n, wantCorrection)
				}
			})
		}
	}
}

func TestLapseOtherResolutionsStale(t *testing.T) {
	f := newSchedFixture(t)
	sc := lapseSchedule(t, f, true)
	f.eval(t, lapseNow, 0)
	before := f.reload(t, sc.ItemID)
	reminder := f.managed(t, before)
	for _, state := range []string{"skip", "snooze", "move"} {
		in := ResolveInput{OccurrenceAt: rt(deadlineOccAt), State: state, Actor: "user"}
		if state != "skip" {
			until := rt("2026-10-26T15:00:00Z")
			in.Until = &until
		}
		_, err := f.s.resolveOccurrenceAt(f.ws.ID, sc.ItemID, in, rt(lapseNow))
		var stale *ErrStaleOccurrence
		if !errors.As(err, &stale) {
			t.Fatalf("%s: %v, want stale", state, err)
		}
	}
	if !reflect.DeepEqual(before, f.reload(t, sc.ItemID)) || !reflect.DeepEqual(reminder, f.managed(t, before)) {
		t.Fatal("stale resolution changed next occurrence")
	}
}

func TestLapseSkipReturnsLapse(t *testing.T) {
	f := newSchedFixture(t)
	sc := lapseSchedule(t, f, true)
	res, err := f.s.resolveOccurrenceAt(f.ws.ID, sc.ItemID, ResolveInput{OccurrenceAt: rt(deadlineOccAt), State: "skip", Actor: "user"}, rt(lapseNow))
	if err != nil {
		t.Fatal(err)
	}
	assertLapsed(t, f, sc)
	if res.Schedule.NextAt == nil || *res.Schedule.NextAt != "2026-10-27T09:00:00Z" {
		t.Fatalf("skip advanced twice: %+v", res.Schedule)
	}
	if f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=?`, sc.ID) != 1 {
		t.Fatal("skip closed another occurrence")
	}
}

func TestLapseRenegotiateCommitsLapseAndRefusesChange(t *testing.T) {
	for _, scope := range []string{"occurrence", "series"} {
		t.Run(scope, func(t *testing.T) {
			f := newSchedFixture(t)
			sc := lapseSchedule(t, f, true)
			in := models.CommitmentRenegotiateInput{Scope: scope, Reason: "Plans changed", Preview: true}
			if scope == "occurrence" {
				in.Deadline = ptr("2026-10-28T10:00:00Z")
			} else {
				in.DeadlineBehavior = ptr("stays_owed")
			}
			p, err := f.s.renegotiateCommitmentAt(f.ws.ID, sc.ItemID, "user", in, rt(lapseNow), nil)
			if err != nil || !p.Preview.MissRecordedFirst {
				t.Fatalf("preview: %+v %v", p, err)
			}
			if f.deadlineEvidence(t) != 0 {
				t.Fatal("preview wrote miss")
			}
			in.Preview = false
			in.Revision = p.Revision
			_, err = f.s.renegotiateCommitmentAt(f.ws.ID, sc.ItemID, "user", in, rt(lapseNow), nil)
			if !errors.Is(err, ErrCommitmentChanged) {
				t.Fatalf("apply = %v, want commitment changed", err)
			}
			assertLapsed(t, f, sc)
			after := f.reload(t, sc.ItemID)
			if after.NextAt == nil || *after.NextAt != "2026-10-27T09:00:00Z" {
				t.Fatalf("renegotiate advanced twice: %+v", after)
			}
			if f.count(t, `SELECT COUNT(*) FROM schedule_occurrence_overrides WHERE schedule_id=?`, sc.ID) != 0 || f.count(t, `SELECT COUNT(*) FROM item_schedules WHERE id=? AND deadline_behavior='lapses'`, sc.ID) != 1 {
				t.Fatal("refused change applied")
			}
			_, err = f.s.renegotiateCommitmentAt(f.ws.ID, sc.ItemID, "user", in, rt(lapseNow), nil)
			if !errors.Is(err, ErrCommitmentChanged) {
				t.Fatalf("retry: %v", err)
			}
			if !reflect.DeepEqual(after, f.reload(t, sc.ItemID)) {
				t.Fatal("retry advanced twice")
			}
		})
	}
}

func TestLapseSkippedAndTerminalDoNotMiss(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(strconv.FormatBool(terminal), func(t *testing.T) {
			f := newSchedFixture(t)
			sc := lapseSchedule(t, f, true)
			if terminal {
				f.exec(t, `UPDATE items SET fields=? WHERE id=?`, `{"status":"done"}`, sc.ItemID)
			} else {
				_, err := f.s.resolveOccurrenceAt(f.ws.ID, sc.ItemID, ResolveInput{OccurrenceAt: rt(deadlineOccAt), State: "skip", Actor: "user", MaySkipBeforeDeadline: true}, rt("2026-10-26T09:30:00Z"))
				if err != nil {
					t.Fatal(err)
				}
			}
			f.eval(t, lapseNow, 0)
			if f.deadlineEvidence(t) != 0 || f.deadlineNotices(t) != 0 {
				t.Fatal("closed item or skipped occurrence recorded miss")
			}
		})
	}
}

func TestLapseMigrationPreservesHistory(t *testing.T) {
	db, rest := preMigrationDB(t, "172")
	s := &Store{db: db, dialect: &sqliteDialect{}}
	ids := commitmentSeed(t, s)
	s.mustExec(t, `UPDATE schedule_occurrences SET actual_done_at=?,actual_source='reported',comment_id='preserved-comment' WHERE schedule_id=?`, "2026-10-28T09:30:00Z", ids[0])
	before := commitmentOldRows(t, s)
	if len(rest) == 0 {
		t.Fatal("lapse migration missing")
	}
	for _, name := range rest {
		b, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := applySQLiteMigration(db, name, string(b)); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(before, commitmentOldRows(t, s)) {
		t.Fatal("migration changed existing rows")
	}
	if n := s.countRows(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=? AND actual_done_at=? AND actual_source='reported' AND comment_id='preserved-comment'`, ids[0], "2026-10-28T09:30:00Z"); n != 1 {
		t.Fatal("migration lost actual completion or comment")
	}
	s.mustExec(t, `UPDATE schedule_occurrences SET state='lapsed',actor='system' WHERE schedule_id=?`, ids[0])
	for _, col := range []string{"actor", "occurrence_wall"} {
		if _, err := db.Exec(`UPDATE schedule_occurrences SET `+col+`=? WHERE schedule_id=?`, "bad\x00text", ids[0]); err == nil {
			t.Fatalf("lost NUL guard on %s", col)
		}
	}
	if _, err := db.Exec(`INSERT INTO schedule_occurrences SELECT 'duplicate',schedule_id,occurrence_at,occurrence_wall,state,comment_id,actor,resolved_at,actual_source,actual_done_at FROM schedule_occurrences WHERE schedule_id=?`, ids[0]); err == nil {
		t.Fatal("lost instant uniqueness")
	}
	s.mustExec(t, `UPDATE schedule_occurrences SET occurrence_wall='2026-10-28' WHERE schedule_id=?`, ids[0])
	if _, err := db.Exec(`INSERT INTO schedule_occurrences SELECT 'duplicate-wall',schedule_id,'2026-10-28T10:00:00Z',occurrence_wall,state,comment_id,actor,resolved_at,actual_source,actual_done_at FROM schedule_occurrences WHERE schedule_id=?`, ids[0]); err == nil {
		t.Fatal("lost all-day uniqueness")
	}
	s.mustExec(t, `DELETE FROM item_schedules WHERE id=?`, ids[0])
	if n := s.countRows(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=?`, ids[0]); n != 0 {
		t.Fatal("lost cascading FK")
	}
}

func TestLapseCatchUpPreservesChildrenOverridesAndHistory(t *testing.T) {
	f := newSchedFixture(t)
	sc := lapseSchedule(t, f, true)
	child := f.another(t, "Child promise")
	f.exec(t, `UPDATE items SET parent_id=? WHERE id=?`, sc.ItemID, child.ID)
	childSc, err := f.s.createScheduleAt(f.ws.ID, child.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-10T09:00", Tz: "UTC"}, schedNow)
	if err != nil {
		t.Fatal(err)
	}
	childBefore := f.reload(t, child.ID)
	childReminder := f.managed(t, childBefore)
	f.deadlineOverride(t, sc, "2026-10-26T10:00:00Z")
	before, err := f.s.scheduleOverridesQ(f.s.db, f.ws.ID, sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, `INSERT INTO schedule_occurrences(id,schedule_id,occurrence_at,occurrence_wall,state,actor,resolved_at,actual_source) VALUES(?,?,?,?,'skipped','user',?,'legacy_unknown')`, newID(), sc.ID, "2026-10-25T09:00:00Z", "2026-10-25T09:00:00", lapseNow)
	history := f.occs(t, sc.ID)
	if got := f.eval(t, "2026-10-29T12:00:00Z", 0); got.Missed != 1 {
		t.Fatalf("catch-up: %+v", got)
	}
	next := f.reload(t, sc.ItemID)
	if next.NextAt == nil || *next.NextAt != "2026-10-30T09:00:00Z" {
		t.Fatalf("catch-up did not advance to future: %+v", next)
	}
	if f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=? AND occurrence_at=? AND state='lapsed'`, sc.ID, deadlineOccAt) != 1 || f.deadlineEvidence(t) != 1 {
		t.Fatal("catch-up lost lapse or miss evidence")
	}
	f.eval(t, "2026-10-29T12:01:00Z", 0)
	if f.deadlineEvidence(t) != 1 || !reflect.DeepEqual(next, f.reload(t, sc.ItemID)) {
		t.Fatal("catch-up advanced twice")
	}
	after, err := f.s.scheduleOverridesQ(f.s.db, f.ws.ID, sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(history[0], f.occs(t, sc.ID)[0]) {
		t.Fatal("lapse changed overrides or existing history")
	}
	if !reflect.DeepEqual(childBefore, f.reload(t, child.ID)) || !reflect.DeepEqual(childReminder, f.managed(t, childSc)) {
		t.Fatal("lapse changed child schedule or reminder")
	}
}

func TestLapseRollbackKeepsMissAndAdvanceAtomic(t *testing.T) {
	f := newSchedFixture(t)
	if f.s.dialect.Driver() != DriverSQLite {
		t.Skip("SQLite failure injection")
	}
	sc := lapseSchedule(t, f, true)
	before := f.reload(t, sc.ItemID)
	reminder := f.managed(t, before)
	f.exec(t, `CREATE TRIGGER fail_lapse_reminder BEFORE UPDATE ON item_reminders BEGIN SELECT RAISE(ABORT,'injected reminder failure'); END`)
	_, err := f.s.EvaluateDeadlines(lapseNow, 0)
	if err == nil {
		t.Fatal("expected reminder failure")
	}
	if f.deadlineEvidence(t) != 0 || f.deadlineNotices(t) != 0 || f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=?`, sc.ID) != 0 {
		t.Fatal("failed advance committed part of lapse")
	}
	if !reflect.DeepEqual(before, f.reload(t, sc.ItemID)) || !reflect.DeepEqual(reminder, f.managed(t, before)) {
		t.Fatal("failed advance changed schedule or reminder")
	}
	f.exec(t, `DROP TRIGGER fail_lapse_reminder`)
	f.eval(t, lapseNow, 0)
	assertLapsed(t, f, sc)
}

func TestLapseExportImportKeepsClosedHistory(t *testing.T) {
	f := newSchedFixture(t)
	sc := lapseSchedule(t, f, true)
	f.eval(t, lapseNow, 0)
	data := f.exportBundle(t)
	ws, report := f.importBundle(t, data, "Lapsed copy")
	if len(report.DroppedSchedules) != 0 {
		t.Fatalf("lapsed schedule dropped: %+v", report)
	}
	item := f.itemByTitle(t, ws.ID, "Lapsing")
	got, err := f.s.GetSchedule(ws.ID, item.ID)
	if err != nil || got == nil {
		t.Fatalf("imported schedule: %+v %v", got, err)
	}
	if !reflect.DeepEqual(f.occs(t, sc.ID), f.occs(t, got.ID)) {
		t.Fatal("import lost lapsed history")
	}
}
