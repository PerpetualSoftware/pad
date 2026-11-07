package store

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/kernelevents"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4351 PR-B: the deadline evaluator. Derived per tick, no stored state of
// its own: the miss evidence is the journal's missed_recorded row for role
// 'deadline' and the occurrence key, written in the same transaction as the
// notice. Runs on SQLite in make test and on Postgres in make test-pg.
//
// Written first and red: EvaluateDeadlines, DeadlineEvalResult and
// kernelevents.ItemDeadlineMissed do not exist yet.

const deadlineOccAt = "2026-10-26T09:00:00Z"

// nativeDeadline makes sc a native-role schedule whose deadline is secs after
// its outstanding occurrence, with the occurrence forced to deadlineOccAt.
func (f *schedFixture) nativeDeadline(t *testing.T, sc *models.ItemSchedule, behavior string, secs int) {
	t.Helper()
	f.exec(t, `UPDATE item_schedules SET roles_origin='native', deadline_days=0, deadline_secs=?, deadline_behavior=?, next_at=?, next_wall=? WHERE id=?`,
		secs, behavior, deadlineOccAt, "2026-10-26T09:00:00", sc.ID)
}

func (f *schedFixture) eval(t *testing.T, nowTS string, limit int) DeadlineEvalResult {
	t.Helper()
	res, err := f.s.EvaluateDeadlines(nowTS, limit)
	if err != nil {
		t.Fatalf("EvaluateDeadlines(%s): %v", nowTS, err)
	}
	return res
}

