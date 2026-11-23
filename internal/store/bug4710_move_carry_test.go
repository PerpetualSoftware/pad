package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-4710: a cross-workspace MOVE carries the pending occurrence's target
// override and next_override_at, so it must carry the occurrence's `moved`
// history row too. Without it the destination reads the move as a snooze
// (snoozed_until = target - lead) and its agenda shows no move.

const (
	b4710Created = "2026-10-29T10:00:00Z" // the 29th 09:00 has passed: D0 is the 30th
	b4710Moved   = "2026-10-29T11:00:00Z" // the remind move, before copyNow
	b4710D1      = "2026-10-31T10:00:00Z" // the moved target
)

// b4710Source creates the source schedule at created and moves its outstanding
// occurrence to D1 at b4710Moved. One older `done` row is history the move
// must still drop.
func b4710Source(t *testing.T, c *schedCopy, in models.ScheduleCreate, created string) *models.ItemSchedule {
	t.Helper()
	sc, err := c.s.createScheduleAt(c.wsA.ID, c.src.ID, in, rt(created))
	if err != nil {
		t.Fatal(err)
	}
	c.exec(t, `INSERT INTO schedule_occurrences (id, schedule_id, occurrence_at, occurrence_wall, state, comment_id, actor, resolved_at)
		VALUES (?, ?, ?, ?, 'done', NULL, 'user', ?)`, newID(), sc.ID, "2026-10-01T09:00:00Z", "2026-10-01T09:00:00", "2026-10-01T10:00:00Z")
	occ, ok, err := OutstandingInstant(sc, time.UTC)
	if err != nil || !ok {
		t.Fatalf("outstanding instant: %v %v", ok, err)
	}
	b4710Resolve(t, c, ResolveInput{OccurrenceAt: occ, State: "move", Until: ptr(rt(b4710D1)), Actor: "user"}, b4710Moved)
	return c.sched(t, c.wsA.ID, c.src.ID)
}

func b4710Resolve(t *testing.T, c *schedCopy, in ResolveInput, now string) {
	t.Helper()
	if _, err := c.s.resolveOccurrenceAt(c.wsA.ID, c.src.ID, in, rt(now)); err != nil {
		t.Fatalf("resolve %s at %s: %v", in.State, now, err)
	}
}

// b4710Outstanding is the destination's outstanding agenda entry at copyNow.
func b4710Outstanding(t *testing.T, c *schedCopy, sc *models.ItemSchedule) AgendaEntry {
	t.Helper()
	es, _, unavailable, err := c.s.ExpandAgenda(sc, time.UTC, rt("2026-10-27T00:00:00Z"), rt("2026-11-03T00:00:00Z"), copyNow)
	if err != nil || unavailable != "" {
		t.Fatalf("ExpandAgenda: unavailable=%q err=%v", unavailable, err)
	}
	var out []AgendaEntry
	for _, e := range es {
		if e.State == "outstanding" {
			out = append(out, e)
		}
	}
	if len(out) != 1 {
		t.Fatalf("outstanding entries = %+v, want 1", out)
	}
	return out[0]
}

// b4710Move previews and commits the cross-workspace move, asserts they report
// the same, and returns the destination schedule and the report.
func b4710Move(t *testing.T, c *schedCopy, move bool) (*models.ItemSchedule, ScheduleCarryReport) {
	t.Helper()
	preview, err := c.s.PreviewScheduleCarry(c.s.db, c.wsA.ID, c.src.ID, "", c.actor, move, copyNow)
	if err != nil {
		t.Fatal(err)
	}
	res := c.do(t, move)
	if preview != res.Schedule {
		t.Errorf("preview %+v != commit %+v", preview, res.Schedule)
	}
	if !res.Schedule.Carried {
		t.Fatalf("report = %+v, want carried", res.Schedule)
	}
	return c.sched(t, c.wsB.ID, res.Item.ID), res.Schedule
}

