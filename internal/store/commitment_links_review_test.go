package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Review fixes on time links (TASK-4353).

var twoDaysBefore = models.ScheduleLinkOffset{Mode: "elapsed", ElapsedSecs: -2 * 86400}

var fiveDays = models.ScheduleLinkOffset{Mode: "elapsed", ElapsedSecs: 5 * 86400}

// An explicit offset update wins over the per-occurrence offset a drag left.
func TestUpdatingALinkOffsetBeatsADraggedOccurrenceOffset(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	l := f.link(t, 0, 1, "target", "target", oneDay)
	if _, err := f.move(1, "target", "2030-03-07T09:00:00Z", nil); err != nil { // drag: override at three days
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_link_overrides WHERE offset_mode IS NOT NULL`); n != 1 {
		t.Fatalf("setup: %d offset overrides", n)
	}
	if _, err := f.s.UpdateScheduleLink(f.ws.ID, l.ID, models.ScheduleLinkUpdate{Offset: &fiveDays}, f.allEdit()); err != nil {
		t.Fatal(err)
	}
	if got := f.role(t, 1, "target"); got != "2030-03-09T09:00:00Z" {
		t.Fatalf("child target=%s, want the parent plus five days", got)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_link_overrides WHERE offset_mode IS NOT NULL`); n != 0 {
		t.Fatalf("%d stale offset overrides left for the occurrence", n)
	}
	// The next parent move follows the new offset, not the dragged one.
	if _, err := f.move(0, "target", "2030-03-10T09:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	if got := f.role(t, 1, "target"); got != "2030-03-15T09:00:00Z" {
		t.Fatalf("child target=%s after the parent moved", got)
	}
}

// ---- 4: a cancel_policy-only update plans no move.

func TestACancelPolicyOnlyUpdateMovesNothingAndAsksForNothing(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.exec(t, `UPDATE item_schedules SET roles_origin='native', deadline_days=2, deadline_secs=0, deadline_behavior='stays_owed' WHERE id=?`, f.scheds[1].ID)
	// A raw link to the child's deadline whose date does not match: an offset
	// change would be a renegotiation needing a reason and a revision.
	f.linkRaw(t, 0, 1, "target", "deadline", oneDay)
	var linkID string
	if err := f.s.db.QueryRow(`SELECT id FROM schedule_links`).Scan(&linkID); err != nil {
		t.Fatal(err)
	}
	before := f.role(t, 1, "deadline")
	pol := models.LinkCancelChildOccurrence
	if _, err := f.s.UpdateScheduleLink(f.ws.ID, linkID, models.ScheduleLinkUpdate{CancelPolicy: &pol}, f.allEdit()); err != nil {
		t.Fatalf("a policy-only update: %v", err)
	}
	if got := f.role(t, 1, "deadline"); got != before {
		t.Fatalf("deadline moved %s -> %s", before, got)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE change_type='move'`); n != 0 {
		t.Fatalf("%d move journal rows", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_links WHERE cancel_policy=?`, pol); n != 1 {
		t.Fatal("the policy was not stored")
	}
}

// ---- 5: a second drag takes mode, zone and all-day from the effective offset.