func (f *schedFixture) deadlineEvidence(t *testing.T) int {
	t.Helper()
	return f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE change_type='missed_recorded' AND role='deadline'`)
}

func (f *schedFixture) deadlineNotices(t *testing.T) int {
	t.Helper()
	return f.count(t, `SELECT COUNT(*) FROM event_outbox WHERE event_type = ?`, kernelevents.ItemDeadlineMissed)
}

func (f *schedFixture) overdueSchedule(t *testing.T, title, behavior string) *models.ItemSchedule {
	t.Helper()
	it := f.another(t, title)
	sc, err := f.s.createScheduleAt(f.ws.ID, it.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}, schedNow)
	if err != nil {
		t.Fatal(err)
	}
	f.nativeDeadline(t, sc, behavior, 3600)
	return sc
}

// A deadline that passed records one miss and one notice, and no later tick,
// for any behavior, adds another.
func TestDeadlineEvalRecordsAMissOnceAcrossTicks(t *testing.T) {
	t.Parallel()
	for _, behavior := range []string{"stays_owed", "lapses"} {
		t.Run(behavior, func(t *testing.T) {
			f := newSchedFixture(t)
			sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})
			f.nativeDeadline(t, sc, behavior, 3600)

			if got := f.eval(t, "2026-10-26T09:59:59Z", 0); got.Missed != 0 {
				t.Fatalf("before the deadline: missed %d, want 0", got.Missed)
			}
			if got := f.eval(t, "2026-10-26T10:00:01Z", 0); got.Missed != 1 {
				t.Fatalf("after the deadline: missed %d, want 1", got.Missed)
			}
			for _, now := range []string{"2026-10-26T10:00:31Z", "2026-10-27T00:00:00Z"} {
				if got := f.eval(t, now, 0); got.Missed != 0 {
					t.Errorf("tick at %s missed %d, want 0", now, got.Missed)
				}
			}
			if n := f.deadlineEvidence(t); n != 1 {
				t.Errorf("miss evidence rows = %d, want 1", n)
			}
			if n := f.deadlineNotices(t); n != 1 {
				t.Errorf("miss notices = %d, want 1", n)
			}
		})
	}
}

// The first tick after the migration records nothing for a row already past
// its deadline: the backfilled roles are not native, so no miss or notice is
// derived from them.
func TestDeadlineEvalSkipsARowPastItsDeadlineAtMigration(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{Kind: "deadline_hard", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})
	f.exec(t, `UPDATE item_schedules SET roles_origin='legacy_backfill', deadline_days=0, deadline_secs=0, deadline_behavior='stays_owed', next_at=?, next_wall=? WHERE id=?`,
		deadlineOccAt, "2026-10-26T09:00:00", sc.ID)

	got := f.eval(t, "2026-10-29T12:00:00Z", 0)
	if got.Missed != 0 || f.deadlineEvidence(t) != 0 || f.deadlineNotices(t) != 0 {
		t.Errorf("first tick: missed %d, evidence %d, notices %d; want none", got.Missed, f.deadlineEvidence(t), f.deadlineNotices(t))
	}
}

// More overdue rows than the scan cap: every pass takes at most the cap, reports
// that it was cut short, and a later pass reaches the rest, so no hard deadline
// is starved by older ones. Each row is missed exactly once.
func TestDeadlineEvalScanCapDoesNotStarve(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	const rows, cap = 5, 2
	for i := 0; i < rows; i++ {
		f.overdueSchedule(t, fmt.Sprintf("Row %d", i), "stays_owed")
	}
	now := "2026-10-26T12:00:00Z"
	missed, passes := 0, 0
	for ; passes < rows; passes++ {
		got := f.eval(t, now, cap)
		if got.Missed > cap {
			t.Fatalf("pass %d missed %d, over the cap %d", passes, got.Missed, cap)
		}
		missed += got.Missed
		if missed < rows && !got.CapHit {
			t.Fatalf("pass %d left rows unevaluated without reporting a cap hit", passes)
		}
		if got.Missed == 0 {
			break
		}
	}
	if missed != rows {
		t.Fatalf("missed %d of %d rows over %d passes", missed, rows, passes)
	}
	if n := f.deadlineEvidence(t); n != rows {
		t.Errorf("evidence rows = %d, want %d", n, rows)
	}
	if n := f.deadlineNotices(t); n != rows {
		t.Errorf("notices = %d, want %d", n, rows)
	}
}

// Rows already missed do not occupy the cap: with the cap at 1, a row past its
// deadline behind many recorded ones is still reached on the next pass.
func TestDeadlineEvalRecordedRowsDoNotOccupyTheCap(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	for i := 0; i < 3; i++ {
		f.overdueSchedule(t, fmt.Sprintf("Old %d", i), "stays_owed")
	}
	for i := 0; i < 3; i++ {
		f.eval(t, "2026-10-26T12:00:00Z", 1)
	}
	f.overdueSchedule(t, "Late arrival", "lapses")
	if got := f.eval(t, "2026-10-26T12:00:00Z", 1); got.Missed != 1 {
		t.Errorf("late row: missed %d, want 1 on the first pass after it", got.Missed)
	}
}

// Two evaluators racing over the same rows (two ticks, or two instances on one
// Postgres) record each miss and send each notice once.
func TestDeadlineEvalConcurrentPassesRecordOnce(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	const rows = 4
	for i := 0; i < rows; i++ {
		f.overdueSchedule(t, fmt.Sprintf("Race %d", i), "stays_owed")
	}
	var wg sync.WaitGroup
	results := make(chan DeadlineEvalResult, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A losing pass may report a busy row; the count below is the verdict.
			result, _ := f.s.EvaluateDeadlines("2026-10-26T12:00:00Z", 0)
			results <- result
		}()
	}
	wg.Wait()
	close(results)
	total := 0
	for result := range results {
		total += result.Missed
	}
	if n := f.deadlineEvidence(t); total != n {
		t.Fatalf("reported misses = %d, committed rows = %d", total, n)
	}
	f.eval(t, "2026-10-26T12:00:30Z", 0)
	if n := f.deadlineEvidence(t); n != rows {
		t.Errorf("evidence rows = %d, want %d", n, rows)
	}
	if n := f.deadlineNotices(t); n != rows {
		t.Errorf("notices = %d, want %d", n, rows)
	}
}

func (f *schedFixture) deadlineOverride(t *testing.T, sc *models.ItemSchedule, value string) {
	t.Helper()
	sc = f.reload(t, sc.ItemID)
	key := OccurrenceKey(sc, *sc.NextAt, *sc.NextWall)
	f.exec(t, `INSERT INTO schedule_occurrence_overrides(id,schedule_id,occurrence_key,role,value_at,created_at,updated_at) VALUES(?,?,?,'deadline',?,?,?) ON CONFLICT(schedule_id,occurrence_key,role) DO UPDATE SET value_at=excluded.value_at`, newID(), sc.ID, key, value, now(), now())
}

func TestDeadlineEvalRenegotiationAndCanonicalEvidence(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.overdueSchedule(t, "Renegotiate", "stays_owed")
	if got := f.eval(t, "2026-10-26T10:00:01Z", 0); got.Missed != 1 {
		t.Fatalf("first miss: %+v", got)
	}
	f.deadlineOverride(t, sc, "2026-10-27T14:00:00+02:00")
	if got := f.eval(t, "2026-10-27T11:59:59Z", 0); got.Missed != 0 {
		t.Fatalf("deferred: %+v", got)
	}
	if got := f.eval(t, "2026-10-27T12:00:01Z", 0); got.Missed != 1 {
		t.Fatalf("second miss: %+v", got)
	}
	if got := f.eval(t, "2026-10-28T00:00:00Z", 0); got.Missed != 0 {
		t.Fatalf("repeat: %+v", got)
	}
	for _, value := range []string{"2026-10-26T10:00:00Z", "2026-10-27T12:00:00Z"} {
		after, _ := json.Marshal(map[string]string{"deadline": value})
		if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=? AND occurrence_key=? AND role='deadline' AND change_type='missed_recorded' AND after_json=?`, sc.ID, deadlineOccAt+"|2026-10-26T09:00:00", string(after)); n != 1 {
			t.Fatalf("evidence for %s = %d", value, n)
		}
	}
	if f.deadlineEvidence(t) != 2 || f.deadlineNotices(t) != 2 {
		t.Fatal("want two journal rows and events")
	}
}

