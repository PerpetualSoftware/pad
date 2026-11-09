package store

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The cancel policy and "No longer needed" (TASK-4353, 2c). A link's
// cancel_policy says what a cancelled parent occurrence does to the child
// occurrence it is paired with: waiting_on (the default) leaves it waiting;
// cancel_child_occurrence marks that one occurrence No longer needed, which
// records no miss. It never cancels the series and never touches finished work.
// The skip of the parent's outstanding occurrence is the cancel door.

// cancelLink links parent -> child (target, one day) with the given policy.
func (f *linkFix) cancelLink(t *testing.T, parent, child int, policy string) *models.ScheduleLink {
	t.Helper()
	res, err := f.s.CreateScheduleLink(f.ws.ID, models.ScheduleLinkCreate{ParentItemID: f.items[parent].ID, ParentRole: "target", ChildItemID: f.items[child].ID, ChildRole: "target", Offset: oneDay, CancelPolicy: policy}, f.allEdit())
	if err != nil {
		t.Fatalf("CreateScheduleLink: %v", err)
	}
	return res.Link
}

func (f *linkFix) skipAs(t *testing.T, i int, actor string) {
	t.Helper()
	sc := f.reload(t, f.items[i].ID)
	if _, err := f.s.ResolveOccurrence(f.ws.ID, f.items[i].ID, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "skip", Actor: actor, MaySkipBeforeDeadline: true}); err != nil {
		t.Fatalf("skip: %v", err)
	}
}

func (f *linkFix) noLongerNeeded(t *testing.T, i int) bool {
	t.Helper()
	gone, err := f.s.OccurrenceNoLongerNeeded(f.ws.ID, f.items[i].ID)
	if err != nil {
		t.Fatalf("OccurrenceNoLongerNeeded: %v", err)
	}
	return gone
}

func (f *linkFix) misses(t *testing.T, i int) int {
	t.Helper()
	return f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE change_type='missed_recorded' AND schedule_id=?`, f.scheds[i].ID)
}

func (f *linkFix) cancelRows(t *testing.T, i int) (n int, actor string) {
	t.Helper()
	n = f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=? AND change_type='update' AND reason LIKE 'No longer needed%'`, f.scheds[i].ID)
	if n > 0 {
		if err := f.s.db.QueryRow(f.s.q(`SELECT actor FROM schedule_changes WHERE schedule_id=? AND change_type='update' AND reason LIKE 'No longer needed%'`), f.scheds[i].ID).Scan(&actor); err != nil {
			t.Fatal(err)
		}
	}
	return
}

