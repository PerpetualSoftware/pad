package store

import (
	"database/sql"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The gated side of a gate is the item (TASK-4353): an item with no commitment
// can be gated, and a gate never depends on the gated item holding a schedule.
// The blocker side may lose its schedule, and its gates are then judged by the
// blocker item's status.

func (f *linkFix) gatesOf(t *testing.T, itemID string, acc models.CommitmentAccess) *models.ScheduleGateView {
	t.Helper()
	v, err := f.s.ScheduleGates(f.ws.ID, itemID, acc)
	if err != nil {
		t.Fatalf("ScheduleGates: %v", err)
	}
	return v
}

func (f *linkFix) expectOf(t *testing.T, what, itemID string, acc models.CommitmentAccess, state, outcome string) {
	t.Helper()
	v := f.gatesOf(t, itemID, acc)
	got := ""
	if len(v.Gates) == 1 {
		got = v.Gates[0].Outcome
	}
	if v.State != state || got != outcome {
		t.Fatalf("%s: state=%s outcome=%s, want %s %s", what, v.State, got, state, outcome)
	}
}

func TestAnItemWithNoCommitmentCanBeGatedReadWaitingOnAndOpened(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 1) // item 0 is the blocker, with a commitment
	pass := f.another(t, "iPhone pass")
	acc := f.gateAccess(pass)
	g := f.gate(t, pass.ID, f.items[0].ID, acc)
	f.expectOf(t, "gated item with no commitment", pass.ID, acc, models.GateStateWaiting, models.GateOutcomePending)
	f.resolve(t, 0, "done")
	f.expectOf(t, "blocker done", pass.ID, acc, models.GateStateReleased, models.GateOutcomeSucceeded)
	if !f.openedAt(t, g.ID).Valid {
		t.Fatal("the gate did not record that it opened")
	}
	if err := f.s.DeleteScheduleGate(f.ws.ID, g.ID, acc); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n := f.gateRows(t); n != 0 {
		t.Fatalf("%d gates left", n)
	}
	// Neither end needs a commitment.
	build := f.another(t, "build")
	acc = f.gateAccess(pass, build)
	f.gate(t, pass.ID, build.ID, acc)
	f.expectOf(t, "two items with no commitment", pass.ID, acc, models.GateStateWaiting, models.GateOutcomePending)
}

