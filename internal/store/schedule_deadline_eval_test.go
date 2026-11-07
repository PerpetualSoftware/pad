package store

import (
	"fmt"
	"sync"
	"testing"

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
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A losing pass may report a busy row; the count below is the verdict.
			_, _ = f.s.EvaluateDeadlines("2026-10-26T12:00:00Z", 0)
		}()
	}
	wg.Wait()
	f.eval(t, "2026-10-26T12:00:30Z", 0)
	if n := f.deadlineEvidence(t); n != rows {
		t.Errorf("evidence rows = %d, want %d", n, rows)
	}
	if n := f.deadlineNotices(t); n != rows {
		t.Errorf("notices = %d, want %d", n, rows)
	}
}