func TestDeadlineEvalOverrideDefersStoredDeadline(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.overdueSchedule(t, "Deferred", "lapses")
	f.deadlineOverride(t, sc, "2026-10-28T10:00:00Z")
	if got := f.eval(t, "2026-10-26T12:00:00Z", 1); got.Missed != 0 || got.CapHit {
		t.Fatalf("early: %+v", got)
	}
	if got := f.eval(t, "2026-10-28T10:00:01Z", 1); got.Missed != 1 {
		t.Fatalf("late: %+v", got)
	}
}

func TestDeadlineEvalPagesPastNotYetDueRows(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	// Date-only rows sort before the timed row by approximation, but expire later.
	for i := 0; i < 205; i++ {
		sc := f.overdueSchedule(t, fmt.Sprintf("Future %03d", i), "stays_owed")
		f.exec(t, `UPDATE item_schedules SET next_at='2026-10-26T00:00:00Z',next_wall='2026-10-26',dtstart='2026-10-01',deadline_secs=0 WHERE id=?`, sc.ID)
	}
	due := f.overdueSchedule(t, "Due behind future rows", "stays_owed")
	got := f.eval(t, "2026-10-26T12:00:00Z", 1)
	if got.Missed != 1 || got.Evaluated != 1 || !got.CapHit {
		t.Fatalf("paged result: %+v", got)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=? AND role='deadline' AND change_type='missed_recorded'`, due.ID); n != 1 {
		t.Fatal("due row behind page was not reached")
	}
}

func TestDeadlineEvalDateOnlyLocalEndOfDay(t *testing.T) {
	t.Parallel()
	for _, override := range []bool{false, true} {
		t.Run(fmt.Sprint(override), func(t *testing.T) {
			f := newSchedFixture(t)
			sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02", Tz: "America/New_York"})
			f.exec(t, `UPDATE item_schedules SET roles_origin='native',deadline_days=0,deadline_secs=0,deadline_behavior='stays_owed' WHERE id=?`, sc.ID)
			if override {
				f.deadlineOverride(t, sc, "2026-11-02")
			}
			if got := f.eval(t, "2026-11-03T04:59:59Z", 0); got.Missed != 0 {
				t.Fatalf("before local end: %+v", got)
			}
			if got := f.eval(t, "2026-11-03T05:00:00Z", 0); got.Missed != 1 {
				t.Fatalf("at next midnight: %+v", got)
			}
			if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE role='deadline' AND after_json=?`, `{"deadline":"2026-11-02"}`); n != 1 {
				t.Fatal("date-only canonical value missing")
			}
		})
	}
}

