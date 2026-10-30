package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4103 C2b-ii r1 fixes. Each test is red at 96f31bd41.

// r1-1: done and skip record X at the RECIPIENT's reading. The caller's zone
// only gates acceptance. RED if the row stores the matched caller reading.
func TestResolveDoneRecordsRecipientReading(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	la := f.user(t, "r1-1-la@example.com", "America/Los_Angeles")
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"done", "skip"} {
		item, sc := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Floating: true, Rrule: "FREQ=DAILY"}, "2026-10-29T12:00:00Z")
		f.assign(t, item.ID, la.ID)
		// 09:00 in Tokyo is 00:00Z; the recipient's 09:00 is 17:00Z.
		f.mustResolve(t, item, ResolveInput{OccurrenceAt: rt("2026-11-02T00:00:00Z"), State: state, Actor: "user", View: tokyo}, "2026-10-29T12:00:00Z")
		var at string
		if err := f.s.db.QueryRow(f.s.q(`SELECT occurrence_at FROM schedule_occurrences WHERE schedule_id = ? AND state IN ('done','skipped')`), sc.ID).Scan(&at); err != nil {
			t.Fatalf("%s: occurrence row: %v", state, err)
		}
		if rt(at) != rt("2026-11-02T17:00:00Z") {
			t.Errorf("%s: occurrence_at = %s, want the recipient reading 2026-11-02T17:00:00Z", state, at)
		}
	}
}

// r1-3: a pinned HOURLY completion in the fall-back hour is exact: done at
// 01:30 EDT plus 1h is 01:30 EST (06:30Z), never at or before the done instant.
func TestCompletionHourlyFallBackIsExact(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	item, _ := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-01T01:30", Tz: "America/New_York", Rrule: "FREQ=HOURLY", Anchor: "completion"}, "2026-10-29T12:00:00Z")
	res := f.mustResolve(t, item, ResolveInput{OccurrenceAt: rt("2026-11-01T05:30:00Z"), State: "done", Actor: "user"}, "2026-11-01T05:30:00Z")
	if res.Schedule.NextAt == nil || *res.Schedule.NextAt != "2026-11-01T06:30:00Z" {
		t.Fatalf("next_at = %v, want 2026-11-01T06:30:00Z", deref(res.Schedule.NextAt))
	}
}

// r1-4: an all-day upsert whose instant matches one row and whose date matches
// another writes one of them and never violates the wall unique index.
func TestUpsertAllDayMatchesTwoRows(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	_, sc := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02", Tz: "UTC", Rrule: "FREQ=DAILY"}, "2026-10-29T12:00:00Z")
	ins := `INSERT INTO schedule_occurrences (id, schedule_id, occurrence_at, occurrence_wall, state, comment_id, actor, resolved_at) VALUES (?, ?, ?, ?, 'missed', NULL, '', ?)`
	f.exec(t, ins, newID(), sc.ID, "2026-11-03T00:00:00Z", "2026-11-04", "2026-11-05T00:00:00Z")
	f.exec(t, ins, newID(), sc.ID, "2026-11-03T10:00:00Z", "2026-11-03", "2026-11-05T00:00:00Z")
	tx, err := f.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := f.s.upsertOccurrenceTx(tx, sc.ID, "2026-11-03T00:00:00Z", "2026-11-03", "done", "", "user", "2026-11-05T00:00:00Z"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if n := f.occTotal(t, sc.ID); n != 2 {
		t.Errorf("rows = %d, want 2", n)
	}
	if n := f.occRows(t, sc.ID, "done"); n != 1 {
		t.Errorf("done rows = %d, want 1", n)
	}
}

// Q1: done on a calendar rule re-anchors like the fire does, so a sparse rule
// that is always resolved early keeps its scan origin moving instead of walking
// from dtstart into cap_scan.
func TestResolveEarlyOnSparseCalendarReanchors(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	item, sc := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2000-01-01T09:00", Tz: "UTC", Rrule: "FREQ=MINUTELY;BYHOUR=9"}, "2000-01-01T00:00:00Z")
	f.exec(t, `UPDATE item_schedules SET next_at = ?, next_wall = ? WHERE id = ?`, "2026-10-28T09:00:00Z", "2026-10-28T09:00:00", sc.ID)
	x := rt("2026-10-28T09:00:00Z")
	for i := 0; i < 3; i++ {
		res := f.mustResolve(t, item, ResolveInput{OccurrenceAt: x, State: "done", Actor: "user"}, "2026-10-28T08:00:00Z")
		want := x.Add(time.Minute)
		got := res.Schedule
		if got.NextAt == nil || rt(*got.NextAt) != want {
			t.Fatalf("round %d: next_at = %v, want %s", i, deref(got.NextAt), want.Format(time.RFC3339))
		}
		if got.AnchorWall == nil || *got.AnchorWall != x.Format("2006-01-02T15:04:05") {
			t.Fatalf("round %d: anchor_wall = %v, want X's wall %s", i, deref(got.AnchorWall), x.Format("2006-01-02T15:04:05"))
		}
		x = want
	}
}
