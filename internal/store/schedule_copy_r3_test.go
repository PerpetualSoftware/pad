package store

import (
	"errors"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4104 D3 r3: the move catch-up and the exhausted verdict read the SOURCE
// schedule in the source recipient's zone.

// laSource assigns the source item to a Los Angeles user who is not a member of
// B (so a copy or move drops the assignee and B's recipient is the copier, UTC)
// and creates a floating daily 09:00 schedule whose outstanding occurrence is
// the 29th. Its early bound (next_at, UTC+14) is the 28th 19:00Z, already past
// at copyNow; its instant in Los Angeles is 16:00Z, still ahead.
func laSource(t *testing.T, c *schedCopy, rrule string) *models.ItemSchedule {
	t.Helper()
	la := createTestUser(t, c.s, "d3r3-la@example.com", "lar3", "password123")
	if _, err := c.s.SetUserTimezoneIfUnset(la.ID, "America/Los_Angeles"); err != nil {
		t.Fatal(err)
	}
	c.exec(t, `UPDATE items SET assigned_user_id = ? WHERE id = ?`, la.ID, c.src.ID)
	sc := c.schedule(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Floating: true, Rrule: rrule})
	c.exec(t, `UPDATE item_schedules SET next_at = ?, next_wall = ? WHERE id = ?`, "2026-10-28T19:00:00Z", "2026-10-29T09:00:00", sc.ID)
	return sc
}

// r3-1: an ARMED floating occurrence is due by its recipient-zone instant, not
// by next_at (the early bound). Here it is not due, so a move arms B fresh (the
// 30th in UTC) instead of carrying the 29th and firing it at once.
func TestMoveDoesNotCatchUpAnArmedFloatingOccurrenceAheadInTheRecipientZone(t *testing.T) {
	t.Parallel()
	c := newSchedCopy(t)
	laSource(t, c, "FREQ=DAILY")
	res := c.do(t, true)
	if !res.Schedule.Carried {
		t.Fatalf("report = %+v", res.Schedule)
	}
	b := c.sched(t, c.wsB.ID, res.Item.ID)
	if b == nil || b.NextWall == nil || *b.NextWall != "2026-10-30T09:00:00" {
		t.Fatalf("destination next_wall = %v, want the 30th (armed fresh, nothing caught up)", b)
	}
	if got := c.tick(t, "2026-10-29T12:01:00Z", c.wsB.ID); len(got) != 0 {
		t.Fatalf("the move fired the occurrence at once: %+v", got)
	}
}

// r3-2: the rule is spent in the destination's zone (UTC: the last wall, the
// 29th 09:00, is behind) but the source's outstanding occurrence is still ahead
// in its own (Los Angeles 16:00Z). A move would lose it, so it is refused and
// the source stays live; a copy keeps today's "exhausted" skip.
func TestMoveRefusesWhenTheRuleIsSpentOnlyInTheDestinationZone(t *testing.T) {
	t.Parallel()
	c := newSchedCopy(t)
	sc := laSource(t, c, "FREQ=DAILY;UNTIL=20261029T090000")
	before := c.sched(t, c.wsA.ID, c.src.ID)
	itemsB := countItemsIn(t, c.s, c.wsB.ID)

	// The preflight answers the same.
	if _, err := c.s.PreviewScheduleCarry(c.s.db, c.wsA.ID, c.src.ID, "", c.actor, true, copyNow); err == nil {
		t.Error("preview of the move did not refuse")
	} else {
		var nc *ScheduleNotCarriedError
		if !errors.As(err, &nc) || nc.Reason != reasonExhaustedInDestZone {
			t.Errorf("preview err = %v", err)
		}
	}
	if rep, err := c.s.PreviewScheduleCarry(c.s.db, c.wsA.ID, c.src.ID, "", c.actor, false, copyNow); err != nil || rep.Skipped != "exhausted" {
		t.Errorf("copy preview = %+v, %v; want skipped exhausted", rep, err)
	}

	req := c.req()
	req.SourceItemID, req.ArchiveSource, req.scheduleNow = c.src.ID, true, copyNow
	_, err := c.s.CopyItemAcrossWorkspaces(req)
	var refused *ScheduleNotCarriedError
	if !errors.As(err, &refused) || refused.Reason != reasonExhaustedInDestZone {
		t.Fatalf("move err = %v, want ScheduleNotCarriedError %q", err, reasonExhaustedInDestZone)
	}
	if live, _ := c.s.GetItem(c.src.ID); live == nil {
		t.Error("the source was archived despite the refusal")
	}
	if got := countItemsIn(t, c.s, c.wsB.ID); got != itemsB {
		t.Errorf("destination items %d -> %d", itemsB, got)
	}
	a := c.sched(t, c.wsA.ID, c.src.ID)
	if a == nil || a.EndedAt != nil || a.NextWall == nil || a.NextAt == nil || a.UpdatedAt != before.UpdatedAt {
		t.Errorf("source schedule = %+v, want live and untouched (was %+v)", a, before)
	}
	if n, _, _ := c.managed(t, sc.ID); n != 1 {
		t.Errorf("source managed rows = %d, want 1", n)
	}

	res := c.do(t, false)
	if res.Item == nil || res.Schedule.Carried || res.Schedule.Skipped != "exhausted" {
		t.Errorf("copy report = %+v, want skipped exhausted", res.Schedule)
	}
	if a := c.sched(t, c.wsA.ID, c.src.ID); a == nil || a.EndedAt != nil {
		t.Errorf("a plain copy touched the source schedule: %+v", a)
	}
}