func TestDeadlineEvalFloatingRecipientZone(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	u := f.user(t, "deadline-floating@example.com", "America/Los_Angeles")
	f.assign(t, f.item.ID, u.ID)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Floating: true})
	f.exec(t, `UPDATE item_schedules SET roles_origin='native',deadline_days=0,deadline_secs=3600,deadline_behavior='stays_owed' WHERE id=?`, sc.ID)
	if got := f.eval(t, "2026-11-02T17:59:59Z", 0); got.Missed != 0 {
		t.Fatalf("early bound: %+v", got)
	}
	if got := f.eval(t, "2026-11-02T18:00:01Z", 0); got.Missed != 1 {
		t.Fatalf("recipient instant: %+v", got)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE role='deadline' AND occurrence_key='2026-11-02T09:00:00' AND after_json=?`, `{"deadline":"2026-11-02T18:00:00Z"}`); n != 1 {
		t.Fatal("floating evidence mismatch")
	}
}

func TestDeadlineEvalConversionGuard(t *testing.T) {
	t.Parallel()
	for _, stamp := range []string{"2026-10-26T10:00:00Z", "2026-10-26T10:00:00.1Z", "2026-10-26T12:00:00+02:00"} {
		t.Run(stamp, func(t *testing.T) {
			f := newSchedFixture(t)
			sc := f.overdueSchedule(t, "Converted", "stays_owed")
			f.exec(t, `UPDATE item_schedules SET roles_origin='legacy_backfill' WHERE id=?`, sc.ID)
			tx, err := f.s.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if err = f.s.pinScheduleTx(tx, f.ws.ID, sc.ItemID); err != nil {
				t.Fatal(err)
			}
			before, err := f.s.scheduleSnapshotTx(tx, sc.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec(f.s.q(`UPDATE item_schedules SET roles_origin='native' WHERE id=?`), sc.ID); err != nil {
				t.Fatal(err)
			}
			if err = f.s.journalScheduleAfterTx(tx, sc.ID, "update", "", "", before, "system"); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}
			f.exec(t, `UPDATE schedule_changes SET recorded_at=? WHERE schedule_id=? AND change_type='update'`, stamp, sc.ID)
			if got := f.eval(t, "2026-10-26T12:00:00Z", 0); got.Missed != 0 || got.CapHit || f.deadlineEvidence(t) != 0 || f.deadlineNotices(t) != 0 {
				t.Fatalf("converted overdue: %+v", got)
			}
			f.deadlineOverride(t, sc, "2026-10-27T10:00:00Z")
			if got := f.eval(t, "2026-10-27T10:00:01Z", 0); got.Missed != 1 {
				t.Fatalf("renegotiated: %+v", got)
			}
		})
	}
}

func TestDeadlineEvalResolvedBeforeDeadline(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"done", "skip"} {
		t.Run(state, func(t *testing.T) {
			f := newSchedFixture(t)
			sc := f.overdueSchedule(t, "Resolve", "stays_owed")
			_, err := f.s.resolveOccurrenceAt(f.ws.ID, sc.ItemID, ResolveInput{State: state, OccurrenceAt: rt(deadlineOccAt), View: time.UTC, Actor: "user"}, rt("2026-10-26T09:30:00Z"))
			if err != nil {
				t.Fatal(err)
			}
			if got := f.eval(t, "2026-10-26T10:00:01Z", 0); got.Missed != 0 || f.deadlineEvidence(t) != 0 || f.deadlineNotices(t) != 0 {
				t.Fatalf("resolved: %+v", got)
			}
		})
	}
}

func TestDeadlineEvalArchivedRows(t *testing.T) {
	t.Parallel()
	for _, table := range []string{"items", "workspaces"} {
		t.Run(table, func(t *testing.T) {
			f := newSchedFixture(t)
			sc := f.overdueSchedule(t, "Archived", "stays_owed")
			id := sc.ItemID
			if table == "workspaces" {
				id = f.ws.ID
			}
			f.exec(t, `UPDATE `+table+` SET deleted_at=? WHERE id=?`, now(), id)
			if got := f.eval(t, "2026-10-26T12:00:00Z", 0); got.Missed != 0 || f.deadlineEvidence(t) != 0 || f.deadlineNotices(t) != 0 {
				t.Fatalf("archived: %+v", got)
			}
		})
	}
}

func TestDeadlineEvalNoNotificationsAndItemEvent(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.overdueSchedule(t, "No notification", "stays_owed")
	u := f.user(t, "deadline-no-notification@example.com", "UTC")
	f.assign(t, sc.ItemID, u.ID)
	addDevice(t, f.s, u.ID)
	before := f.count(t, `SELECT COUNT(*) FROM user_notifications`)
	if got := f.eval(t, "2026-10-26T12:00:00Z", 0); got.Missed != 1 {
		t.Fatalf("miss: %+v", got)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM user_notifications`); n != before {
		t.Fatalf("notifications changed %d -> %d", before, n)
	}
	var subject, kind, payload string
	if err := f.s.db.QueryRow(f.s.q(`SELECT subject_id,subject_kind,payload FROM event_outbox WHERE event_type=?`), kernelevents.ItemDeadlineMissed).Scan(&subject, &kind, &payload); err != nil {
		t.Fatal(err)
	}
	if subject != sc.ItemID || kind != kernelevents.SubjectItem {
		t.Fatalf("event subject/kind: %s %s", subject, kind)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(payload), &body); err != nil {
		t.Fatal(err)
	}
	if body["id"] != sc.ItemID {
		t.Fatalf("item snapshot missing: %s", payload)
	}
}