// The destination reads exactly what a same-workspace move reads
// (TestTASK4590SnoozeThenMoveLandsOnTarget): at and effective_at are the moved
// target, nothing is snoozed, and the occurrence has its moved row.
func b4710AssertMoved(t *testing.T, c *schedCopy, b *models.ItemSchedule, rep ScheduleCarryReport) {
	t.Helper()
	d1 := rt(b4710D1)
	if b.NextOverrideAt == nil || !rt(*b.NextOverrideAt).Equal(d1) {
		t.Fatalf("destination next_override_at = %v, want %s", b.NextOverrideAt, d1)
	}
	if n := c.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id = ? AND state = 'moved'`, b.ID); n != 1 {
		t.Errorf("destination moved rows = %d, want 1", n)
	}
	if n := c.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id = ?`, b.ID); n != 1 {
		t.Errorf("destination history rows = %d, want only the moved one", n)
	}
	e := b4710Outstanding(t, c, b)
	if !e.At.Equal(d1) || e.EffectiveAt == nil || !e.EffectiveAt.Equal(d1) {
		t.Errorf("agenda at = %s effective_at = %v, want the moved target %s", e.At, e.EffectiveAt, d1)
	}
	if e.SnoozedUntil != nil {
		t.Errorf("agenda snoozed_until = %s, want nil (a move, not a snooze)", e.SnoozedUntil)
	}
	if got, err := c.s.ReadScheduleSnoozedUntil(b, copyNow); err != nil || got != nil {
		t.Errorf("ReadScheduleSnoozedUntil = %v %v, want nil", got, err)
	}
	if rep.HistoryDropped != 1 {
		t.Errorf("history_dropped = %d, want 1 (the done row; the moved row travels)", rep.HistoryDropped)
	}
}

func TestBUG4710MoveCarriesMovedOccurrence(t *testing.T) {
	t.Parallel()
	t.Run("pinned, pending ahead", func(t *testing.T) {
		t.Parallel()
		c := newSchedCopy(t)
		b4710Source(t, c, daily(), b4710Created)
		b, rep := b4710Move(t, c, true)
		if b.NextWall == nil || *b.NextWall != "2026-10-30T09:00:00" {
			t.Fatalf("destination next_wall = %v, want the 30th", b.NextWall)
		}
		b4710AssertMoved(t, c, b, rep)
	})
	t.Run("pinned, pending due (catch-up)", func(t *testing.T) {
		t.Parallel()
		c := newSchedCopy(t)
		// Created on the 27th: D0 is the 28th 09:00, past at copyNow.
		b4710Source(t, c, daily(), "2026-10-27T12:00:00Z")
		b, rep := b4710Move(t, c, true)
		if b.NextWall == nil || *b.NextWall != "2026-10-28T09:00:00" {
			t.Fatalf("destination next_wall = %v, want the 28th carried", b.NextWall)
		}
		b4710AssertMoved(t, c, b, rep)
	})
	t.Run("floating", func(t *testing.T) {
		t.Parallel()
		c := newSchedCopy(t)
		b4710Source(t, c, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Floating: true, Rrule: "FREQ=DAILY", LeadMinutes: 30}, b4710Created)
		b, rep := b4710Move(t, c, true)
		if b.NextWall == nil || *b.NextWall != "2026-10-30T09:00:00" {
			t.Fatalf("destination next_wall = %v, want the 30th", b.NextWall)
		}
		b4710AssertMoved(t, c, b, rep)
	})
}