func TestASecondDragUsesTheOverriddenOffsetsMode(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.link(t, 0, 1, "target", "target", oneDay)
	if _, err := f.move(1, "target", "2030-03-07T09:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	// The occurrence's offset is a civil one (a different mode from the link's).
	f.exec(t, `UPDATE schedule_link_overrides SET offset_mode='civil',offset_elapsed=0,offset_days=3,offset_clock=0,offset_all_day=0,offset_zone='UTC'`)
	if _, err := f.move(1, "target", "2030-03-09T09:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_link_overrides WHERE offset_mode='civil'`); n != 1 {
		t.Fatalf("the second drag did not keep the occurrence's civil mode (%d civil rows)", n)
	}
	if _, err := f.move(0, "target", "2030-03-10T09:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	if got := f.role(t, 1, "target"); got != "2030-03-15T09:00:00Z" {
		t.Fatalf("child target=%s, want the parent plus the dropped five days", got)
	}
}

// ---- 6: the revision does not depend on the order links were read in.

func TestTheRevisionIsIndependentOfLinkReadOrder(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 3)
	f.link(t, 0, 1, "target", "target", oneDay)
	f.link(t, 0, 2, "target", "target", threeDays)
	revision := func(reverse bool) string {
		tx, err := f.s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		c, err := f.s.pinCommitmentTx(tx, f.ws.ID, "", nil, false, f.items[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if reverse {
			for i, j := 0, len(c.links)-1; i < j; i, j = i+1, j-1 {
				c.links[i], c.links[j] = c.links[j], c.links[i]
			}
		}
		p, err := f.s.planLinkedTx(tx, c, f.ws.ID, f.items[0].ID, "target", time.Date(2030, 3, 10, 9, 0, 0, 0, time.UTC), true)
		if err != nil {
			t.Fatal(err)
		}
		return p.revision
	}
	if a, b := revision(false), revision(true); a != b {
		t.Fatalf("revision depends on link order: %s vs %s", a, b)
	}
}

// ---- 7: the check just before COMMIT, isolated from the checks between writes.

func TestTheBudgetIsCheckedJustBeforeCommit(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.link(t, 0, 1, "target", "target", oneDay)
	base := time.Now()
	late := false
	f.s.linkClock = func() time.Time {
		if late {
			return base.Add(time.Hour)
		}
		return base
	}
	// The clock is on time through every write and every check between them; it
	// runs out only as the commit begins, so only the pre-COMMIT check can see it.
	f.s.beforeLinkCommit = func() { late = true }
	_, err := f.move(0, "target", "2030-03-10T09:00:00Z", nil)
	if !errors.Is(err, ErrLinkBudget) || IsDBOutcomeUnknown(err) {
		t.Fatalf("err=%v, want the budget refusal from the pre-COMMIT check", err)
	}
	if got := f.role(t, 0, "target"); got != "2030-03-04T09:00:00Z" {
		t.Fatalf("root moved to %s", got)
	}
}

// ---- 3: the component cap holds at insert, and an over-cap component can be repaired.

// capFix builds item 0 (hub) with 500 raw links down to items 1..125 (four
// roles each), plus item 126 to link in as the 501st.
func capFix(t *testing.T) *linkFix {
	t.Helper()
	f := newLinkFix(t, 127)
	f.exec(t, `UPDATE item_schedules SET roles_origin='native', earliest_days=-1, earliest_secs=0 WHERE id=?`, f.scheds[0].ID)
	for j := 1; j <= 125; j++ {
		for _, role := range []string{"target", "earliest", "deadline", "occupied_until"} {
			f.linkRaw(t, 0, j, "target", role, oneDay)
		}
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_links`); n != 500 {
		t.Fatalf("setup: %d links", n)
	}
	return f
}

func TestThe501stLinkOfAComponentIsRefusedAtInsert(t *testing.T) {
	t.Parallel()
	f := capFix(t)
	journal := f.count(t, `SELECT COUNT(*) FROM schedule_changes`)
	_, err := f.create(126, 0, "target", "earliest", twoDaysBefore, models.LinkWriteOptions{}, f.allEdit())
	var lr *LinkRefusedError
	if !errors.As(err, &lr) || lr.Code != "link_limit" {
		t.Fatalf("err=%v, want link_limit", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_links`); n != 500 {
		t.Fatalf("%d links after the refused write", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes`); n != journal {
		t.Fatalf("%d journal rows after the refused write", n-journal)
	}
}

func TestAnOverCapComponentCanStillBeRepaired(t *testing.T) {
	t.Parallel()
	f := capFix(t)
	f.linkRaw(t, 126, 0, "target", "earliest", twoDaysBefore) // 501, as data that predates the cap
	var id string
	if err := f.s.db.QueryRow(f.s.q(`SELECT id FROM schedule_links WHERE parent_schedule_id=?`), f.scheds[126].ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	pol := models.LinkCancelChildOccurrence
	if _, err := f.s.UpdateScheduleLink(f.ws.ID, id, models.ScheduleLinkUpdate{CancelPolicy: &pol}, f.allEdit()); err != nil {
		t.Fatalf("update of an over-cap component: %v", err)
	}
	if err := f.s.DeleteScheduleLink(f.ws.ID, id, f.allEdit()); err != nil {
		t.Fatalf("delete from an over-cap component: %v", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_links`); n != 500 {
		t.Fatalf("%d links after the delete", n)
	}
}

// ---- 2: nothing the caller cannot read changes what they are told.

// hiddenFix has parent 0, a readable child 1 and child 2 that the reader
// cannot read; with crossing, child 2 holds its own deadline two days after its
// target, so the move below carries its target past that bound.
func hiddenFix(t *testing.T, crossing bool) (*linkFix, models.CommitmentAccess) {
	t.Helper()
	f := newLinkFix(t, 3)
	if crossing {
		f.exec(t, `UPDATE item_schedules SET roles_origin='native', deadline_days=2, deadline_secs=0, deadline_behavior='stays_owed' WHERE id=?`, f.scheds[2].ID)
	}
	f.link(t, 0, 1, "target", "target", oneDay)
	f.link(t, 1, 2, "target", "target", oneDay)
	acc := f.access(0, 1)
	delete(acc.Readable, f.items[2].ID)
	return f, acc
}

func TestAHiddenCrossedBoundChangesNoResponse(t *testing.T) {
	t.Parallel()
	const to = "2030-03-20T09:00:00Z"
	paths := map[string]func(f *linkFix, acc models.CommitmentAccess) (any, error){
		"preview": func(f *linkFix, acc models.CommitmentAccess) (any, error) {
			return f.move(0, "target", to, func(in *models.LinkedMoveInput) { in.Access, in.Preview = acc, true })
		},
		"apply without a revision": func(f *linkFix, acc models.CommitmentAccess) (any, error) {
			return f.move(0, "target", to, func(in *models.LinkedMoveInput) { in.Access = acc })
		},
		"apply with a revision": func(f *linkFix, acc models.CommitmentAccess) (any, error) {
			prev, err := f.move(0, "target", to, func(in *models.LinkedMoveInput) { in.Access, in.Preview = acc, true })
			if err != nil {
				return nil, err
			}
			return f.move(0, "target", to, func(in *models.LinkedMoveInput) { in.Access, in.Revision = acc, prev.Revision })
		},
		"link write without a revision": func(f *linkFix, acc models.CommitmentAccess) (any, error) {
			var id string
			if err := f.s.db.QueryRow(f.s.q(`SELECT id FROM schedule_links WHERE child_schedule_id=?`), f.scheds[1].ID).Scan(&id); err != nil {
				return nil, err
			}
			return f.s.UpdateScheduleLink(f.ws.ID, id, models.ScheduleLinkUpdate{Offset: &fiveDays}, acc)
		},
	}
	for name, path := range paths {
		name, path := name, path
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			show := func(crossing bool) string {
				t.Helper()
				f, acc := hiddenFix(t, crossing)
				res, err := path(f, acc)
				// The revision is an opaque token that hashes hidden state by design.
				switch r := res.(type) {
				case *models.LinkedMoveResult:
					if r != nil {
						c := *r
						c.Revision = ""
						res = c
					}
				case *models.ScheduleLinkResult:
					if r != nil {
						c := *r
						c.Moved.Revision = ""
						res = c
					}
				}
				if r, ok := res.(models.ScheduleLinkResult); ok {
					res = r.Moved // the link row carries random ids and stamps
				}
				out := fmt.Sprintf("%#v | %v", res, err)
				for i, it := range f.items {
					out = strings.ReplaceAll(out, it.ID, fmt.Sprintf("item%d", i))
				}
				return out
			}
			if a, b := show(false), show(true); a != b {
				t.Fatalf("a hidden crossed bound changed the response:\n without: %s\n with:    %s", a, b)
			}
		})
	}
}

// ---- finished children stay where they are.

func (f *linkFix) setStatus(t *testing.T, i int, status string) {
	t.Helper()
	f.exec(t, `UPDATE items SET fields = ? WHERE id = ?`, `{"status":"`+status+`"}`, f.items[i].ID)
}

func (f *linkFix) remindAt(t *testing.T, i int) string {
	t.Helper()
	var at string
	_ = f.s.db.QueryRow(f.s.q(`SELECT remind_at FROM item_reminders WHERE schedule_id=?`), f.scheds[i].ID).Scan(&at)
	return at
}

func (f *linkFix) journalRows(t *testing.T, i int) int {
	t.Helper()
	return f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=?`, f.scheds[i].ID)
}

// Item 0 is the parent; 1 is done, 2 abandoned, 3 open, 4 an open child of the
// done one, 5 an open child of the open one.
func finishedFix(t *testing.T) *linkFix {
	t.Helper()
	f := newLinkFix(t, 6)
	for _, c := range []int{1, 2, 3} {
		f.link(t, 0, c, "target", "target", oneDay)
	}
	f.link(t, 1, 4, "target", "target", oneDay)
	f.link(t, 3, 5, "target", "target", oneDay)
	f.setStatus(t, 1, "done")
	f.setStatus(t, 2, "cancelled")
	return f
}

func TestAFinishedChildStaysWhereItIsAndStopsTheCascade(t *testing.T) {
	t.Parallel()
	f := finishedFix(t)
	dates := map[int]string{}
	rem := map[int]string{}
	rows := map[int]int{}
	for i := 1; i <= 5; i++ {
		dates[i], rem[i], rows[i] = f.role(t, i, "target"), f.remindAt(t, i), f.journalRows(t, i)
	}
	to := "2030-03-10T09:00:00Z"
	prev, err := f.move(0, "target", to, func(in *models.LinkedMoveInput) { in.Preview = true })
	if err != nil {
		t.Fatal(err)
	}
	left := map[string]bool{}
	for _, l := range prev.LeftInPlace {
		left[l.ItemID] = true
	}
	if len(prev.LeftInPlace) != 2 || !left[f.items[1].ID] || !left[f.items[2].ID] {
		t.Fatalf("left in place=%+v, want the done and the abandoned child", prev.LeftInPlace)
	}
	if len(prev.Changes) != 3 { // the root, the open sibling and its child
		t.Fatalf("changes=%+v", prev.Changes)
	}
	if _, err := f.move(0, "target", to, func(in *models.LinkedMoveInput) { in.Revision = prev.Revision }); err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{1, 2, 4} {
		if got := f.role(t, i, "target"); got != dates[i] {
			t.Errorf("item %d moved %s -> %s", i, dates[i], got)
		}
		if f.remindAt(t, i) != rem[i] {
			t.Errorf("item %d reminder re-armed", i)
		}
		if f.journalRows(t, i) != rows[i] {
			t.Errorf("item %d has new journal rows", i)
		}
	}
	for _, i := range []int{3, 5} {
		if got := f.role(t, i, "target"); got == dates[i] {
			t.Errorf("open item %d did not move", i)
		}
	}
	// A reader of the parent alone is told nothing about the finished children.
	acc := f.access(0)
	acc.Readable = map[string]bool{f.items[0].ID: true, f.items[1].ID: true}
	hidden, err := f.move(0, "target", "2030-03-12T09:00:00Z", func(in *models.LinkedMoveInput) { in.Preview, in.Access = true, acc })
	if err != nil || len(hidden.LeftInPlace) != 1 || hidden.LeftInPlace[0].ItemID != f.items[1].ID {
		t.Fatalf("a reader of one finished child sees %+v %v", hidden.LeftInPlace, err)
	}
}

func TestALinkWriteLeavesAFinishedChildAlone(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	l := f.link(t, 0, 1, "target", "target", oneDay)
	f.setStatus(t, 1, "done")
	before, rem, rows := f.role(t, 1, "target"), f.remindAt(t, 1), f.journalRows(t, 1)
	prev, err := f.s.UpdateScheduleLink(f.ws.ID, l.ID, models.ScheduleLinkUpdate{Offset: &fiveDays, Move: models.LinkWriteOptions{Preview: true}}, f.allEdit())
	if err != nil || len(prev.Moved.LeftInPlace) != 1 {
		t.Fatalf("preview=%+v %v", prev, err)
	}
	if _, err := f.s.UpdateScheduleLink(f.ws.ID, l.ID, models.ScheduleLinkUpdate{Offset: &fiveDays}, f.allEdit()); err != nil {
		t.Fatal(err)
	}
	if f.role(t, 1, "target") != before || f.remindAt(t, 1) != rem || f.journalRows(t, 1) != rows {
		t.Fatal("a finished child moved with its link")
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_links WHERE offset_elapsed=?`, 5*86400); n != 1 {
		t.Fatal("the new offset was not stored")
	}
}

