package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Gates stay within one workspace (TASK-4353). A cross-workspace copy or move of
// a GATED item cannot carry them: the copy starts with none (the source keeps
// hers), a move deletes the source's so the moved item neither arrives unblocked
// by accident nor leaves an orphan gate behind, and the result counts what was
// not carried, only of blockers the mover can read. This is at the item level:
// an item with no commitment has no schedule carry to hang it on. A moved
// BLOCKER is not a gated item: its gates stay and read cancelled.

// gatedSource gates the source item behind two blockers in workspace A and
// returns them: the first readable by the mover, the second not.
func (c *schedCopy) gatedSource(t *testing.T, withCommitment bool) (readable, hidden *models.Item) {
	t.Helper()
	if withCommitment {
		c.scheduleFuture(t, daily())
	}
	readable = createTestItem(t, c.s, c.wsA.ID, c.colA.ID, "readable blocker", "")
	hidden = createTestItem(t, c.s, c.wsA.ID, c.colA.ID, "hidden blocker", "")
	acc := models.CommitmentAccess{Readable: map[string]bool{}, Editable: map[string]bool{}}
	for _, it := range []*models.Item{c.src, readable, hidden} {
		acc.Readable[it.ID], acc.Editable[it.ID] = true, true
	}
	for _, blocker := range []*models.Item{readable, hidden} {
		if _, err := c.s.CreateScheduleGate(c.wsA.ID, c.src.ID, blocker.ID, acc); err != nil {
			t.Fatal(err)
		}
	}
	return readable, hidden
}

func TestACrossWorkspaceMoveOfAGatedItemDropsItsGatesAndCountsTheReadableOnes(t *testing.T) {
	t.Parallel()
	for _, withCommitment := range []bool{true, false} {
		withCommitment := withCommitment
		name := "item without a commitment"
		if withCommitment {
			name = "item with a commitment"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := newSchedCopy(t)
			_, hidden := c.gatedSource(t, withCommitment)
			canRead := func(id string) bool { return id != hidden.ID }
			if n, err := c.s.ItemGatesDropped(c.wsA.ID, c.src.ID, canRead); err != nil || n != 1 {
				t.Fatalf("preview: %d %v, want only the readable blocker counted", n, err)
			}
			req := c.req()
			req.SourceItemID, req.ArchiveSource, req.scheduleNow, req.SourceChildReadable = c.src.ID, true, copyNow, canRead
			res := c.copy(t, req)
			if res.GatesDropped != 1 {
				t.Fatalf("GatesDropped=%d, want 1: the hidden blocker is deleted and not counted", res.GatesDropped)
			}
			if n := c.count(t, `SELECT COUNT(*) FROM schedule_gates`); n != 0 {
				t.Fatalf("%d gates left after the move, including the hidden one", n)
			}
		})
	}
}

func TestACrossWorkspaceCopyOfAGatedItemGetsNoGatesAndTheSourceKeepsHers(t *testing.T) {
	t.Parallel()
	for _, withCommitment := range []bool{true, false} {
		withCommitment := withCommitment
		t.Run(map[bool]string{true: "with a commitment", false: "without"}[withCommitment], func(t *testing.T) {
			t.Parallel()
			c := newSchedCopy(t)
			c.gatedSource(t, withCommitment)
			res := c.do(t, false)
			if res.GatesDropped != 2 {
				t.Fatalf("GatesDropped=%d, want 2", res.GatesDropped)
			}
			if n := c.count(t, `SELECT COUNT(*) FROM schedule_gates WHERE workspace_id=?`, c.wsA.ID); n != 2 {
				t.Fatalf("the source kept %d gates, want 2", n)
			}
			if n := c.count(t, `SELECT COUNT(*) FROM schedule_gates WHERE workspace_id=?`, c.wsB.ID); n != 0 {
				t.Fatalf("the copy holds %d gates", n)
			}
		})
	}
}

// A same-workspace move keeps the gates: it is not a carry.
func TestAMoveToAnotherCollectionKeepsTheGates(t *testing.T) {
	t.Parallel()
	c := newSchedCopy(t)
	c.gatedSource(t, false)
	other := createTestCollection(t, c.s, c.wsA.ID, "Elsewhere")
	if _, err := c.s.MoveItem(c.src.ID, other.ID, "{}"); err != nil {
		t.Fatalf("MoveItem: %v", err)
	}
	if n := c.count(t, `SELECT COUNT(*) FROM schedule_gates`); n != 2 {
		t.Fatalf("%d gates after a same-workspace move, want 2", n)
	}
}

// A moved BLOCKER is not a gated item: nothing is dropped or counted, and its
// gates stay behind, reading cancelled.
func TestMovingABlockerDropsNoGate(t *testing.T) {
	t.Parallel()
	c := newSchedCopy(t)
	gated := createTestItem(t, c.s, c.wsA.ID, c.colA.ID, "Gated", "")
	acc := models.CommitmentAccess{Readable: map[string]bool{gated.ID: true, c.src.ID: true}, Editable: map[string]bool{gated.ID: true, c.src.ID: true}}
	if _, err := c.s.CreateScheduleGate(c.wsA.ID, gated.ID, c.src.ID, acc); err != nil {
		t.Fatal(err)
	}
	res := c.do(t, true)
	if res.GatesDropped != 0 {
		t.Fatalf("GatesDropped=%d for a moved blocker", res.GatesDropped)
	}
	if n := c.count(t, `SELECT COUNT(*) FROM schedule_gates`); n != 1 {
		t.Fatalf("%d gates, want the blocker's gate left", n)
	}
	if v, err := c.s.ScheduleGates(c.wsA.ID, gated.ID, acc); err != nil || v.State != models.GateStateBlockedNeedsDecision {
		t.Fatalf("gate state after the blocker moved: %+v %v", v, err)
	}
}
