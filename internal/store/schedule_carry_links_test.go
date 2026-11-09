package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Links stay within one workspace (TASK-4353, 2a). A cross-workspace copy or
// move cannot carry them: the copy starts with none, a move removes the source's
// (it archives the item and ends the schedule), and the report counts what was
// not carried. A same-workspace move is not a carry and keeps them.

// linkedSource gives the source item two links in workspace A: it is the parent
// of one and the child of another.
func (c *schedCopy) linkedSource(t *testing.T) (src *models.ItemSchedule, others [2]*models.Item) {
	t.Helper()
	src = c.scheduleFuture(t, daily())
	var scs [2]*models.ItemSchedule
	for i := range others {
		others[i] = createTestItem(t, c.s, c.wsA.ID, c.colA.ID, "other", "")
		var err error
		if scs[i], err = c.s.createScheduleAt(c.wsA.ID, others[i].ID, daily(), copyNow); err != nil {
			t.Fatal(err)
		}
	}
	for _, l := range []linkRow{
		{ParentSchedule: src.ID, ParentRole: "target", ChildSchedule: scs[0].ID, ChildRole: "target"},
		{ParentSchedule: scs[1].ID, ParentRole: "target", ChildSchedule: src.ID, ChildRole: "target"},
	} {
		l.ID, l.Offset, l.CancelPolicy, l.CreatedAt, l.UpdatedAt = newID(), oneDay, models.LinkWaitingOn, nowNano(), nowNano()
		tx, err := c.s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := c.s.insertLinkTx(tx, c.wsA.ID, l); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	return src, others
}

func TestAMoveAcrossWorkspacesDropsTheLinksAndReportsTheCount(t *testing.T) {
	t.Parallel()
	c := newSchedCopy(t)
	src, _ := c.linkedSource(t)
	prev, err := c.s.PreviewScheduleCarry(c.s.db, c.wsA.ID, c.src.ID, "", c.actor, true, copyNow)
	if err != nil || prev.LinksDropped != 2 {
		t.Fatalf("preview %+v %v", prev, err)
	}
	res := c.do(t, true)
	if res.Schedule.LinksDropped != 2 || !res.Schedule.Carried {
		t.Fatalf("report %+v", res.Schedule)
	}
	if n := c.count(t, `SELECT COUNT(*) FROM schedule_links WHERE parent_schedule_id=? OR child_schedule_id=?`, src.ID, src.ID); n != 0 {
		t.Fatalf("%d links left on the moved source", n)
	}
	if n := c.count(t, `SELECT COUNT(*) FROM schedule_links`); n != 0 {
		t.Fatalf("%d links left in total, want the moved schedule's two gone", n)
	}
	if n := c.count(t, `SELECT COUNT(*) FROM schedule_links WHERE workspace_id=?`, c.wsB.ID); n != 0 {
		t.Fatalf("the destination holds %d links", n)
	}
}

func TestACopyAcrossWorkspacesGetsNoLinksAndTheSourceKeepsHers(t *testing.T) {
	t.Parallel()
	c := newSchedCopy(t)
	c.linkedSource(t)
	res := c.do(t, false)
	if res.Schedule.LinksDropped != 2 || !res.Schedule.Carried {
		t.Fatalf("report %+v", res.Schedule)
	}
	if n := c.count(t, `SELECT COUNT(*) FROM schedule_links WHERE workspace_id=?`, c.wsA.ID); n != 2 {
		t.Fatalf("the source lost links: %d left", n)
	}
	if n := c.count(t, `SELECT COUNT(*) FROM schedule_links WHERE workspace_id=?`, c.wsB.ID); n != 0 {
		t.Fatalf("the copy holds %d links", n)
	}
}

// A schedule that already ended is not ended again by the move, but its links
// still go: the item leaves the workspace.
func TestAMoveOfAnEndedLinkedScheduleStillDropsItsLinks(t *testing.T) {
	t.Parallel()
	c := newSchedCopy(t)
	src, _ := c.linkedSource(t)
	c.exec(t, `UPDATE item_schedules SET ended_at=?, next_at=NULL, next_wall=NULL WHERE id=?`, "2026-10-29T10:00:00Z", src.ID)
	res := c.do(t, true)
	if res.Schedule.LinksDropped != 2 || res.Schedule.Carried {
		t.Fatalf("report %+v", res.Schedule)
	}
	if n := c.count(t, `SELECT COUNT(*) FROM schedule_links`); n != 0 {
		t.Fatalf("%d links left", n)
	}
}

func TestAMoveToAnotherCollectionInTheWorkspaceKeepsTheLinks(t *testing.T) {
	t.Parallel()
	c := newSchedCopy(t)
	c.linkedSource(t)
	other := createTestCollection(t, c.s, c.wsA.ID, "Elsewhere")
	if _, err := c.s.MoveItem(c.src.ID, other.ID, "{}"); err != nil {
		t.Fatalf("MoveItem: %v", err)
	}
	if n := c.count(t, `SELECT COUNT(*) FROM schedule_links`); n != 2 {
		t.Fatalf("%d links after a same-workspace move, want 2", n)
	}
}

func TestDeletingAScheduleLeavesNoLinkRows(t *testing.T) {
	t.Parallel()
	c := newSchedCopy(t)
	_, others := c.linkedSource(t)
	if _, err := c.s.DeleteSchedule(c.wsA.ID, c.src.ID); err != nil {
		t.Fatal(err)
	}
	if n := c.count(t, `SELECT COUNT(*) FROM schedule_links`); n != 0 {
		t.Fatalf("%d link rows left for a deleted schedule", n)
	}
	if n := c.count(t, `SELECT COUNT(*) FROM item_schedules WHERE item_id IN (?,?)`, others[0].ID, others[1].ID); n != 2 {
		t.Fatal("the other ends lost their schedules")
	}
}