func TestDeadlineEvalLegacyMissedStateDoesNotSuppress(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.overdueSchedule(t, "Legacy missed", "stays_owed")
	f.exec(t, `INSERT INTO schedule_occurrences(id,schedule_id,occurrence_at,occurrence_wall,state,actor,resolved_at) VALUES(?,?,?,?,'missed','',?)`, newID(), sc.ID, deadlineOccAt, "2026-10-26T09:00:00", now())
	if got := f.eval(t, "2026-10-26T12:00:00Z", 0); got.Missed != 1 {
		t.Fatalf("legacy state: %+v", got)
	}
	if f.deadlineEvidence(t) != 1 || f.deadlineNotices(t) != 1 {
		t.Fatal("deadline journal/event missing")
	}
}

func TestDeadlineEvalBadRowDoesNotStopPass(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	bad := f.overdueSchedule(t, "Bad zone", "stays_owed")
	f.exec(t, `UPDATE item_schedules SET tz='Invalid/Zone', next_at='2026-10-25T09:00:00Z' WHERE id=?`, bad.ID)
	f.overdueSchedule(t, "Good row", "stays_owed")
	got, err := f.s.EvaluateDeadlines("2026-10-26T12:00:00Z", 0)
	if err == nil || got.Missed != 1 {
		t.Fatalf("result=%+v error=%v", got, err)
	}
}

func TestDeadlineEvalSeedDisplacementAcrossDST(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-31T09:00", Tz: "America/New_York"})
	f.exec(t, `UPDATE item_schedules SET roles_origin='native',target_days=5,target_secs=3600,deadline_days=1,deadline_secs=0,deadline_behavior='stays_owed' WHERE id=?`, sc.ID)
	if got := f.eval(t, "2026-11-01T13:59:59Z", 0); got.Missed != 0 {
		t.Fatalf("before civil deadline: %+v", got)
	}
	if got := f.eval(t, "2026-11-01T14:00:01Z", 0); got.Missed != 1 {
		t.Fatalf("after civil deadline: %+v", got)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE role='deadline' AND after_json=?`, `{"deadline":"2026-11-01T14:00:00Z"}`); n != 1 {
		t.Fatal("seed-relative deadline mismatch")
	}
}
