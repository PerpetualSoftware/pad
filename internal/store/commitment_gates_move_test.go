package store

import (
	"errors"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

const gateMoveNow = "2030-03-04T10:00:00Z"

func movedGateFixture(t *testing.T, floating bool, until string) (*linkFix, *models.ScheduleGate, *models.ItemSchedule) {
	t.Helper()
	f := newLinkFix(t, 2)
	f.recurring(t, 0)
	if floating {
		f.exec(t, `UPDATE item_schedules SET floating=?,tz=NULL WHERE id=?`, true, f.scheds[0].ID)
	}
	graphGateRecipients(t, f)
	sc := f.reload(t, f.items[0].ID)
	g := f.gate(t, f.items[1].ID, f.items[0].ID, f.allEdit())
	target := rt(until)
	if _, err := f.s.resolveOccurrenceAt(f.ws.ID, f.items[0].ID, ResolveInput{
		OccurrenceAt: rt(*sc.NextAt), State: "move", Until: &target, Actor: "user",
	}, rt(gateMoveNow)); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=? AND state='moved'`, sc.ID); n != 1 {
		t.Fatalf("moved occurrences=%d, want 1", n)
	}
	return f, g, sc
}

func TestGatePassedOverMovedOccurrenceIsLapsed(t *testing.T) {
	for _, floating := range []bool{false, true} {
		for _, target := range []string{"2030-03-04T08:00:00Z", gateMoveNow} {
			name := "pinned/"
			if floating {
				name = "floating/"
			}
			t.Run(name+target, func(t *testing.T) {
				f, g, sc := movedGateFixture(t, floating, target)
				next := f.reload(t, f.items[0].ID)
				if next.NextAt == nil || next.NextWall == nil || OccurrenceKey(next, *next.NextAt, *next.NextWall) == f.gateKeyOf(t, g.ID) {
					t.Fatal("setup: schedule did not pass over the gated occurrence")
				}
				out, decided, err := f.s.occurrenceOutcomeQ(f.s.db, sc.ID, f.gateKeyOf(t, g.ID))
				if err != nil || out != schedule.OutcomeLapsed || !decided {
					t.Errorf("fresh occurrence outcome=%s decided=%v err=%v, want lapsed true nil", out, decided, err)
				}
				ev, err := f.s.evaluateAndOpenGates(f.ws.ID, f.items[1].ID)
				if err != nil {
					t.Fatal(err)
				}
				if len(ev.gates) != 1 || ev.gates[0].Outcome != models.GateOutcomeLapsed || ev.state != models.GateStateBlockedNeedsDecision {
					t.Errorf("transactional gate evaluation=%+v, want one lapsed gate blocked for a decision", ev)
				}
				if f.openedAt(t, g.ID).Valid || f.count(t, `SELECT COUNT(*) FROM user_notifications WHERE kind='commitment_gate_opened'`) != 0 {
					t.Error("passed-over move opened the gate or sent an opening notice")
				}
				f.expect(t, "passed-over moved blocker", 1, f.allEdit(), models.GateStateBlockedNeedsDecision, models.GateOutcomeLapsed)
			})
		}
	}
}

func TestGateFutureMovedOccurrenceStaysPending(t *testing.T) {
	for _, floating := range []bool{false, true} {
		name := "pinned"
		if floating {
			name = "floating"
		}
		t.Run(name, func(t *testing.T) {
			f, g, _ := movedGateFixture(t, floating, "2030-03-06T09:00:00Z")
			sc := f.reload(t, f.items[0].ID)
			if sc.NextAt == nil || sc.NextWall == nil || OccurrenceKey(sc, *sc.NextAt, *sc.NextWall) != f.gateKeyOf(t, g.ID) {
				t.Fatal("future move changed the outstanding occurrence identity")
			}
			out, decided, err := f.s.occurrenceOutcomeQ(f.s.db, sc.ID, f.gateKeyOf(t, g.ID))
			if err != nil || out != schedule.OutcomePending || decided {
				t.Fatalf("future occurrence outcome=%s decided=%v err=%v, want pending false nil", out, decided, err)
			}
			f.expect(t, "future moved blocker", 1, f.allEdit(), models.GateStateWaiting, models.GateOutcomePending)
		})
	}
}

func TestGatePassedOverMovedOccurrenceDoneIsStale(t *testing.T) {
	f, g, sc := movedGateFixture(t, false, "2030-03-04T08:00:00Z")
	var stale *ErrStaleOccurrence
	if _, err := f.s.resolveOccurrenceAt(f.ws.ID, f.items[0].ID, ResolveInput{
		OccurrenceAt: rt(*sc.NextAt), State: "done", Actor: "user",
	}, rt(gateMoveNow)); !errors.As(err, &stale) {
		t.Fatalf("Done on passed-over moved occurrence: %v, want ErrStale", err)
	}
	if f.openedAt(t, g.ID).Valid {
		t.Fatal("stale Done opened the gate")
	}
}

func TestGatePassedOverMovedOccurrenceDecisionActions(t *testing.T) {
	for _, action := range []string{"release", "re-gate"} {
		t.Run(action, func(t *testing.T) {
			f, g, sc := movedGateFixture(t, false, "2030-03-04T08:00:00Z")
			acc := f.allEdit()
			if state, outcome := f.verdict(t, 1, acc); state != models.GateStateBlockedNeedsDecision || outcome != models.GateOutcomeLapsed {
				t.Errorf("before decision: state=%s outcome=%s, want blocked_needs_decision lapsed", state, outcome)
			}
			if err := f.s.DeleteScheduleGate(f.ws.ID, g.ID, acc); err != nil {
				t.Fatal(err)
			}
			if action == "release" {
				f.expect(t, "released by decision", 1, acc, models.GateStateReleased, "")
				f.resolve(t, 1, "done")
				return
			}
			later := f.gate(t, f.items[1].ID, f.items[0].ID, acc)
			if f.gateKeyOf(t, later.ID) == OccurrenceKey(sc, *sc.NextAt, *sc.NextWall) {
				t.Fatal("re-gate kept the passed-over occurrence")
			}
			f.expect(t, "re-gated onto next occurrence", 1, acc, models.GateStateWaiting, models.GateOutcomePending)
			f.resolve(t, 0, "done")
			f.expect(t, "later blocker done", 1, acc, models.GateStateReleased, models.GateOutcomeSucceeded)
		})
	}
}