// Only the child paired with the cancelled parent occurrence, and only under the
// explicit policy: the default leaves a child waiting on its parent.
func TestACancelledParentOccurrenceMarksOnlyTheLinkedChildUnderTheExplicitPolicy(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 3)
	f.cancelLink(t, 0, 1, models.LinkCancelChildOccurrence)
	f.cancelLink(t, 0, 2, "") // the default
	before := f.reload(t, f.items[1].ID)
	f.skipAs(t, 0, "agent")
	if !f.noLongerNeeded(t, 1) {
		t.Fatal("the explicit cancel policy did not mark the child occurrence")
	}
	if f.noLongerNeeded(t, 2) {
		t.Fatal("the default policy marked the child: it waits on its parent")
	}
	if f.noLongerNeeded(t, 0) {
		t.Fatal("the cancelled parent occurrence is marked")
	}
	// Journalled on the child, under the actor; the waiting child has no such row.
	if n, actor := f.cancelRows(t, 1); n != 1 || actor != "agent" {
		t.Fatalf("journal rows=%d actor=%q, want one under agent", n, actor)
	}
	if n, _ := f.cancelRows(t, 2); n != 0 {
		t.Fatalf("the waiting child has %d no-longer-needed journal rows", n)
	}
	// The child's journal is read by people who may not read the parent: its row
	// names neither the parent item nor its schedule.
	var row string
	if err := f.s.db.QueryRow(f.s.q(`SELECT COALESCE(reason,'') || COALESCE(before_json,'') || COALESCE(after_json,'') || COALESCE(occurrence_key,'') FROM schedule_changes WHERE schedule_id=? AND change_type='update' AND reason LIKE 'No longer needed%'`), f.scheds[1].ID).Scan(&row); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{f.items[0].ID, f.scheds[0].ID, f.items[2].ID} {
		if strings.Contains(row, id) {
			t.Fatalf("the journal row names %s: %s", id, row)
		}
	}
	// The series is untouched: not ended, not advanced, no occurrence resolved.
	after := f.reload(t, f.items[1].ID)
	if after.EndedAt != nil || *after.NextAt != *before.NextAt || *after.NextWall != *before.NextWall {
		t.Fatalf("the child schedule changed: before=%+v after=%+v", before, after)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=?`, f.scheds[1].ID); n != 0 {
		t.Fatalf("%d occurrences resolved on the child", n)
	}
}

// Only a skip cancels. Done, snooze and move are not cancellations.
func TestOnlyASkipOfTheParentOccurrenceCancels(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.cancelLink(t, 0, 1, models.LinkCancelChildOccurrence)
	f.resolve(t, 0, "done")
	if f.noLongerNeeded(t, 1) {
		t.Fatal("a done parent occurrence marked the child")
	}
}

func TestANoLongerNeededOccurrenceRecordsNoMissAndALapsingOneDoesNotLapse(t *testing.T) {
	t.Parallel()
	for _, behavior := range []string{"stays_owed", "lapses"} {
		behavior := behavior
		t.Run(behavior, func(t *testing.T) {
			t.Parallel()
			f := newLinkFix(t, 3)
			f.cancelLink(t, 0, 1, models.LinkCancelChildOccurrence)
			f.cancelLink(t, 0, 2, "")
			f.deadline(t, 1, behavior)
			f.deadline(t, 2, behavior)
			f.skipAs(t, 0, "user")
			if !f.noLongerNeeded(t, 1) {
				t.Fatal("setup: the child is not marked")
			}
			res := f.eval(t, farFuture, 0)
			if n := f.misses(t, 1); n != 0 {
				t.Fatalf("the no-longer-needed occurrence recorded %d misses", n)
			}
			if n := f.misses(t, 2); n != 1 || res.Missed != 1 {
				t.Fatalf("the waiting child recorded %d misses (result %+v), want exactly its own", n, res)
			}
			if n := f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=? AND state='lapsed'`, f.scheds[1].ID); n != 0 {
				t.Fatal("a no-longer-needed occurrence lapsed")
			}
			if sc := f.reload(t, f.items[1].ID); sc.EndedAt != nil || sc.NextAt == nil {
				t.Fatal("the evaluator ended the series")
			}
			// Skipping the cancelled occurrence is the person closing it: it records no miss either.
			f.eval(t, farFuture, 0)
			if n := f.misses(t, 1); n != 0 {
				t.Fatalf("%d misses after a second tick", n)
			}
		})
	}
}

// A skip of the child's own occurrence, past its deadline and before the tick,
// records the miss first (nothing erases a miss that happened) unless the
// occurrence was cancelled with its parent's.
func TestSkippingANoLongerNeededOccurrenceRecordsNoMiss(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.cancelLink(t, 0, 1, models.LinkCancelChildOccurrence)
	f.deadline(t, 1, "stays_owed")
	f.skipAs(t, 0, "user")
	sc := f.reload(t, f.items[1].ID)
	// A moment after the child's deadline, before any tick.
	if _, err := f.s.resolveOccurrenceAt(f.ws.ID, f.items[1].ID, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "skip", Actor: "user", MaySkipBeforeDeadline: true}, rt("2030-03-06T00:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if n := f.misses(t, 1); n != 0 {
		t.Fatalf("closing a no-longer-needed occurrence recorded %d misses", n)
	}
}