// Moved to D1, then snoozed: next_override_at is the snooze, not the target.
// The destination reads the snooze as the source does.
func TestBUG4710MoveCarriesMovedThenSnoozed(t *testing.T) {
	t.Parallel()
	c := newSchedCopy(t)
	sc := b4710Source(t, c, daily(), b4710Created)
	occ, ok, err := OutstandingInstant(sc, time.UTC)
	if err != nil || !ok {
		t.Fatalf("outstanding instant: %v %v", ok, err)
	}
	snooze := rt("2026-10-31T12:00:00Z")
	b4710Resolve(t, c, ResolveInput{OccurrenceAt: occ, State: "snooze", Until: &snooze, Actor: "user"}, "2026-10-29T11:30:00Z")
	src := c.sched(t, c.wsA.ID, c.src.ID)
	if got, err := c.s.ReadScheduleSnoozedUntil(src, copyNow); err != nil || got == nil || !got.Equal(snooze) {
		t.Fatalf("setup: source snoozed_until = %v %v, want %s", got, err, snooze)
	}
	b, rep := b4710Move(t, c, true)
	if n := c.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id = ? AND state = 'moved'`, b.ID); n != 1 {
		t.Errorf("destination moved rows = %d, want 1", n)
	}
	e := b4710Outstanding(t, c, b)
	d1 := rt(b4710D1)
	if !e.At.Equal(d1) {
		t.Errorf("agenda at = %s, want the moved target %s", e.At, d1)
	}
	if e.SnoozedUntil == nil || !e.SnoozedUntil.Equal(snooze) {
		t.Errorf("agenda snoozed_until = %v, want %s", e.SnoozedUntil, snooze)
	}
	if got, err := c.s.ReadScheduleSnoozedUntil(b, copyNow); err != nil || got == nil || !got.Equal(snooze) {
		t.Errorf("ReadScheduleSnoozedUntil = %v %v, want %s", got, err, snooze)
	}
	if rep.HistoryDropped != 1 {
		t.Errorf("history_dropped = %d, want 1", rep.HistoryDropped)
	}
}

// A COPY of a moved item is unchanged: fresh history, no override, no snooze.
func TestBUG4710CopyOfAMovedItemCarriesNoHistory(t *testing.T) {
	t.Parallel()
	c := newSchedCopy(t)
	b4710Source(t, c, daily(), b4710Created)
	b, rep := b4710Move(t, c, false)
	if n := c.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id = ?`, b.ID); n != 0 {
		t.Errorf("destination history rows = %d, want 0", n)
	}
	if n := c.count(t, `SELECT COUNT(*) FROM schedule_occurrence_overrides WHERE schedule_id = ?`, b.ID); n != 0 {
		t.Errorf("destination overrides = %d, want 0", n)
	}
	if b.NextOverrideAt != nil {
		t.Errorf("destination next_override_at = %s, want nil", *b.NextOverrideAt)
	}
	if rep.HistoryDropped != 2 {
		t.Errorf("history_dropped = %d, want 2 (a copy carries none)", rep.HistoryDropped)
	}
	if a := c.sched(t, c.wsA.ID, c.src.ID); a.EndedAt != nil {
		t.Error("a copy ended the source")
	}
}

// A floating move that changes the recipient zone (a Tokyo assignee who
// is not a member of B, so B reads the copier's UTC) keys the carried row on
// the DESTINATION's outstanding instant (the 30th 09:00 is 00:00Z in Tokyo,
// 09:00Z in UTC), so resolving the occurrence there
// rewrites that row instead of adding a second one for the same wall.
func TestBUG4710FloatingMoveAcrossZonesKeysTheRowInTheDestination(t *testing.T) {
	t.Parallel()
	c := newSchedCopy(t)
	tk := createTestUser(t, c.s, "b4710-tokyo@example.com", "tokyo4710", "password123")
	if _, err := c.s.SetUserTimezoneIfUnset(tk.ID, "Asia/Tokyo"); err != nil {
		t.Fatal(err)
	}
	c.exec(t, `UPDATE items SET assigned_user_id = ? WHERE id = ?`, tk.ID, c.src.ID)
	sc, err := c.s.createScheduleAt(c.wsA.ID, c.src.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Floating: true, Rrule: "FREQ=DAILY", LeadMinutes: 30}, rt(b4710Created))
	if err != nil {
		t.Fatal(err)
	}
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	occ, ok, err := OutstandingInstant(sc, tokyo)
	if err != nil || !ok {
		t.Fatalf("outstanding instant: %v %v", ok, err)
	}
	b4710Resolve(t, c, ResolveInput{OccurrenceAt: occ, State: "move", Until: ptr(rt(b4710D1)), Actor: "user"}, b4710Moved)
	b, _ := b4710Move(t, c, true)
	if b.NextWall == nil || *b.NextWall != "2026-10-30T09:00:00" || b.NextAt == nil {
		t.Fatalf("destination = %+v, want the 30th", b)
	}
	var at string
	if err := c.s.db.QueryRow(c.s.q(`SELECT occurrence_at FROM schedule_occurrences WHERE schedule_id = ? AND state = 'moved'`), b.ID).Scan(&at); err != nil {
		t.Fatal(err)
	}
	if at != *b.NextAt {
		t.Fatalf("moved row occurrence_at = %s, want the destination's next_at %s", at, *b.NextAt)
	}
	cur, ok, err := OutstandingInstant(b, time.UTC)
	if err != nil || !ok {
		t.Fatalf("destination outstanding instant: %v %v", ok, err)
	}
	b4710Item, err := c.s.GetItem(b.ItemID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.s.resolveOccurrenceAt(c.wsB.ID, b4710Item.ID, ResolveInput{OccurrenceAt: cur, State: "done", Actor: "user"}, copyNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n := c.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id = ? AND occurrence_wall = ?`, b.ID, "2026-10-30T09:00:00"); n != 1 {
		t.Errorf("destination rows for the wall = %d, want 1 (the moved row rewritten as done)", n)
	}
}
