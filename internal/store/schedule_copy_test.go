package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/kernelevents"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// TASK-4104 D3: schedules on a cross-workspace copy and move.

// copyNow is the instant every copy here runs at (req.scheduleNow).
var copyNow = rt("2026-10-29T12:00:00Z")

const copyNowTS = "2026-10-29T12:00:00Z"

// schedCopy is the copy fixture with a source item in A.
type schedCopy struct {
	copyFixture
	src *models.Item
}

func newSchedCopy(t *testing.T) *schedCopy {
	t.Helper()
	f := newCopyFixture(t)
	return &schedCopy{copyFixture: f, src: createTestItem(t, f.s, f.wsA.ID, f.colA.ID, "Pay rent", "")}
}

func (c *schedCopy) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := c.s.db.Exec(c.s.q(q), args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func (c *schedCopy) count(t *testing.T, q string, args ...any) (n int) {
	t.Helper()
	if err := c.s.db.QueryRow(c.s.q(q), args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// daily is a pinned daily 09:00 UTC rule with a 30 minute lead.
func daily() models.ScheduleCreate {
	return models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY", LeadMinutes: 30}
}

// schedule creates the source's schedule and puts its outstanding occurrence at
// the 28th, already past at copyNow.
func (c *schedCopy) schedule(t *testing.T, in models.ScheduleCreate) *models.ItemSchedule {
	t.Helper()
	sc, err := c.s.createScheduleAt(c.wsA.ID, c.src.ID, in, rt("2026-10-27T12:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// scheduleFuture creates the source's schedule as of the copy instant, so its
// outstanding occurrence is still ahead (nothing due to catch up).
func (c *schedCopy) scheduleFuture(t *testing.T, in models.ScheduleCreate) *models.ItemSchedule {
	t.Helper()
	sc, err := c.s.createScheduleAt(c.wsA.ID, c.src.ID, in, copyNow)
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// forcePast moves the outstanding occurrence to the 28th at 09:00 UTC.
func (c *schedCopy) forcePast(t *testing.T, sc *models.ItemSchedule) {
	t.Helper()
	c.exec(t, `UPDATE item_schedules SET next_at = ?, next_wall = ? WHERE id = ?`, "2026-10-28T09:00:00Z", "2026-10-28T09:00:00", sc.ID)
	c.exec(t, `UPDATE item_reminders SET remind_at = ? WHERE schedule_id = ?`, "2026-10-28T08:30:00Z", sc.ID)
}

func (c *schedCopy) history(t *testing.T, sc *models.ItemSchedule, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		c.exec(t, `INSERT INTO schedule_occurrences (id, schedule_id, occurrence_at, occurrence_wall, state, comment_id, actor, resolved_at)
			VALUES (?, ?, ?, ?, 'done', NULL, 'user', ?)`, newID(), sc.ID,
			fmt.Sprintf("2026-10-%02dT09:00:00Z", i), fmt.Sprintf("2026-10-%02dT09:00:00", i), "2026-10-27T12:00:00Z")
	}
}

func (c *schedCopy) do(t *testing.T, archive bool) *CrossWorkspaceCopyResult {
	t.Helper()
	req := c.req()
	req.SourceItemID = c.src.ID
	req.ArchiveSource = archive
	req.scheduleNow = copyNow
	return c.copy(t, req)
}

func (c *schedCopy) sched(t *testing.T, wsID, itemID string) *models.ItemSchedule {
	t.Helper()
	sc, err := c.s.GetSchedule(wsID, itemID)
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// tick fires what is due at ts and returns the fired rows of one workspace.
func (c *schedCopy) tick(t *testing.T, ts, wsID string) []*models.Reminder {
	t.Helper()
	all, err := c.s.FireDueReminders(ts, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []*models.Reminder
	for _, r := range all {
		if r.WorkspaceID == wsID {
			out = append(out, r)
		}
	}
	return out
}

func (c *schedCopy) managed(t *testing.T, scheduleID string) (n int, remindAt string, fired bool) {
	t.Helper()
	n = c.count(t, `SELECT COUNT(*) FROM item_reminders WHERE schedule_id = ?`, scheduleID)
	if n == 1 {
		var f *string
		if err := c.s.db.QueryRow(c.s.q(`SELECT remind_at, fired_at FROM item_reminders WHERE schedule_id = ?`), scheduleID).Scan(&remindAt, &f); err != nil {
			t.Fatal(err)
		}
		fired = f != nil
	}
	return n, remindAt, fired
}

// A copy arms the destination schedule fresh: the timing and labels carry, the
// override and the history do not, and the outstanding occurrence is the first
// one after the copy instant, not the source's stale one.
func TestCopyCarriesScheduleFresh(t *testing.T) {
	t.Parallel()
	c := newSchedCopy(t)
	in := daily()
	in.Audience = "human"
	sc := c.schedule(t, in)
	c.forcePast(t, sc)
	c.history(t, sc, 3)
	c.exec(t, `UPDATE item_schedules SET next_override_at = ? WHERE id = ?`, "2026-10-28T10:00:00Z", sc.ID)

	res := c.do(t, false)
	if !res.Schedule.Carried || res.Schedule.HistoryDropped != 3 || res.Schedule.Skipped != "" {
		t.Fatalf("report = %+v, want carried with 3 history dropped", res.Schedule)
	}
	b := c.sched(t, c.wsB.ID, res.Item.ID)
	if b == nil {
		t.Fatal("no schedule in the destination")
	}
	if b.Kind != "check" || b.Audience != "human" || b.LeadMinutes != 30 || b.Rrule == nil || *b.Rrule != *sc.Rrule || b.Dtstart != sc.Dtstart {
		t.Errorf("fields not carried: %+v", b)
	}
	if b.NextWall == nil || *b.NextWall != "2026-10-30T09:00:00" || b.NextOverrideAt != nil || b.EndedAt != nil {
		t.Errorf("outstanding = wall %v override %v ended %v, want the 30th, no override", b.NextWall, b.NextOverrideAt, b.EndedAt)
	}
	if n := c.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id = ?`, b.ID); n != 0 {
		t.Errorf("destination carries %d history rows", n)
	}
	if n, at, fired := c.managed(t, b.ID); n != 1 || at != "2026-10-30T08:30:00Z" || fired {
		t.Errorf("managed row = %d %s fired=%v, want one armed row at 08:30Z on the 30th", n, at, fired)
	}
	if n := c.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id = ?`, sc.ID); n != 3 {
		t.Errorf("source history = %d, want 3 untouched", n)
	}
}

// A schedule whose rule does not compile is skipped with its reason; the item is
// still copied.
func TestCopySkipsUncompilableSchedule(t *testing.T) {
	t.Parallel()
	c := newSchedCopy(t)
	sc := c.schedule(t, daily())
	c.exec(t, `UPDATE item_schedules SET rrule = 'NOT A RULE' WHERE id = ?`, sc.ID)
	c.history(t, sc, 2)

	res := c.do(t, false)
	if res.Item == nil {
		t.Fatal("item not copied")
	}
	if res.Schedule.Carried || res.Schedule.Skipped == "" || res.Schedule.HistoryDropped != 2 {
		t.Errorf("report = %+v, want skipped with a reason and 2 history", res.Schedule)
	}
	if b := c.sched(t, c.wsB.ID, res.Item.ID); b != nil {
		t.Errorf("a schedule that does not compile was stored: %+v", b)
	}
}

// An ended source schedule is skipped as "ended", and a move still ends nothing
// that is not live.
func TestCopySkipsEndedSchedule(t *testing.T) {
	t.Parallel()
	c := newSchedCopy(t)
	sc := c.schedule(t, daily())
	c.exec(t, `UPDATE item_schedules SET ended_at = ?, next_at = NULL, next_wall = NULL WHERE id = ?`, "2026-10-28T10:00:00Z", sc.ID)
	c.exec(t, `DELETE FROM item_reminders WHERE schedule_id = ?`, sc.ID)

	res := c.do(t, false)
	if res.Schedule.Carried || res.Schedule.Skipped != "ended" {
		t.Errorf("report = %+v, want skipped ended", res.Schedule)
	}
	if b := c.sched(t, c.wsB.ID, res.Item.ID); b != nil {
		t.Errorf("an ended schedule was carried: %+v", b)
	}
}

// A copy never fires the source's overdue occurrence in the destination, even
// when the source has fired it and nobody has resolved it: the source still
// holds it (Q3).
func TestCopyDoesNotCatchUpAFiredSource(t *testing.T) {
	t.Parallel()
	c := newSchedCopy(t)
	sc := c.schedule(t, daily())
	c.forcePast(t, sc)
	if got := c.tick(t, copyNowTS, c.wsA.ID); len(got) != 1 {
		t.Fatalf("source fired %d, want 1", len(got))
	}

	res := c.do(t, false)
	b := c.sched(t, c.wsB.ID, res.Item.ID)
	if b == nil {
		t.Fatal("no schedule in the destination")
	}
	n, at, fired := c.managed(t, b.ID)
	if n != 1 || fired || at <= copyNowTS {
		t.Fatalf("destination managed row = %d %s fired=%v, want armed in the future", n, at, fired)
	}
	if got := c.tick(t, "2026-10-29T12:01:00Z", c.wsB.ID); len(got) != 0 {
		t.Errorf("the destination fired %d on the first tick, want a copy to catch nothing up", len(got))
	}
	if a := c.sched(t, c.wsA.ID, c.src.ID); a.EndedAt != nil || !a.ReminderFired {
		t.Errorf("a plain copy changed the source schedule: %+v", a)
	}
}

// A move whose source has fired the outstanding occurrence unresolved carries
// exactly one catch-up: the destination fires that occurrence once, the source
// is ended and fires nothing.
func TestMoveCatchesUpAFiredUnresolvedOccurrenceOnce(t *testing.T) {
	t.Parallel()
	c := newSchedCopy(t)
	sc := c.schedule(t, daily())
	c.forcePast(t, sc)
	if got := c.tick(t, copyNowTS, c.wsA.ID); len(got) != 1 {
		t.Fatalf("source fired %d, want 1", len(got))
	}

	res := c.do(t, true)
	got := c.tick(t, "2026-10-29T12:01:00Z", c.wsB.ID)
	if len(got) != 1 || got[0].OccurrenceAt == nil || *got[0].OccurrenceAt != "2026-10-28T09:00:00Z" {
		t.Fatalf("destination fired %+v, want the 28th once", got)
	}
	if again := c.tick(t, "2026-10-29T12:02:00Z", c.wsB.ID); len(again) != 0 {
		t.Errorf("the catch-up fired twice")
	}
	if a := c.tick(t, "2026-12-31T00:00:00Z", c.wsA.ID); len(a) != 0 {
		t.Errorf("the source fired after the move: %+v", a)
	}
	if b := c.sched(t, c.wsB.ID, res.Item.ID); b == nil || !b.ReminderFired {
		t.Errorf("destination schedule = %+v", b)
	}
}

// A move with nothing outstanding carries no catch-up: an occurrence that fired
// and was then resolved.
func TestMoveDoesNotCatchUpWhenNothingIsOutstanding(t *testing.T) {
	t.Parallel()
	t.Run("fired then resolved", func(t *testing.T) {
		c := newSchedCopy(t)
		sc := c.schedule(t, daily())
		c.forcePast(t, sc)
		if got := c.tick(t, copyNowTS, c.wsA.ID); len(got) != 1 {
			t.Fatalf("source fired %d, want 1", len(got))
		}
		if _, err := c.s.resolveOccurrenceAt(c.wsA.ID, c.src.ID, ResolveInput{State: "done", OccurrenceAt: rt("2026-10-28T09:00:00Z"), Actor: "user"}, copyNow); err != nil {
			t.Fatal(err)
		}
		res := c.do(t, true)
		if got := c.tick(t, "2026-10-29T12:01:00Z", c.wsB.ID); len(got) != 0 {
			t.Errorf("the destination fired %d after a resolved occurrence", len(got))
		}
		if b := c.sched(t, c.wsB.ID, res.Item.ID); b == nil {
			t.Error("no schedule in the destination")
		}
	})
}

// A move into another collection of the SAME workspace keeps the schedule and
// its managed row untouched.
func TestSameWorkspaceMoveKeepsTheSchedule(t *testing.T) {
	t.Parallel()
	c := newSchedCopy(t)
	sc := c.schedule(t, daily())
	_, _, _ = c.managed(t, sc.ID)
	other := createTestCollection(t, c.s, c.wsA.ID, "Other A")
	if _, err := c.s.MoveItem(c.src.ID, other.ID, `{}`); err != nil {
		t.Fatalf("MoveItem: %v", err)
	}
	after := c.sched(t, c.wsA.ID, c.src.ID)
	if after == nil || after.ID != sc.ID || after.EndedAt != nil || after.NextWall == nil || *after.NextWall != *sc.NextWall {
		t.Fatalf("schedule after a same-workspace move = %+v, want %+v", after, sc)
	}
	if n, _, fired := c.managed(t, sc.ID); n != 1 || fired {
		t.Errorf("managed row = %d fired=%v, want the one armed row", n, fired)
	}
}

// 8c: a cross-workspace move ENDS the source schedule. Restoring the source
// does not bring it back, and only the destination fires.
func TestMoveEndsTheSourceScheduleAndARestoredSourceStaysSilent(t *testing.T) {
	t.Parallel()
	c := newSchedCopy(t)
	sc := c.schedule(t, daily())
	c.history(t, sc, 2)

	res := c.do(t, true)
	a := c.sched(t, c.wsA.ID, c.src.ID)
	if a == nil || a.EndedAt == nil || a.NextAt != nil || a.NextWall != nil || a.NextOverrideAt != nil {
		t.Fatalf("source schedule after the move = %+v, want ended with no outstanding occurrence", a)
	}
	if n, _, _ := c.managed(t, sc.ID); n != 0 {
		t.Errorf("source managed rows = %d, want 0", n)
	}
	if n := c.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id = ?`, sc.ID); n != 2 {
		t.Errorf("source history = %d, want it kept", n)
	}
	if _, err := c.s.RestoreItem(c.src.ID); err != nil {
		t.Fatalf("RestoreItem: %v", err)
	}
	later := "2026-12-31T00:00:00Z"
	all, err := c.s.FireDueReminders(later, 0) // one tick: it fires both workspaces' due rows
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].WorkspaceID != c.wsB.ID || all[0].ItemID != res.Item.ID {
		t.Fatalf("the tick fired %+v, want exactly the destination's moved item", all)
	}
}

// A plain copy leaves the source's schedule exactly as it was.
func TestPlainCopyLeavesTheSourceScheduleAlone(t *testing.T) {
	t.Parallel()
	c := newSchedCopy(t)
	sc := c.schedule(t, daily())
	c.do(t, false)
	a := c.sched(t, c.wsA.ID, c.src.ID)
	if a == nil || a.EndedAt != nil || a.NextWall == nil || *a.NextWall != *sc.NextWall || a.UpdatedAt != sc.UpdatedAt {
		t.Errorf("source schedule changed by a plain copy: %+v -> %+v", sc, a)
	}
	if n, _, _ := c.managed(t, sc.ID); n != 1 {
		t.Errorf("source managed rows = %d, want 1", n)
	}
}

// A failure at the schedule stage leaves nothing: no destination item, the
// source live and its schedule not ended.
func TestCopyIsAtomicAtTheScheduleStage(t *testing.T) {
	t.Parallel()
	c := newSchedCopy(t)
	sc := c.schedule(t, daily())
	itemsB := countItemsIn(t, c.s, c.wsB.ID)

	req := c.req()
	req.SourceItemID = c.src.ID
	req.ArchiveSource = true
	req.scheduleNow = copyNow
	req.failAfterStage = copyStageSchedule
	if _, err := c.s.CopyItemAcrossWorkspaces(req); err == nil {
		t.Fatal("copy succeeded despite an injected failure at the schedule stage")
	}
	if got := countItemsIn(t, c.s, c.wsB.ID); got != itemsB {
		t.Errorf("destination items %d -> %d", itemsB, got)
	}
	if got := c.count(t, `SELECT COUNT(*) FROM item_schedules WHERE workspace_id = ?`, c.wsB.ID); got != 0 {
		t.Errorf("destination schedules = %d after the rollback", got)
	}
	if live, _ := c.s.GetItem(c.src.ID); live == nil {
		t.Error("the source was archived despite the rollback")
	}
	if a := c.sched(t, c.wsA.ID, c.src.ID); a == nil || a.EndedAt != nil {
		t.Errorf("source schedule after the rollback = %+v", a)
	}
	if n, _, _ := c.managed(t, sc.ID); n != 1 {
		t.Errorf("source managed rows = %d after the rollback", n)
	}
}

// A floating schedule is read in the DESTINATION's recipient zone: the copy's
// assignee when a member of B, else the copier.
func TestCopiedFloatingScheduleFollowsTheDestinationRecipient(t *testing.T) {
	t.Parallel()
	c := newSchedCopy(t)
	la := createTestUser(t, c.s, "d3-la@example.com", "la", "password123")
	if _, err := c.s.SetUserTimezoneIfUnset(la.ID, "America/Los_Angeles"); err != nil {
		t.Fatal(err)
	}
	if err := c.s.AddWorkspaceMember(c.wsB.ID, la.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	c.exec(t, `UPDATE items SET assigned_user_id = ? WHERE id = ?`, la.ID, c.src.ID)
	c.schedule(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Floating: true, Rrule: "FREQ=DAILY"})

	res := c.do(t, false)
	if res.Item.AssignedUserID == nil || *res.Item.AssignedUserID != la.ID {
		t.Fatalf("assignee did not carry: %+v", res.Item.AssignedUserID)
	}
	// 12:00Z is 05:00 in Los Angeles: today's 09:00 local (16:00Z) is still ahead.
	b := c.sched(t, c.wsB.ID, res.Item.ID)
	if b == nil || b.NextWall == nil || *b.NextWall != "2026-10-29T09:00:00" {
		t.Fatalf("assigned destination schedule = %+v, want the 29th", b)
	}
	if got := c.tick(t, "2026-10-29T15:59:00Z", c.wsB.ID); len(got) != 0 {
		t.Fatalf("fired before 09:00 in Los Angeles: %+v", got)
	}
	got := c.tick(t, "2026-10-29T16:00:00Z", c.wsB.ID)
	if len(got) != 1 || *got[0].OccurrenceAt != "2026-10-29T16:00:00Z" {
		t.Fatalf("fired %+v, want 16:00Z on the 29th", got)
	}

	// No assignee: the copier's zone (unset is UTC), where 09:00 today has passed.
	c2 := newSchedCopy(t)
	c2.schedule(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Floating: true, Rrule: "FREQ=DAILY"})
	res2 := c2.do(t, false)
	if b := c2.sched(t, c2.wsB.ID, res2.Item.ID); b == nil || b.NextWall == nil || *b.NextWall != "2026-10-30T09:00:00" {
		t.Fatalf("copier-zone destination schedule = %+v, want the 30th", b)
	}
}

// The carry reads the copier's zone from the ACCOUNT that owns the new item
// (ActorUserID, the created_by_user_id the later live reads use), not from the
// audit Actor string (TASK-4104 Z2).
func TestCopiedFloatingScheduleReadsTheCopierAccountZoneNotTheAuditActor(t *testing.T) {
	t.Parallel()
	c := newSchedCopy(t)
	la := createTestUser(t, c.s, "d3z-la@example.com", "laz", "password123")
	if _, err := c.s.SetUserTimezoneIfUnset(la.ID, "America/Los_Angeles"); err != nil {
		t.Fatal(err)
	}
	c.schedule(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Floating: true, Rrule: "FREQ=DAILY"})

	req := c.req()
	req.SourceItemID, req.scheduleNow = c.src.ID, copyNow
	req.Actor = "agent-not-an-account" // the audit string: no account, so UTC
	req.ActorUserID = la.ID
	res := c.copy(t, req)

	// 12:00Z is 05:00 in Los Angeles: today's 09:00 local is still ahead. In UTC
	// it would have passed, and B would arm the 30th.
	b := c.sched(t, c.wsB.ID, res.Item.ID)
	if b == nil || b.NextWall == nil || *b.NextWall != "2026-10-29T09:00:00" {
		t.Fatalf("destination schedule = %+v, want the 29th (the account's zone)", b)
	}
}

// The preflight asks the same store function the copy runs, so both agree on
// every outcome.
func TestPreviewScheduleCarryMatchesTheCopy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(t *testing.T, c *schedCopy)
	}{
		{"carry", func(t *testing.T, c *schedCopy) { c.history(t, c.schedule(t, daily()), 2) }},
		{"skip uncompilable", func(t *testing.T, c *schedCopy) {
			sc := c.schedule(t, daily())
			c.exec(t, `UPDATE item_schedules SET rrule = 'NOT A RULE' WHERE id = ?`, sc.ID)
		}},
		{"none", func(t *testing.T, c *schedCopy) {}},
		{"ended", func(t *testing.T, c *schedCopy) {
			sc := c.schedule(t, daily())
			c.exec(t, `UPDATE item_schedules SET ended_at = ?, next_at = NULL, next_wall = NULL WHERE id = ?`, "2026-10-28T10:00:00Z", sc.ID)
		}},
		{"exhausted", func(t *testing.T, c *schedCopy) { c.exhaust(t, c.schedule(t, daily())) }},
	}
	for _, tc := range cases {
		for _, move := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s move=%v", tc.name, move), func(t *testing.T) {
				c := newSchedCopy(t)
				tc.setup(t, c)
				got, perr := c.s.PreviewScheduleCarry(c.s.db, c.wsA.ID, c.src.ID, "", c.actor, move, copyNow)
				req := c.req()
				req.SourceItemID, req.ArchiveSource, req.scheduleNow = c.src.ID, move, copyNow
				res, cerr := c.s.CopyItemAcrossWorkspaces(req)
				var pn, cn *ScheduleNotCarriedError
				if errors.As(perr, &pn) != errors.As(cerr, &cn) || (pn != nil && *pn != *cn) {
					t.Fatalf("preview err %v != copy err %v", perr, cerr)
				}
				if cerr != nil {
					return
				}
				if perr != nil || res.Schedule != got {
					t.Errorf("preview %+v (%v) != copy %+v", got, perr, res.Schedule)
				}
			})
		}
	}
	t.Run("carry reports", func(t *testing.T) {
		c := newSchedCopy(t)
		c.history(t, c.schedule(t, daily()), 2)
		got, _ := c.s.PreviewScheduleCarry(c.s.db, c.wsA.ID, c.src.ID, "", c.actor, false, copyNow)
		if !got.HasSchedule || !got.Carried || got.HistoryDropped != 2 {
			t.Errorf("preview = %+v", got)
		}
	})
}

// exhaust makes the rule end before copyNow while the schedule still looks live.
func (c *schedCopy) exhaust(t *testing.T, sc *models.ItemSchedule) {
	t.Helper()
	c.exec(t, `UPDATE item_schedules SET rrule = 'FREQ=DAILY;UNTIL=20261015T000000Z' WHERE id = ?`, sc.ID)
}

// D3-f, floating: the preview reads the schedule in the zone the COPY will, the
// DESTINATION recipient's: the carried assignee, else the copier. The source is
// assigned to a Los Angeles user who is not a member of B, so the copy drops the
// assignee and reads the copier's zone (UTC). The rule's last occurrence is 09:00
// on the 29th: ahead in Los Angeles (05:00 at copyNow), behind in UTC (12:00).
// A preview that read the SOURCE recipient (Los Angeles) would say carried.
func TestPreviewReadsAFloatingScheduleInTheDestinationZone(t *testing.T) {
	t.Parallel()
	c := newSchedCopy(t)
	la := createTestUser(t, c.s, "d3fix-la@example.com", "lafix", "password123")
	if _, err := c.s.SetUserTimezoneIfUnset(la.ID, "America/Los_Angeles"); err != nil {
		t.Fatal(err)
	}
	c.exec(t, `UPDATE items SET assigned_user_id = ? WHERE id = ?`, la.ID, c.src.ID)
	c.schedule(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Floating: true, Rrule: "FREQ=DAILY;UNTIL=20261029T090000"})

	got, err := c.s.PreviewScheduleCarry(c.s.db, c.wsA.ID, c.src.ID, "", c.actor, false, copyNow)
	if err != nil {
		t.Fatal(err)
	}
	res := c.do(t, false)
	if res.DroppedAssignee != true {
		t.Fatal("setup: the assignee should have been dropped")
	}
	if res.Schedule != got {
		t.Fatalf("preview %+v != copy %+v", got, res.Schedule)
	}
	if res.Schedule.Carried || res.Schedule.Skipped != "exhausted" {
		t.Fatalf("report = %+v, want exhausted in the copier's UTC zone", res.Schedule)
	}
}

// D3 r1-3: every reason a live schedule can fail to carry, and what each door
// does with it. A MOVE of a non-terminal one is refused and rolls back whole; a
// copy skips and reports. Terminal ones (ended, exhausted) are covered below.
func TestMoveRefusesALiveScheduleItCannotCarry(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, want string
		corrupt    func(t *testing.T, c *schedCopy, sc *models.ItemSchedule)
		fired      bool // a move carries a fired occurrence as it is; no compile
	}{
		{"rule does not compile", "timing:syntax", func(t *testing.T, c *schedCopy, sc *models.ItemSchedule) {
			c.exec(t, `UPDATE item_schedules SET rrule = 'NOT A RULE' WHERE id = ?`, sc.ID)
		}, false},
		{"zone", "timing:zone", func(t *testing.T, c *schedCopy, sc *models.ItemSchedule) {
			c.exec(t, `UPDATE item_schedules SET tz = 'Not/AZone' WHERE id = ?`, sc.ID)
		}, false},
		{"anchor_wall mismatches the rule", "anchor_wall:combination", func(t *testing.T, c *schedCopy, sc *models.ItemSchedule) {
			c.exec(t, `UPDATE item_schedules SET anchor_wall = '2026-10-28' WHERE id = ?`, sc.ID)
		}, false},
		{"dtstart is not a wall", "timing:wall", func(t *testing.T, c *schedCopy, sc *models.ItemSchedule) {
			c.exec(t, `UPDATE item_schedules SET dtstart = 'garbage' WHERE id = ?`, sc.ID)
		}, false},
		{"window ends before it starts", "dtend:range", func(t *testing.T, c *schedCopy, sc *models.ItemSchedule) {
			c.exec(t, `UPDATE item_schedules SET kind = 'window', dtend = '2026-09-01T09:00' WHERE id = ?`, sc.ID)
		}, false},
		{"fired occurrence is all-day on a timed rule", "next_wall:combination", func(t *testing.T, c *schedCopy, sc *models.ItemSchedule) {
			c.forcePast(t, sc)
			c.exec(t, `UPDATE item_reminders SET fired_at = ? WHERE schedule_id = ?`, "2026-10-28T09:00:00Z", sc.ID)
			c.exec(t, `UPDATE item_schedules SET next_wall = '2026-10-28' WHERE id = ?`, sc.ID)
		}, true},
		{"fired occurrence wall is malformed", "next_wall:wall", func(t *testing.T, c *schedCopy, sc *models.ItemSchedule) {
			c.forcePast(t, sc)
			c.exec(t, `UPDATE item_reminders SET fired_at = ? WHERE schedule_id = ?`, "2026-10-28T09:00:00Z", sc.ID)
			c.exec(t, `UPDATE item_schedules SET next_wall = 'garbage' WHERE id = ?`, sc.ID)
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newSchedCopy(t)
			sc := c.schedule(t, daily())
			tc.corrupt(t, c, sc)
			before := c.sched(t, c.wsA.ID, c.src.ID)
			itemsB := countItemsIn(t, c.s, c.wsB.ID)

			req := c.req()
			req.SourceItemID, req.ArchiveSource, req.scheduleNow = c.src.ID, true, copyNow
			_, err := c.s.CopyItemAcrossWorkspaces(req)
			var refused *ScheduleNotCarriedError
			if !errors.As(err, &refused) || refused.Reason != tc.want {
				t.Fatalf("move err = %v, want ScheduleNotCarriedError %q", err, tc.want)
			}
			if got := countItemsIn(t, c.s, c.wsB.ID); got != itemsB {
				t.Errorf("destination items %d -> %d", itemsB, got)
			}
			if live, _ := c.s.GetItem(c.src.ID); live == nil {
				t.Error("the source was archived despite the refusal")
			}
			a := c.sched(t, c.wsA.ID, c.src.ID)
			if a == nil || a.EndedAt != nil || a.UpdatedAt != before.UpdatedAt || a.NextWall == nil {
				t.Errorf("source schedule after the refused move = %+v, want live and untouched (was %+v)", a, before)
			}
			if n, _, _ := c.managed(t, sc.ID); n != 1 {
				t.Errorf("source managed rows = %d, want 1", n)
			}
			if n := c.count(t, `SELECT COUNT(*) FROM item_schedules WHERE workspace_id = ?`, c.wsB.ID); n != 0 {
				t.Errorf("destination schedules = %d", n)
			}

			if tc.fired {
				return // a plain copy never reads the fired wall
			}
			res := c.do(t, false)
			if res.Item == nil || res.Schedule.Carried || res.Schedule.Skipped != tc.want {
				t.Errorf("copy report = %+v, want skipped %q", res.Schedule, tc.want)
			}
			if a := c.sched(t, c.wsA.ID, c.src.ID); a == nil || a.EndedAt != nil {
				t.Errorf("a plain copy touched the source schedule: %+v", a)
			}
		})
	}
}

// D3 r1-3, terminal reasons: nothing is left to fire, so a move ends the source
// (when it is not already) and reports it; a copy skips and reports.
// Since D3 r2-2 the outstanding occurrence is still AHEAD here: a due one is
// carried by a move, so "exhausted" needs a rule with nothing after it.
func TestMoveOfATerminalScheduleEndsItAndReports(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{"exhausted", "ended"} {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			c := newSchedCopy(t)
			sc := c.scheduleFuture(t, daily())
			var endedAt string
			if reason == "exhausted" {
				c.exhaust(t, sc)
			} else {
				endedAt = "2026-10-28T10:00:00Z"
				c.exec(t, `UPDATE item_schedules SET ended_at = ?, next_at = NULL, next_wall = NULL WHERE id = ?`, endedAt, sc.ID)
				c.exec(t, `DELETE FROM item_reminders WHERE schedule_id = ?`, sc.ID)
			}
			cp := c.do(t, false)
			if cp.Schedule.Carried || cp.Schedule.Skipped != reason {
				t.Errorf("copy report = %+v, want skipped %q", cp.Schedule, reason)
			}
			if a := c.sched(t, c.wsA.ID, c.src.ID); a == nil || (reason == "exhausted") == (a.EndedAt != nil) {
				t.Errorf("a plain copy changed whether the source is ended: %+v", a)
			}

			c2 := newSchedCopy(t)
			sc2 := c2.scheduleFuture(t, daily())
			if reason == "exhausted" {
				c2.exhaust(t, sc2)
			} else {
				c2.exec(t, `UPDATE item_schedules SET ended_at = ?, next_at = NULL, next_wall = NULL WHERE id = ?`, endedAt, sc2.ID)
				c2.exec(t, `DELETE FROM item_reminders WHERE schedule_id = ?`, sc2.ID)
			}
			res := c2.do(t, true)
			if res.Schedule.Carried || res.Schedule.Skipped != reason || !res.Schedule.HasSchedule {
				t.Errorf("move report = %+v, want skipped %q", res.Schedule, reason)
			}
			a := c2.sched(t, c2.wsA.ID, c2.src.ID)
			if a == nil || a.EndedAt == nil || a.NextWall != nil {
				t.Errorf("source schedule after the move = %+v, want ended", a)
			}
			if reason == "ended" && *a.EndedAt != endedAt {
				t.Errorf("an already ended schedule was re-ended: %s", *a.EndedAt)
			}
			if n, _, _ := c2.managed(t, sc2.ID); n != 0 {
				t.Errorf("source managed rows = %d", n)
			}
			if b := c2.sched(t, c2.wsB.ID, res.Item.ID); b != nil {
				t.Errorf("destination got a schedule: %+v", b)
			}
		})
	}
}

// D3 r1-3, cap_time: load-dependent, so BOTH a copy and a move are refused with
// the cap error (the HTTP edge answers 503 schedule_busy) and nothing is written.
// SEAM: carryLimits is lowered so the real clock cap bites. Serial on purpose
// (it sets a package variable): parallel tests wait until it finishes.
func TestCapTimeRefusesTheCopyAndTheMoveWithNothingWritten(t *testing.T) {
	old := carryLimits
	carryLimits.MaxDuration = time.Nanosecond
	defer func() { carryLimits = old }()
	for _, move := range []bool{false, true} {
		t.Run(fmt.Sprintf("move=%v", move), func(t *testing.T) {
			c := newSchedCopy(t)
			sc := c.scheduleFuture(t, daily())
			before := c.sched(t, c.wsA.ID, c.src.ID)
			itemsB := countItemsIn(t, c.s, c.wsB.ID)

			req := c.req()
			req.SourceItemID, req.ArchiveSource, req.scheduleNow = c.src.ID, move, copyNow
			_, err := c.s.CopyItemAcrossWorkspaces(req)
			var capErr *schedule.CapError
			if !errors.As(err, &capErr) || capErr.Kind != schedule.CapTime {
				t.Fatalf("err = %v, want a CapTime error", err)
			}
			if got := countItemsIn(t, c.s, c.wsB.ID); got != itemsB {
				t.Errorf("destination items %d -> %d", itemsB, got)
			}
			if live, _ := c.s.GetItem(c.src.ID); live == nil {
				t.Error("the source was archived")
			}
			if a := c.sched(t, c.wsA.ID, c.src.ID); a == nil || a.EndedAt != nil || a.UpdatedAt != before.UpdatedAt {
				t.Errorf("source schedule changed: %+v", a)
			}
			if n, _, _ := c.managed(t, sc.ID); n != 1 {
				t.Errorf("source managed rows = %d", n)
			}
		})
	}
}

// C3: an update of the schedule of an archived item is refused with the item
// error, so it cannot revive a schedule a move ended.
func TestUpdateScheduleOfAnArchivedItemIsRefused(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})
	if err := f.s.DeleteItem(f.item.ID); err != nil {
		t.Fatal(err)
	}
	d := "2027-01-01T09:00"
	_, err := f.s.updateScheduleAt(f.ws.ID, f.item.ID, models.ScheduleUpdate{Dtstart: &d}, schedNow)
	if !errors.Is(err, ErrScheduleItemGone) {
		t.Fatalf("err = %v, want ErrScheduleItemGone", err)
	}
}

// PG: the move holds the item and the schedule through its commit. A timing edit
// parked on pinScheduleTx's row locks must find the item archived when it gets
// them, and leave the ended schedule ended.
func TestPGUpdateParkedOnTheScheduleWhileAMoveCommitsIsRefused(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set: row locks only exist on Postgres")
	}
	t.Parallel()
	c := newSchedCopy(t)
	sc := c.schedule(t, daily())

	release, moved := c.holdMove(t)

	patched := make(chan error, 1)
	go func() {
		d := "2027-01-01T09:00"
		_, err := c.s.updateScheduleAt(c.wsA.ID, c.src.ID, models.ScheduleUpdate{Dtstart: &d}, copyNow)
		patched <- err
	}()
	waitForLockWait(t, c.s, "FOR NO KEY UPDATE OF", patched)
	release()
	if err := <-moved; err != nil {
		t.Fatalf("move: %v", err)
	}
	if err := <-patched; !errors.Is(err, ErrScheduleItemGone) {
		t.Fatalf("parked update err = %v, want ErrScheduleItemGone", err)
	}
	a := c.sched(t, c.wsA.ID, c.src.ID)
	if a == nil || a.EndedAt == nil {
		t.Fatalf("source schedule revived: %+v", a)
	}
	if n, _, _ := c.managed(t, sc.ID); n != 0 {
		t.Errorf("source managed rows = %d, want 0", n)
	}
}

// holdMove starts a move of c.src that pauses just before its commit, after the
// schedule step, and returns a release func and the move's result channel.
func (c *schedCopy) holdMove(t *testing.T) (release func(), done <-chan error) {
	t.Helper()
	atHook, rel := make(chan struct{}), make(chan struct{})
	moved := make(chan error, 1)
	go func() {
		req := c.req()
		req.SourceItemID = c.src.ID
		req.ArchiveSource = true
		req.scheduleNow = copyNow
		req.AfterWriteTx = func(tx *sql.Tx, _ *CrossWorkspaceCopyResult) error {
			close(atHook)
			<-rel
			return nil
		}
		_, err := c.s.CopyItemAcrossWorkspaces(req)
		moved <- err
	}()
	select {
	case <-atHook:
	case err := <-moved:
		t.Fatalf("the move finished before the hook: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the move never reached its commit")
	}
	return func() { close(rel) }, moved
}

// PG: a fire and a move of the same item serialise on the item row, and neither
// order double-fires or loses the fire.
func TestPGFireVersusMove(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set: row locks only exist on Postgres")
	}
	t.Run("move archives first: the fire emits nothing", func(t *testing.T) {
		t.Parallel()
		c := newSchedCopy(t)
		sc := c.schedule(t, daily())
		c.forcePast(t, sc)
		rid := *c.sched(t, c.wsA.ID, c.src.ID).ReminderID
		release, moved := c.holdMove(t)

		var fired *models.Reminder
		fireDone := make(chan error, 1)
		go func() {
			var err error
			fired, err = c.s.fireOneReminder(rid, copyNowTS)
			fireDone <- err
		}()
		waitForLockWait(t, c.s, "FOR NO KEY UPDATE OF", fireDone)
		release()
		if err := <-moved; err != nil {
			t.Fatalf("move: %v", err)
		}
		if err := <-fireDone; err != nil {
			t.Fatalf("fire: %v", err)
		}
		if fired != nil {
			t.Errorf("the fire of an archived item emitted %+v", fired)
		}
		if n := c.count(t, `SELECT COUNT(*) FROM event_outbox WHERE event_type = ?`, kernelevents.ItemReminderDue); n != 0 {
			t.Errorf("%d reminder events, want none from the source", n)
		}
		if a := c.sched(t, c.wsA.ID, c.src.ID); a == nil || a.EndedAt == nil {
			t.Errorf("source schedule = %+v, want ended", a)
		}
	})
	t.Run("fire pins first: the move waits, then carries the fired occurrence once", func(t *testing.T) {
		t.Parallel()
		c := newSchedCopy(t)
		sc := c.schedule(t, daily())
		c.forcePast(t, sc)
		rid := *c.sched(t, c.wsA.ID, c.src.ID).ReminderID

		tx, err := c.s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if ok, err := c.s.pinManagedReminderTx(tx, rid); err != nil || !ok {
			t.Fatalf("pin: %v %v", ok, err)
		}
		if _, err := tx.Exec(c.s.q(`UPDATE item_reminders SET fired_at = ? WHERE id = ?`), copyNowTS, rid); err != nil {
			t.Fatal(err)
		}
		req := c.req()
		req.SourceItemID = c.src.ID
		req.ArchiveSource = true
		req.scheduleNow = copyNow
		moved := make(chan error, 1)
		var res *CrossWorkspaceCopyResult
		go func() {
			var err error
			res, err = c.s.CopyItemAcrossWorkspaces(req)
			moved <- err
		}()
		waitForLockWait(t, c.s, "UPDATE items SET deleted_at", moved)
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if err := <-moved; err != nil {
			t.Fatalf("move: %v", err)
		}
		if res.Schedule.Skipped != "" || !res.Schedule.Carried {
			t.Fatalf("report = %+v", res.Schedule)
		}
		if got := c.tick(t, "2026-10-29T12:01:00Z", c.wsB.ID); len(got) != 1 || *got[0].OccurrenceAt != "2026-10-28T09:00:00Z" {
			t.Fatalf("destination fired %+v, want the fired occurrence once", got)
		}
		if got := c.tick(t, "2026-12-31T00:00:00Z", c.wsA.ID); len(got) != 0 {
			t.Errorf("the source fired after the move: %+v", got)
		}
	})
}

// PG (r1-2, the honest red): a FLOATING schedule whose recipient zone the move
// changes. The source assignee (Tokyo) is not a member of B, so carryAssigneeTx
// drops it and B's recipient is the copier (Los Angeles). A timing edit re-arms
// the fired source to its next Tokyo occurrence (the 30th) while the move is
// parked on the schedule row. Reading the re-armed fired_at, the move arms B
// fresh in Los Angeles: 12:00Z is 05:00 there, so the 29th's 09:00 is still
// ahead. A stale fired_at would take the catch-up branch and carry Tokyo's 30th.
func TestPGMoveParkedBehindAReArmingPatchFollowsTheNewRecipientZone(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set: row locks only exist on Postgres")
	}
	t.Parallel()
	c := newSchedCopy(t)
	tokyo := createTestUser(t, c.s, "d3-r12-tokyo@example.com", "tokyo", "password123")
	la := createTestUser(t, c.s, "d3-r12-la@example.com", "la", "password123")
	for _, u := range []struct{ id, tz, ws string }{{tokyo.ID, "Asia/Tokyo", c.wsA.ID}, {la.ID, "America/Los_Angeles", c.wsB.ID}} {
		if _, err := c.s.SetUserTimezoneIfUnset(u.id, u.tz); err != nil {
			t.Fatal(err)
		}
		if err := c.s.AddWorkspaceMember(u.ws, u.id, "editor"); err != nil {
			t.Fatal(err)
		}
	}
	c.exec(t, `UPDATE items SET assigned_user_id = ? WHERE id = ?`, tokyo.ID, c.src.ID)
	sc := c.schedule(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Floating: true, Rrule: "FREQ=DAILY"})
	// Fired for the 29th in Tokyo (09:00 JST = 00:00Z).
	c.exec(t, `UPDATE item_schedules SET next_at = ?, next_wall = ? WHERE id = ?`, "2026-10-29T00:00:00Z", "2026-10-29T09:00:00", sc.ID)
	c.exec(t, `UPDATE item_reminders SET fired_at = ?, remind_at = ? WHERE schedule_id = ?`, "2026-10-29T00:00:00Z", "2026-10-28T23:30:00Z", sc.ID)

	tx, err := c.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(c.s.q(`SELECT 1 FROM item_schedules WHERE id = ? FOR UPDATE`), sc.ID); err != nil {
		t.Fatal(err)
	}
	req := c.req()
	req.Actor, req.ActorUserID = la.ID, la.ID // a signed-in copier: both name the account
	req.SourceItemID, req.ArchiveSource, req.scheduleNow = c.src.ID, true, copyNow
	moved := make(chan error, 1)
	var res *CrossWorkspaceCopyResult
	go func() {
		var err error
		res, err = c.s.CopyItemAcrossWorkspaces(req)
		moved <- err
	}()
	waitForLockWait(t, c.s, "FOR UPDATE", moved)
	// Re-armed to Tokyo's next occurrence, the 30th (00:00Z), which is after now.
	if _, err := tx.Exec(c.s.q(`UPDATE item_reminders SET fired_at = NULL, remind_at = ? WHERE schedule_id = ?`), "2026-10-29T23:30:00Z", sc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(c.s.q(`UPDATE item_schedules SET next_at = ?, next_wall = ? WHERE id = ?`), "2026-10-30T00:00:00Z", "2026-10-30T09:00:00", sc.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-moved; err != nil {
		t.Fatalf("move: %v", err)
	}
	if !res.Schedule.Carried || res.Item.AssignedUserID != nil {
		t.Fatalf("report = %+v, assignee = %v; want carried and the Tokyo assignee dropped", res.Schedule, res.Item.AssignedUserID)
	}
	if b := c.sched(t, c.wsB.ID, res.Item.ID); b == nil || b.NextWall == nil || *b.NextWall != "2026-10-29T09:00:00" {
		t.Fatalf("destination next_wall = %v next_at = %v, want the 29th (09:00 in Los Angeles is still ahead)", derefStr(b.NextWall), derefStr(b.NextAt))
	}
}

// PG (r1-2): a timing edit that re-arms the managed reminder to a FUTURE
// occurrence commits while a move is parked on the schedule row. The move must
// read the re-armed reminder, not the pre-wait snapshot's fired_at, so B is armed
// at that future occurrence and fires nothing now. (Re-armed to a DUE instant,
// the amended Q3 would carry and catch it up; that is the due-at-move rows' case.)
func TestPGMoveParkedBehindAPatchReArmingToTheFutureDoesNotCatchUp(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set: row locks only exist on Postgres")
	}
	t.Parallel()
	c := newSchedCopy(t)
	sc := c.schedule(t, daily())
	c.forcePast(t, sc)
	c.exec(t, `UPDATE item_reminders SET fired_at = ? WHERE schedule_id = ?`, "2026-10-28T09:00:00Z", sc.ID)

	// The PATCH holds the schedule row, and re-arms (clears fired_at) before it commits.
	tx, err := c.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(c.s.q(`SELECT 1 FROM item_schedules WHERE id = ? FOR UPDATE`), sc.ID); err != nil {
		t.Fatal(err)
	}
	req := c.req()
	req.SourceItemID, req.ArchiveSource, req.scheduleNow = c.src.ID, true, copyNow
	moved := make(chan error, 1)
	var res *CrossWorkspaceCopyResult
	go func() {
		var err error
		res, err = c.s.CopyItemAcrossWorkspaces(req)
		moved <- err
	}()
	waitForLockWait(t, c.s, "FOR UPDATE", moved)
	if _, err := tx.Exec(c.s.q(`UPDATE item_reminders SET fired_at = NULL, remind_at = ? WHERE schedule_id = ?`), "2026-10-30T08:30:00Z", sc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(c.s.q(`UPDATE item_schedules SET next_at = ?, next_wall = ? WHERE id = ?`), "2026-10-30T09:00:00Z", "2026-10-30T09:00:00", sc.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-moved; err != nil {
		t.Fatalf("move: %v", err)
	}
	if !res.Schedule.Carried {
		t.Fatalf("report = %+v", res.Schedule)
	}
	// Armed at the re-armed future occurrence (the 30th, after copyNow), not caught
	// up as fired from the pre-wait snapshot.
	if b := c.sched(t, c.wsB.ID, res.Item.ID); b == nil || b.NextWall == nil || *b.NextWall != "2026-10-30T09:00:00" {
		t.Fatalf("destination schedule = %+v, want the 30th", b)
	}
	if got := c.tick(t, "2026-10-29T12:01:00Z", c.wsB.ID); len(got) != 0 {
		t.Fatalf("the move caught up a re-armed occurrence in B: %+v", got)
	}
}

// oneShot is a single occurrence on the 28th 09:00 UTC (30 minute lead).
func oneShot() models.ScheduleCreate {
	return models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-28T09:00", Tz: "UTC", LeadMinutes: 30}
}

// D3 r2-2: an occurrence that is DUE but has not fired (the tick lags) is still
// outstanding. A move carries it: the destination fires it once, late, and the
// source is ended and fires nothing. A one-shot is not "exhausted" and a
// recurring schedule does not skip to the next day.
func TestMoveCarriesADueButUnfiredOccurrence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   models.ScheduleCreate
	}{{"one-shot", oneShot()}, {"recurring", daily()}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newSchedCopy(t)
			sc := c.schedule(t, tc.in)
			c.forcePast(t, sc)
			if n, _, fired := c.managed(t, sc.ID); n != 1 || fired {
				t.Fatalf("source managed row = %d fired=%v, want armed unfired", n, fired)
			}

			res := c.do(t, true)
			if !res.Schedule.Carried || res.Schedule.Skipped != "" {
				t.Fatalf("move report = %+v, want carried", res.Schedule)
			}
			b := c.sched(t, c.wsB.ID, res.Item.ID)
			if b == nil || b.NextWall == nil || *b.NextWall != "2026-10-28T09:00:00" {
				t.Fatalf("destination schedule = %+v, want the due 28th outstanding", b)
			}
			got := c.tick(t, "2026-10-29T12:01:00Z", c.wsB.ID)
			if len(got) != 1 || got[0].OccurrenceAt == nil || *got[0].OccurrenceAt != "2026-10-28T09:00:00Z" {
				t.Fatalf("destination fired %+v, want the 28th once", got)
			}
			if again := c.tick(t, "2026-10-29T12:02:00Z", c.wsB.ID); len(again) != 0 {
				t.Errorf("the carried occurrence fired twice: %+v", again)
			}
			if a := c.tick(t, "2026-12-31T00:00:00Z", c.wsA.ID); len(a) != 0 {
				t.Errorf("the source fired after the move: %+v", a)
			}
			a := c.sched(t, c.wsA.ID, c.src.ID)
			if a == nil || a.EndedAt == nil || a.NextWall != nil {
				t.Errorf("source schedule = %+v, want ended", a)
			}
			if n, _, _ := c.managed(t, sc.ID); n != 0 {
				t.Errorf("source managed rows = %d", n)
			}

			// Resolved in the destination, a recurring schedule continues on its
			// grid and a one-shot is done.
			if _, err := c.s.resolveOccurrenceAt(c.wsB.ID, res.Item.ID, ResolveInput{State: "done", OccurrenceAt: rt("2026-10-28T09:00:00Z"), Actor: "user"}, copyNow); err != nil {
				t.Fatal(err)
			}
			b = c.sched(t, c.wsB.ID, res.Item.ID)
			if tc.name == "recurring" {
				if b == nil || b.NextWall == nil || *b.NextWall != "2026-10-30T09:00:00" {
					t.Errorf("recurring destination after resolve: next_wall = %v, want the 30th (the grid after now)", derefStr(b.NextWall))
				}
			} else if b != nil && b.NextWall != nil {
				t.Errorf("one-shot destination after resolve = %+v, want nothing outstanding", b)
			}
		})
	}
}

// D3 r2-2, the other door: a plain COPY of a due-but-unfired schedule catches
// nothing up. The recurring destination arms the next occurrence after now; a
// one-shot has none, so it is skipped as exhausted. The source is untouched.
func TestCopyDoesNotCatchUpADueButUnfiredOccurrence(t *testing.T) {
	t.Parallel()
	t.Run("recurring", func(t *testing.T) {
		t.Parallel()
		c := newSchedCopy(t)
		sc := c.schedule(t, daily())
		c.forcePast(t, sc)
		res := c.do(t, false)
		b := c.sched(t, c.wsB.ID, res.Item.ID)
		if b == nil {
			t.Fatal("no schedule in the destination")
		}
		if n, at, fired := c.managed(t, b.ID); n != 1 || fired || at <= copyNowTS {
			t.Errorf("managed row = %d %s fired=%v, want armed in the future", n, at, fired)
		}
		if got := c.tick(t, "2026-10-29T12:01:00Z", c.wsB.ID); len(got) != 0 {
			t.Errorf("the destination fired %d, want no catch-up on a copy", len(got))
		}
	})
	t.Run("one-shot", func(t *testing.T) {
		t.Parallel()
		c := newSchedCopy(t)
		sc := c.schedule(t, oneShot())
		c.forcePast(t, sc)
		res := c.do(t, false)
		if res.Schedule.Carried || res.Schedule.Skipped != "exhausted" {
			t.Errorf("copy report = %+v, want skipped exhausted", res.Schedule)
		}
		if a := c.sched(t, c.wsA.ID, c.src.ID); a == nil || a.EndedAt != nil || a.NextWall == nil {
			t.Errorf("a plain copy changed the source schedule: %+v", a)
		}
	})
}

// D3 r2-2, PG order: a fire tick racing a move for a due-but-unfired occurrence.
// Move first: the source fires nothing and the destination fires it once. Fire
// first: the source fires it once, then the move carries it as the fired,
// unresolved catch-up (Q3) and the destination fires it once; the source never
// fires again.
func TestPGDueUnfiredFireVersusMoveFiresOncePerDoor(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set: row locks only exist on Postgres")
	}
	t.Run("move first", func(t *testing.T) {
		t.Parallel()
		c := newSchedCopy(t)
		sc := c.schedule(t, daily())
		c.forcePast(t, sc)
		rid := *c.sched(t, c.wsA.ID, c.src.ID).ReminderID
		release, moved := c.holdMove(t)
		fireDone := make(chan error, 1)
		var fired *models.Reminder
		go func() {
			var err error
			fired, err = c.s.fireOneReminder(rid, copyNowTS)
			fireDone <- err
		}()
		waitForLockWait(t, c.s, "FOR NO KEY UPDATE OF", fireDone)
		release()
		if err := <-moved; err != nil {
			t.Fatalf("move: %v", err)
		}
		if err := <-fireDone; err != nil {
			t.Fatalf("fire: %v", err)
		}
		if fired != nil {
			t.Errorf("the source fired after the move archived it: %+v", fired)
		}
		got := c.tick(t, "2026-10-29T12:01:00Z", c.wsB.ID)
		if len(got) != 1 || *got[0].OccurrenceAt != "2026-10-28T09:00:00Z" {
			t.Fatalf("destination fired %+v, want the due occurrence once", got)
		}
		if n := c.count(t, `SELECT COUNT(*) FROM event_outbox WHERE event_type = ?`, kernelevents.ItemReminderDue); n != 1 {
			t.Errorf("%d reminder events across both doors, want exactly 1", n)
		}
	})
	t.Run("fire first", func(t *testing.T) {
		// This is a sequential ORDERING row: the fire commits, then the move
		// runs, and the row checks the outcome. The concurrent race is covered
		// by TestPGFireVersusMove's "move first" and "fire pins first" rows.
		t.Parallel()
		c := newSchedCopy(t)
		sc := c.schedule(t, daily())
		c.forcePast(t, sc)
		rid := *c.sched(t, c.wsA.ID, c.src.ID).ReminderID
		if fired, err := c.s.fireOneReminder(rid, copyNowTS); err != nil || fired == nil {
			t.Fatalf("source fire = %+v %v", fired, err)
		}
		res := c.do(t, true)
		if !res.Schedule.Carried {
			t.Fatalf("report = %+v", res.Schedule)
		}
		if got := c.tick(t, "2026-10-29T12:01:00Z", c.wsB.ID); len(got) != 1 || *got[0].OccurrenceAt != "2026-10-28T09:00:00Z" {
			t.Fatalf("destination fired %+v, want the carried occurrence once", got)
		}
		if got := c.tick(t, "2026-12-31T00:00:00Z", c.wsA.ID); len(got) != 0 {
			t.Errorf("the source fired after the move: %+v", got)
		}
		if n := c.count(t, `SELECT COUNT(*) FROM event_outbox WHERE event_type = ?`, kernelevents.ItemReminderDue); n != 2 {
			t.Errorf("%d reminder events, want one per door (the source's fire, the destination's catch-up)", n)
		}
	})
}

// D3 fold-in: cap_scan and cap_stall are deterministic work caps of the rule,
// not load, so a retry cannot help: a MOVE is refused as not carried (409
// schedule_not_carried) and a COPY skips and reports the reason. Only cap_time
// is the 503. SEAM: carryLimits (serial, like the cap_time row).
func TestCapScanIsNotCarriedNotBusy(t *testing.T) {
	old := carryLimits
	carryLimits.MaxScan = 1
	defer func() { carryLimits = old }()
	t.Run("copy skips", func(t *testing.T) {
		c := newSchedCopy(t)
		c.scheduleFuture(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Floating: true, Rrule: "FREQ=DAILY"})
		res := c.do(t, false)
		if res.Schedule.Carried || res.Schedule.Skipped != "timing:cap_scan" {
			t.Fatalf("copy report = %+v, want skipped timing:cap_scan", res.Schedule)
		}
	})
	t.Run("move is refused", func(t *testing.T) {
		c := newSchedCopy(t)
		sc := c.scheduleFuture(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Floating: true, Rrule: "FREQ=DAILY"})
		itemsB := countItemsIn(t, c.s, c.wsB.ID)
		req := c.req()
		req.SourceItemID, req.ArchiveSource, req.scheduleNow = c.src.ID, true, copyNow
		_, err := c.s.CopyItemAcrossWorkspaces(req)
		var nc *ScheduleNotCarriedError
		if !errors.As(err, &nc) || nc.Reason != "timing:cap_scan" {
			t.Fatalf("err = %v, want ScheduleNotCarriedError timing:cap_scan", err)
		}
		var capErr *schedule.CapError
		if errors.As(err, &capErr) {
			t.Errorf("a deterministic cap surfaced as a retryable CapError: %v", err)
		}
		if got := countItemsIn(t, c.s, c.wsB.ID); got != itemsB {
			t.Errorf("destination items %d -> %d", itemsB, got)
		}
		if a := c.sched(t, c.wsA.ID, c.src.ID); a == nil || a.EndedAt != nil {
			t.Errorf("source schedule ended: %+v", a)
		}
		if n, _, _ := c.managed(t, sc.ID); n != 1 {
			t.Errorf("source managed rows = %d", n)
		}
	})
}

// D3 r2-1: the live read of a floating schedule's zone is the item's assignee,
// else its creator, in one statement. (The interleaving the single statement
// closes, a reassignment between two reads, is not deterministic to test.)
func TestScheduleRecipientZoneIsTheAssigneeElseTheCreator(t *testing.T) {
	t.Parallel()
	c := newSchedCopy(t)
	mk := func(email, name, zone string) *models.User {
		u := createTestUser(t, c.s, email, name, "password123")
		if _, err := c.s.SetUserTimezoneIfUnset(u.ID, zone); err != nil {
			t.Fatal(err)
		}
		return u
	}
	la := mk("r21-la@example.com", "r21la", "America/Los_Angeles")
	tokyo := mk("r21-tk@example.com", "r21tk", "Asia/Tokyo")
	zone := func() string {
		loc, err := c.s.scheduleRecipientZone(c.s.db, c.src.ID)
		if err != nil {
			t.Fatal(err)
		}
		return loc.String()
	}
	if got := zone(); got != "UTC" {
		t.Errorf("nobody: %s, want UTC", got)
	}
	c.exec(t, `UPDATE items SET created_by_user_id = ? WHERE id = ?`, tokyo.ID, c.src.ID)
	if got := zone(); got != "Asia/Tokyo" {
		t.Errorf("creator only: %s, want Asia/Tokyo", got)
	}
	c.exec(t, `UPDATE items SET assigned_user_id = ? WHERE id = ?`, la.ID, c.src.ID)
	if got := zone(); got != "America/Los_Angeles" {
		t.Errorf("assignee and creator: %s, want the assignee's America/Los_Angeles", got)
	}
}

// A copy carries no journal row, so every row is reported dropped, and the
// preflight reports the same number.
func TestCopyReportsJournalRowsDropped(t *testing.T) {
	t.Parallel()
	c := newSchedCopy(t)
	sc := c.schedule(t, daily())
	var rows int
	if err := c.s.db.QueryRow(c.s.q(`SELECT COUNT(*) FROM schedule_changes WHERE schedule_id = ?`), sc.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows == 0 {
		t.Fatal("fixture wrote no journal rows")
	}
	pre, err := c.s.PreviewScheduleCarry(c.s.db, c.wsA.ID, c.src.ID, "", c.actor, false, copyNow)
	if err != nil {
		t.Fatal(err)
	}
	res := c.do(t, false)
	if res.Schedule.ChangesDropped != rows || pre.ChangesDropped != rows {
		t.Fatalf("copy %d, preview %d, want %d", res.Schedule.ChangesDropped, pre.ChangesDropped, rows)
	}
}

// A carried move keeps every valid row; the classifier reports the rest, and
// the preview and the restore share it.
func TestClassifyScheduleChangesReportsOrphans(t *testing.T) {
	t.Parallel()
	good := models.ScheduleChangeExport{ID: "a", ItemID: "i", ScheduleID: "s", ChangeType: "update", ReasonSource: "system", RecordedAt: "2026-10-26T10:00:00Z"}
	badType := good
	badType.ID, badType.ChangeType = "b", "nope"
	badItem := good
	badItem.ID, badItem.ItemID = "c", "other"
	kept, drops := classifyScheduleChanges([]models.ScheduleChangeExport{good, badType, badItem},
		map[string]string{"i": "i2"}, map[string]string{"s": "s2"}, map[string]models.ScheduleExport{"s": {ItemID: "i"}})
	if len(kept) != 1 || kept[0].ID != "a" || len(drops) != 2 {
		t.Fatalf("kept %d, drops %d", len(kept), len(drops))
	}
}