// Finished work is history: a done child, a child whose series ended, and a
// removed child are left alone.
func TestTheCancelPolicyNeverTouchesFinishedWork(t *testing.T) {
	t.Parallel()
	for name, finish := range map[string]func(*testing.T, *linkFix){
		"child item is done": func(t *testing.T, f *linkFix) {
			f.exec(t, `UPDATE items SET fields=? WHERE id=?`, `{"status":"done"}`, f.items[1].ID)
		},
		"child series already ended": func(t *testing.T, f *linkFix) { f.resolve(t, 1, "done") },
		"child item removed": func(t *testing.T, f *linkFix) {
			if err := f.s.DeleteItem(f.items[1].ID); err != nil {
				t.Fatal(err)
			}
		},
	} {
		name, finish := name, finish
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newLinkFix(t, 2)
			f.cancelLink(t, 0, 1, models.LinkCancelChildOccurrence)
			finish(t, f)
			f.skipAs(t, 0, "user")
			if n := f.count(t, `SELECT COUNT(*) FROM schedule_link_overrides WHERE no_longer_needed_at IS NOT NULL`); n != 0 {
				t.Fatalf("%d occurrences marked", n)
			}
			if n, _ := f.cancelRows(t, 1); n != 0 {
				t.Fatalf("%d journal rows on finished work", n)
			}
		})
	}
}

// The mark is for one occurrence: once the child's moves on, the next is a
// normal occurrence that can record a miss.
func TestTheMarkBelongsToThatOccurrenceOnly(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.recurring(t, 1)
	f.cancelLink(t, 0, 1, models.LinkCancelChildOccurrence)
	f.deadline(t, 1, "stays_owed")
	first := f.reload(t, f.items[1].ID)
	f.skipAs(t, 0, "user")
	if !f.noLongerNeeded(t, 1) {
		t.Fatal("setup: the child occurrence is not marked")
	}
	f.skipAs(t, 1, "user") // the person closes the cancelled occurrence
	next := f.reload(t, f.items[1].ID)
	if *next.NextWall == *first.NextWall {
		t.Fatal("setup: the child did not advance")
	}
	if f.noLongerNeeded(t, 1) {
		t.Fatal("the next occurrence inherited the mark")
	}
	f.eval(t, farFuture, 0)
	rows, err := f.s.db.Query(f.s.q(`SELECT occurrence_key FROM schedule_changes WHERE change_type='missed_recorded' AND schedule_id=?`), f.scheds[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k)
	}
	if len(keys) != 1 || !strings.HasSuffix(keys[0], *next.NextWall) {
		t.Fatalf("misses %v, want exactly one, for the next occurrence %s", keys, *next.NextWall)
	}
}