// The gated item may place a commitment later, and lose it again: the gate is
// the item's, so it is the same gate throughout.
func TestAGatedItemThatGetsACommitmentAndLosesItKeepsItsGate(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 1)
	pass := f.another(t, "iPhone pass")
	acc := f.gateAccess(pass)
	g := f.gate(t, pass.ID, f.items[0].ID, acc)
	f.native(t, pass, "2030-04-04T09:00", models.CommitmentRolesInput{})
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_gates WHERE id=? AND blocked_item_id=?`, g.ID, pass.ID); n != 1 {
		t.Fatal("placing a commitment changed the gate")
	}
	f.expectOf(t, "gated item placed", pass.ID, acc, models.GateStateWaiting, models.GateOutcomePending)
	if ok, err := f.s.DeleteSchedule(f.ws.ID, pass.ID); err != nil || !ok {
		t.Fatalf("DeleteSchedule: %v %v", ok, err)
	}
	if n := f.gateRows(t); n != 1 {
		t.Fatalf("deleting the gated item's commitment left %d gates, want 1", n)
	}
	f.expectOf(t, "gated item unplaced again", pass.ID, acc, models.GateStateWaiting, models.GateOutcomePending)
	f.resolve(t, 0, "done")
	f.expectOf(t, "blocker done", pass.ID, acc, models.GateStateReleased, models.GateOutcomeSucceeded)
}

// Deleting a blocker's COMMITMENT leaves the gate in place, judged by the blocker
// item's status from then on: terminal and not abandoned releases it, an
// abandoned or removed blocker item leaves the child blocked for a decision.
func TestDeletingABlockersCommitmentLeavesItsGateJudgedByTheItemStatus(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, status, state, outcome string
		remove                       bool
	}{
		{"open", "open", models.GateStateWaiting, models.GateOutcomePending, false},
		{"shipped", "shipped", models.GateStateReleased, models.GateOutcomeSucceeded, false},
		{"discarded", "discarded", models.GateStateBlockedNeedsDecision, models.GateOutcomeCancelled, false},
		{"soft-deleted", "open", models.GateStateBlockedNeedsDecision, models.GateOutcomeCancelled, true},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newLinkFix(t, 1)
			build := f.buildItem(t, "open")
			f.native(t, build, "2030-03-09T09:00", models.CommitmentRolesInput{})
			acc := f.gateAccess(build)
			g := f.gate(t, f.items[0].ID, build.ID, acc)
			if ok, err := f.s.DeleteSchedule(f.ws.ID, build.ID); err != nil || !ok {
				t.Fatalf("DeleteSchedule: %v %v", ok, err)
			}
			var sched sql.NullString
			if err := f.s.db.QueryRow(f.s.q(`SELECT blocker_schedule_id FROM schedule_gates WHERE id=?`), g.ID).Scan(&sched); err != nil {
				t.Fatalf("the gate did not survive its blocker's commitment: %v", err)
			}
			if sched.Valid {
				t.Fatalf("the gate still names the deleted schedule %s", sched.String)
			}
			f.setBlockerStatus(t, build, c.status)
			if c.remove {
				if err := f.s.DeleteItem(build.ID); err != nil {
					t.Fatal(err)
				}
			}
			f.expect(t, "blocker commitment deleted", 0, acc, c.state, c.outcome)
		})
	}
}

func TestAnOpenGateStaysOpenWhenItsBlockersCommitmentIsDeleted(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	acc := f.allEdit()
	g := f.gate(t, f.items[1].ID, f.items[0].ID, acc)
	f.resolve(t, 0, "done")
	f.expect(t, "done", 1, acc, models.GateStateReleased, models.GateOutcomeSucceeded)
	opened := f.openedAt(t, g.ID)
	if ok, err := f.s.DeleteSchedule(f.ws.ID, f.items[0].ID); err != nil || !ok {
		t.Fatalf("DeleteSchedule: %v %v", ok, err)
	}
	// The blocker item is still open, which would read pending if the gate were judged now.
	f.expect(t, "commitment deleted", 1, acc, models.GateStateReleased, models.GateOutcomeSucceeded)
	if f.openedAt(t, g.ID) != opened {
		t.Fatal("opened_at changed")
	}
}

// The gated item's own commitment going takes nothing from the gate, and the
// blocker's commitment going with a pending gate does not release the child.
func TestADeletedBlockerCommitmentNeverReleasesAPendingGate(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	acc := f.allEdit()
	f.gate(t, f.items[1].ID, f.items[0].ID, acc)
	if _, err := f.s.DeleteSchedule(f.ws.ID, f.items[0].ID); err != nil {
		t.Fatal(err)
	}
	f.expect(t, "blocker without a commitment, still open", 1, acc, models.GateStateWaiting, models.GateOutcomePending)
}

// Hard deletes of items exist only in a workspace purge, which takes the gated
// item and its gates with it.
func TestAWorkspacePurgeRemovesItsGates(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	plain := f.another(t, "plain blocker")
	f.gate(t, f.items[0].ID, f.items[1].ID, f.allEdit())
	f.gate(t, f.items[0].ID, plain.ID, f.gateAccess(plain))
	if err := f.s.DeleteWorkspace(f.ws.Slug); err != nil {
		t.Fatal(err)
	}
	if err := f.s.PurgeWorkspaceData(f.ws.ID); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n := f.gateRows(t); n != 0 {
		t.Fatalf("%d gates left after the purge", n)
	}
}

// A gated item put in the trash keeps its gate, and so does a restore.
func TestASoftDeletedGatedItemKeepsItsGateAcrossARestore(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	acc := f.allEdit()
	f.gate(t, f.items[1].ID, f.items[0].ID, acc)
	if err := f.s.DeleteItem(f.items[1].ID); err != nil {
		t.Fatal(err)
	}
	if n := f.gateRows(t); n != 1 {
		t.Fatalf("%d gates while the gated item is in the trash, want 1", n)
	}
	if _, err := f.s.RestoreItem(f.items[1].ID); err != nil {
		t.Fatal(err)
	}
	f.expect(t, "restored gated item", 1, acc, models.GateStateWaiting, models.GateOutcomePending)
}

// Moving the gated item to another collection of its workspace, or changing its
// status, leaves the gate where it is.
func TestAGatedItemsCollectionAndStatusLeaveItsGate(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	acc := f.allEdit()
	f.gate(t, f.items[1].ID, f.items[0].ID, acc)
	other := createTestCollection(t, f.s, f.ws.ID, "Elsewhere")
	if _, err := f.s.MoveItem(f.items[1].ID, other.ID, "{}"); err != nil {
		t.Fatalf("MoveItem: %v", err)
	}
	f.exec(t, `UPDATE items SET fields=? WHERE id=?`, `{"status":"done"}`, f.items[1].ID)
	f.expect(t, "gated item moved and done", 1, acc, models.GateStateWaiting, models.GateOutcomePending)
}