// HQ1: a move carries a snooze that is still ahead, whether the snoozed
// occurrence is ahead (fresh path, same occurrence) or due (catch-up path); a
// snooze at or before now is spent and the due occurrence fires at once.
func TestMoveCarriesASnoozeStillAhead(t *testing.T) {
	t.Parallel()
	t.Run("pending occurrence ahead", func(t *testing.T) {
		t.Parallel()
		c := newSchedCopy(t)
		sc := c.scheduleFuture(t, daily()) // the 30th 09:00Z
		c.exec(t, `UPDATE item_schedules SET next_override_at = ? WHERE id = ?`, "2026-10-30T15:00:00Z", sc.ID)
		res := c.do(t, true)
		b := c.sched(t, c.wsB.ID, res.Item.ID)
		if b == nil || b.NextWall == nil || *b.NextWall != "2026-10-30T09:00:00" || b.NextOverrideAt == nil || *b.NextOverrideAt != "2026-10-30T15:00:00Z" {
			t.Fatalf("destination = %+v, want the 30th snoozed to 15:00Z", b)
		}
		if n, at, fired := c.managed(t, b.ID); n != 1 || fired || at != "2026-10-30T14:30:00Z" {
			t.Errorf("managed row = %d %s fired=%v, want armed at 14:30Z", n, at, fired)
		}
	})
	t.Run("due occurrence, snooze ahead", func(t *testing.T) {
		t.Parallel()
		c := newSchedCopy(t)
		sc := c.schedule(t, daily())
		c.forcePast(t, sc)
		c.exec(t, `UPDATE item_schedules SET next_override_at = ? WHERE id = ?`, "2026-10-29T15:00:00Z", sc.ID)
		c.exec(t, `UPDATE item_reminders SET remind_at = ? WHERE schedule_id = ?`, "2026-10-29T14:30:00Z", sc.ID)
		res := c.do(t, true)
		b := c.sched(t, c.wsB.ID, res.Item.ID)
		if b == nil || b.NextWall == nil || *b.NextWall != "2026-10-28T09:00:00" || b.NextOverrideAt == nil {
			t.Fatalf("destination = %+v, want the 28th still snoozed", b)
		}
		if got := c.tick(t, "2026-10-29T12:01:00Z", c.wsB.ID); len(got) != 0 {
			t.Fatalf("the snoozed occurrence fired early: %+v", got)
		}
		if got := c.tick(t, "2026-10-29T15:00:00Z", c.wsB.ID); len(got) != 1 {
			t.Fatalf("after the snooze the destination fired %d, want 1", len(got))
		}
	})
	t.Run("due occurrence, snooze spent", func(t *testing.T) {
		t.Parallel()
		c := newSchedCopy(t)
		sc := c.schedule(t, daily())
		c.forcePast(t, sc)
		c.exec(t, `UPDATE item_schedules SET next_override_at = ? WHERE id = ?`, "2026-10-29T10:00:00Z", sc.ID)
		res := c.do(t, true)
		b := c.sched(t, c.wsB.ID, res.Item.ID)
		if b == nil || b.NextWall == nil || *b.NextWall != "2026-10-28T09:00:00" || b.NextOverrideAt != nil {
			t.Fatalf("destination = %+v, want the 28th with no override", b)
		}
		if got := c.tick(t, "2026-10-29T12:01:00Z", c.wsB.ID); len(got) != 1 {
			t.Fatalf("destination fired %d, want the catch-up once", len(got))
		}
	})
}