// The mark and the skip commit together or not at all.
func TestTheCancelMarkIsInTheSkipsTransaction(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	if f.s.dialect.Driver() != DriverSQLite {
		t.Skip("uses a SQLite trigger to fail the mark")
	}
	f.cancelLink(t, 0, 1, models.LinkCancelChildOccurrence)
	f.exec(t, `CREATE TRIGGER fail_mark BEFORE INSERT ON schedule_link_overrides BEGIN SELECT RAISE(ABORT, 'mark refused'); END`)
	sc := f.reload(t, f.items[0].ID)
	if _, err := f.s.ResolveOccurrence(f.ws.ID, f.items[0].ID, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "skip", Actor: "user", MaySkipBeforeDeadline: true}); err == nil {
		t.Fatal("the skip succeeded though the mark failed")
	}
	if after := f.reload(t, f.items[0].ID); after.EndedAt != nil || *after.NextAt != *sc.NextAt {
		t.Fatalf("the skip took effect without its mark: %+v", after)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=?`, f.scheds[0].ID); n != 0 {
		t.Fatalf("%d occurrences recorded", n)
	}
}

func TestTheCancelMarkWorksWithOneConnection(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.cancelLink(t, 0, 1, models.LinkCancelChildOccurrence)
	f.s.db.SetMaxOpenConns(1)
	f.skipAs(t, 0, "user")
	if !f.noLongerNeeded(t, 1) {
		t.Fatal("the child is not marked")
	}
}

func TestNoLongerNeededOfAnItemWithoutAnOutstandingOccurrenceIsFalse(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	bare := f.another(t, "no date")
	if gone, err := f.s.OccurrenceNoLongerNeeded(f.ws.ID, bare.ID); err != nil || gone {
		t.Fatalf("gone=%v err=%v", gone, err)
	}
	f.resolve(t, 1, "done") // the one-off series ended
	if gone, err := f.s.OccurrenceNoLongerNeeded(f.ws.ID, f.items[1].ID); err != nil || gone {
		t.Fatalf("ended series: gone=%v err=%v", gone, err)
	}
}

// ---- an offset update leaves the mark ----

// markRaw writes the mark the cancel hook writes, on item i's outstanding
// occurrence, with an optional dragged offset on the same row.
func (f *linkFix) markRaw(t *testing.T, linkID string, i int, dragged bool) {
	t.Helper()
	sc := f.reload(t, f.items[i].ID)
	key := *sc.NextAt + "|" + *sc.NextWall
	var mode any
	if dragged {
		mode = "elapsed"
	}
	f.exec(t, `INSERT INTO schedule_link_overrides (id,link_id,occurrence_key,offset_mode,offset_elapsed,no_longer_needed_at,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?)`,
		newID(), linkID, key, mode, 3*86400, nowNano(), nowNano(), nowNano())
}

func (f *linkFix) markedRows(t *testing.T) (marked, withOffset int) {
	t.Helper()
	marked = f.count(t, `SELECT COUNT(*) FROM schedule_link_overrides WHERE no_longer_needed_at IS NOT NULL`)
	withOffset = f.count(t, `SELECT COUNT(*) FROM schedule_link_overrides WHERE no_longer_needed_at IS NOT NULL AND offset_mode IS NOT NULL`)
	return
}

func TestAnOffsetUpdateKeepsANoLongerNeededMark(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.recurring(t, 0) // the parent has an outstanding occurrence after the update
	l := f.link(t, 0, 1, "target", "target", oneDay)
	f.markRaw(t, l.ID, 1, true)
	childBefore := f.role(t, 1, "target")
	two := models.ScheduleLinkOffset{Mode: "elapsed", ElapsedSecs: 2 * 86400}
	if _, err := f.s.UpdateScheduleLink(f.ws.ID, l.ID, models.ScheduleLinkUpdate{Offset: &two}, f.allEdit()); err != nil {
		t.Fatal(err)
	}
	if !f.noLongerNeeded(t, 1) {
		t.Fatal("the offset update cleared the mark")
	}
	// The dragged offset on the same row is replaced; the mark is not.
	if marked, withOffset := f.markedRows(t); marked != 1 || withOffset != 0 {
		t.Fatalf("marked=%d with a dragged offset=%d, want 1 and 0", marked, withOffset)
	}
	// The marked occurrence is left alone by the plan, so its date did not move.
	if got := f.role(t, 1, "target"); got != childBefore {
		t.Fatalf("the update moved a no-longer-needed occurrence: %s -> %s", childBefore, got)
	}
}

func TestAParentMoveLeavesANoLongerNeededChildWhereItIs(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	l := f.link(t, 0, 1, "target", "target", oneDay)
	f.markRaw(t, l.ID, 1, false)
	before := f.role(t, 1, "target")
	if _, err := f.move(0, "target", "2030-03-10T09:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	if got := f.role(t, 1, "target"); got != before {
		t.Fatalf("a parent move carried a no-longer-needed occurrence: %s -> %s", before, got)
	}
	_ = sql.ErrNoRows
}

// ---- miss first, then mark, and disarm ----

func (f *linkFix) reminderCounts(t *testing.T, i int) (total, fired int) {
	t.Helper()
	total = f.count(t, `SELECT COUNT(*) FROM item_reminders WHERE schedule_id=?`, f.scheds[i].ID)
	fired = f.count(t, `SELECT COUNT(*) FROM item_reminders WHERE schedule_id=? AND fired_at IS NOT NULL`, f.scheds[i].ID)
	return
}

// skipAt skips item i's outstanding occurrence as of nowT.
func (f *linkFix) skipAt(t *testing.T, i int, nowT string) {
	t.Helper()
	sc := f.reload(t, f.items[i].ID)
	if _, err := f.s.resolveOccurrenceAt(f.ws.ID, f.items[i].ID, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "skip", Actor: "user", MaySkipBeforeDeadline: true}, rt(nowT)); err != nil {
		t.Fatalf("skip: %v", err)
	}
}

// A child whose deadline passed before the parent was skipped, and before any
// tick, has a miss that already happened: marking the occurrence records it
// first, once, as a skip does, and then marks it.
func TestMarkingRecordsAnUnrecordedDueMissFirstExactlyOnce(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.cancelLink(t, 0, 1, models.LinkCancelChildOccurrence)
	f.deadline(t, 1, "stays_owed") // the child's deadline is 2030-03-05 09:00
	f.skipAt(t, 0, "2030-03-06T00:00:00Z")
	if !f.noLongerNeeded(t, 1) {
		t.Fatal("the child occurrence is not marked")
	}
	if n := f.misses(t, 1); n != 1 {
		t.Fatalf("%d misses recorded before the mark, want exactly 1", n)
	}
	if n := f.deadlineNotices(t); n != 1 {
		t.Fatalf("%d deadline notices, want 1", n)
	}
	f.eval(t, farFuture, 0)
	if n := f.misses(t, 1); n != 1 {
		t.Fatalf("%d misses after a tick, want still 1", n)
	}
	if n := f.deadlineNotices(t); n != 1 {
		t.Fatalf("%d deadline notices after a tick, want still 1", n)
	}
}

// A miss the evaluator already recorded is not recorded again.
func TestMarkingDoesNotRecordAMissTheEvaluatorAlreadyRecorded(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.cancelLink(t, 0, 1, models.LinkCancelChildOccurrence)
	f.deadline(t, 1, "stays_owed")
	f.eval(t, "2030-03-05T12:00:00Z", 0)
	if n := f.misses(t, 1); n != 1 {
		t.Fatalf("setup: %d misses", n)
	}
	f.skipAt(t, 0, "2030-03-06T00:00:00Z")
	if !f.noLongerNeeded(t, 1) {
		t.Fatal("the child occurrence is not marked")
	}
	if n := f.misses(t, 1); n != 1 {
		t.Fatalf("%d misses after the mark, want the one already on record", n)
	}
}

// Only an open child records a miss: a finished one is left alone entirely.
func TestMarkingRecordsNoMissForAFinishedChild(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.cancelLink(t, 0, 1, models.LinkCancelChildOccurrence)
	f.deadline(t, 1, "stays_owed")
	f.exec(t, `UPDATE items SET fields=? WHERE id=?`, `{"status":"done"}`, f.items[1].ID)
	f.skipAt(t, 0, "2030-03-06T00:00:00Z")
	if f.noLongerNeeded(t, 1) {
		t.Fatal("a finished child was marked")
	}
	if n := f.misses(t, 1); n != 0 {
		t.Fatalf("%d misses recorded for a finished child", n)
	}
}

// The mark disarms the child's managed reminder, in the same transaction; a
// reminder that already fired is left alone, and so is everything under the
// default policy.
func TestMarkingDisarmsTheChildsArmedReminder(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 3)
	f.cancelLink(t, 0, 1, models.LinkCancelChildOccurrence)
	f.cancelLink(t, 0, 2, "") // the default policy
	for _, i := range []int{1, 2} {
		if total, _ := f.reminderCounts(t, i); total != 1 {
			t.Fatalf("setup: child %d has %d reminders", i, total)
		}
	}
	f.skipAs(t, 0, "user")
	if total, _ := f.reminderCounts(t, 1); total != 0 {
		t.Fatalf("the marked child still has %d reminders", total)
	}
	if total, _ := f.reminderCounts(t, 2); total != 1 {
		t.Fatalf("the waiting child has %d reminders, want 1", total)
	}
	if n := f.misses(t, 2); n != 0 {
		t.Fatalf("the waiting child recorded %d misses", n)
	}
	if f.noLongerNeeded(t, 2) {
		t.Fatal("the waiting child was marked")
	}
}

func TestMarkingLeavesAReminderThatAlreadyFired(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.cancelLink(t, 0, 1, models.LinkCancelChildOccurrence)
	f.exec(t, `UPDATE item_reminders SET fired_at=? WHERE schedule_id=?`, nowNano(), f.scheds[1].ID)
	f.skipAs(t, 0, "user")
	if !f.noLongerNeeded(t, 1) {
		t.Fatal("the child occurrence is not marked")
	}
	if total, fired := f.reminderCounts(t, 1); total != 1 || fired != 1 {
		t.Fatalf("reminders=%d fired=%d, want the fired row left alone", total, fired)
	}
}
