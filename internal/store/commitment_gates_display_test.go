package store

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// ScheduleGates is the per-reader DISPLAY of a gate; evaluateAndOpenGates is the
// act (TASK-4353). What opens a gate, and what decides that a child is released,
// is the unfiltered evaluation: a reader who cannot see the blocker neither opens
// a gate wrongly nor reads a held child as released.

// withoutRead is access to every fixture item except the given ones.
func (f *linkFix) withoutRead(items ...*models.Item) models.CommitmentAccess {
	a := f.allEdit()
	for _, it := range items {
		delete(a.Readable, it.ID)
		delete(a.Editable, it.ID)
	}
	return a
}

func TestADisplayReaderWhoCannotSeeTheBlockerStillSeesTheChildHeld(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		act   func(*testing.T, *linkFix)
		state string
		open  bool
	}{
		{"blocker pending", func(*testing.T, *linkFix) {}, models.GateStateWaiting, false},
		{"blocker skipped", func(t *testing.T, f *linkFix) { f.resolve(t, 1, "skip") }, models.GateStateBlockedNeedsDecision, false},
		{"blocker done", func(t *testing.T, f *linkFix) { f.resolve(t, 1, "done") }, models.GateStateReleased, true},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newLinkFix(t, 2)
			g := f.gate(t, f.items[0].ID, f.items[1].ID, f.allEdit())
			c.act(t, f)
			reader := f.withoutRead(f.items[1])
			v := f.gates(t, 0, reader)
			if v.State != c.state || len(v.Gates) != 0 || len(v.Unresolved) != 0 {
				t.Fatalf("display for a reader who cannot see the blocker: %+v, want state %s and no detail", v, c.state)
			}
			b, _ := json.Marshal(v)
			for _, id := range []string{f.items[1].ID, g.ID} {
				if strings.Contains(string(b), id) {
					t.Fatalf("the display names %s: %s", id, b)
				}
			}
			// The gate opens on the blocker's outcome, whoever reads.
			if got := f.openedAt(t, g.ID).Valid; got != c.open {
				t.Fatalf("opened=%v, want %v", got, c.open)
			}
			// A reader of both ends gets the detail, and the same state.
			full := f.gates(t, 0, f.allEdit())
			if full.State != c.state || len(full.Gates) != 1 || full.Gates[0].BlockerItemID != f.items[1].ID {
				t.Fatalf("display for a reader of both ends: %+v", full)
			}
		})
	}
}

// The acting function takes no reader: it sees every gate, hidden ones included.
func TestOpeningAGateReadsTheUnfilteredEvaluation(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 3)
	f.gate(t, f.items[0].ID, f.items[1].ID, f.allEdit())
	f.gate(t, f.items[0].ID, f.items[2].ID, f.allEdit())
	f.resolve(t, 1, "done")
	ev, err := f.s.evaluateAndOpenGates(f.ws.ID, f.items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.gates) != 2 || ev.state != models.GateStateWaiting {
		t.Fatalf("evaluation=%+v, want both gates and waiting", ev)
	}
	opened := 0
	for _, g := range ev.gates {
		if g.OpenedAt != nil {
			opened++
			if g.BlockerItemID != f.items[1].ID {
				t.Fatalf("the wrong gate opened: %+v", g)
			}
		}
	}
	if opened != 1 {
		t.Fatalf("%d gates opened, want the one whose blocker succeeded", opened)
	}
	// A hidden pending blocker next to a visible succeeded one holds the child.
	v := f.gates(t, 0, f.withoutRead(f.items[2]))
	if v.State != models.GateStateWaiting || len(v.Gates) != 1 || v.Gates[0].BlockerItemID != f.items[1].ID || len(v.Unresolved) != 0 {
		t.Fatalf("display=%+v", v)
	}
}

// Moving a blocker to another workspace archives it here: its gate reads
// cancelled, the child stays blocked for a decision, and a reader who cannot
// read the archived blocker does not see the child released.
func TestAMovedBlockerLeavesTheChildBlockedForADecision(t *testing.T) {
	t.Parallel()
	c := newSchedCopy(t)
	c.scheduleFuture(t, daily())
	gated := createTestItem(t, c.s, c.wsA.ID, c.colA.ID, "Gated", "")
	if _, err := c.s.createScheduleAt(c.wsA.ID, gated.ID, daily(), copyNow); err != nil {
		t.Fatal(err)
	}
	both := models.CommitmentAccess{Readable: map[string]bool{gated.ID: true, c.src.ID: true}, Editable: map[string]bool{gated.ID: true, c.src.ID: true}}
	if _, err := c.s.CreateScheduleGate(c.wsA.ID, gated.ID, c.src.ID, both); err != nil {
		t.Fatal(err)
	}
	// A copy leaves the blocker where it is.
	c.do(t, false)
	if v, err := c.s.ScheduleGates(c.wsA.ID, gated.ID, both); err != nil || v.State != models.GateStateWaiting {
		t.Fatalf("after a copy of the blocker: %+v %v", v, err)
	}
	c.do(t, true)
	hidden := models.CommitmentAccess{Readable: map[string]bool{gated.ID: true}, Editable: map[string]bool{gated.ID: true}}
	for name, acc := range map[string]models.CommitmentAccess{"reader of both ends": both, "reader without the archived blocker": hidden} {
		v, err := c.s.ScheduleGates(c.wsA.ID, gated.ID, acc)
		if err != nil {
			t.Fatal(err)
		}
		if v.State != models.GateStateBlockedNeedsDecision {
			t.Fatalf("%s: state=%s, want blocked_needs_decision", name, v.State)
		}
	}
	if n := c.count(t, `SELECT COUNT(*) FROM schedule_gates WHERE opened_at IS NOT NULL`); n != 0 {
		t.Fatal("a moved blocker opened the gate")
	}
}