// A child that finishes (or reopens) between preview and apply invalidates the preview.
func TestAChildFinishingOrReopeningChangesTheRevision(t *testing.T) {
	t.Parallel()
	for _, hidden := range []bool{false, true} {
		for _, finish := range []bool{true, false} {
			hidden, finish := hidden, finish
			t.Run(fmt.Sprintf("hidden=%v/finish=%v", hidden, finish), func(t *testing.T) {
				t.Parallel()
				f := newLinkFix(t, 3)
				f.link(t, 0, 1, "target", "target", oneDay)
				f.link(t, 0, 2, "target", "target", oneDay)
				acc := f.allEdit()
				if hidden {
					delete(acc.Readable, f.items[1].ID)
				}
				if !finish {
					f.setStatus(t, 1, "done")
				}
				to := "2030-03-10T09:00:00Z"
				prev, err := f.move(0, "target", to, func(in *models.LinkedMoveInput) { in.Preview, in.Access = true, acc })
				if err != nil {
					t.Fatal(err)
				}
				if finish {
					f.setStatus(t, 1, "done")
				} else {
					f.setStatus(t, 1, "open")
				}
				rows := f.count(t, `SELECT COUNT(*) FROM schedule_changes`)
				if _, err := f.move(0, "target", to, func(in *models.LinkedMoveInput) { in.Revision, in.Access = prev.Revision, acc }); !errors.Is(err, ErrCommitmentChanged) {
					t.Fatalf("err=%v, want ErrCommitmentChanged", err)
				}
				if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes`); n != rows {
					t.Fatalf("%d journal rows written", n-rows)
				}
				if got := f.role(t, 0, "target"); got != "2030-03-04T09:00:00Z" {
					t.Fatalf("root moved to %s", got)
				}
			})
		}
	}
}

// Only a non-root finished node leaves the plan: a finished root still moves,
// and brings its open children with it.
func TestAFinishedRootStillMovesWithItsChildren(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.link(t, 0, 1, "target", "target", oneDay)
	f.setStatus(t, 0, "done")
	if _, err := f.move(0, "target", "2030-03-10T09:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	if got := f.role(t, 0, "target"); got != "2030-03-10T09:00:00Z" {
		t.Fatalf("finished root=%s", got)
	}
	if got := f.role(t, 1, "target"); got != "2030-03-11T09:00:00Z" {
		t.Fatalf("its open child=%s", got)
	}
	// A new link below a finished parent places the child from it.
	g := newLinkFix(t, 2)
	g.setStatus(t, 0, "done")
	g.link(t, 0, 1, "target", "target", threeDays)
	if got := g.role(t, 1, "target"); got != "2030-03-07T09:00:00Z" {
		t.Fatalf("child of a finished parent=%s", got)
	}
}
