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

// A move with nothing fired and unresolved carries no catch-up: an overdue
// occurrence that never fired, and one that fired and was then resolved.
func TestMoveDoesNotCatchUpWhenNothingIsOutstanding(t *testing.T) {
	t.Parallel()
	t.Run("not fired", func(t *testing.T) {
		c := newSchedCopy(t)
		sc := c.schedule(t, daily())
		c.forcePast(t, sc)
		res := c.do(t, true)
		b := c.sched(t, c.wsB.ID, res.Item.ID)
		if b == nil {
			t.Fatal("no schedule in the destination")
		}
		if n, at, fired := c.managed(t, b.ID); n != 1 || fired || at <= copyNowTS {
			t.Errorf("managed row = %d %s fired=%v, want armed in the future", n, at, fired)
		}
		if got := c.tick(t, "2026-10-29T12:01:00Z", c.wsB.ID); len(got) != 0 {
			t.Errorf("the destination fired %d, want no catch-up", len(got))
		}
	})
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
	}
	for _, tc := range cases {
		for _, move := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s move=%v", tc.name, move), func(t *testing.T) {
				c := newSchedCopy(t)
				tc.setup(t, c)
				got, err := c.s.PreviewScheduleCarry(c.s.db, c.wsA.ID, c.src.ID, move, copyNow)
				if err != nil {
					t.Fatal(err)
				}
				if res := c.do(t, move); res.Schedule != got {
					t.Errorf("preview %+v != copy %+v", got, res.Schedule)
				}
			})
		}
	}
	t.Run("carry reports", func(t *testing.T) {
		c := newSchedCopy(t)
		c.history(t, c.schedule(t, daily()), 2)
		got, _ := c.s.PreviewScheduleCarry(c.s.db, c.wsA.ID, c.src.ID, false, copyNow)
		if !got.Carried || got.HistoryDropped != 2 {
			t.Errorf("preview = %+v", got)
		}
	})
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
// parked on the schedule row (it passed the item's KEY SHARE, which the archive
// does not conflict with) must find the item archived when it gets the row, and
// leave the ended schedule ended.
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
	waitForLockWait(t, c.s, "FOR UPDATE", patched)
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
