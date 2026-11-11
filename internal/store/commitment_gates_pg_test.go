package store

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Postgres lock-order tests for gate writes (TASK-4353, 2c). A gate write is a
// link write behind the same pin, so it queues with a cascade and with a resolve
// on the workspace and never asks for an item row other than its root. The
// harness is the one in commitment_links_pg_test.go: the transactions under
// test queue behind a held workspace row and are then released together.

func (f *linkPG) gate(blocked, blocker int) error {
	_, err := f.s.CreateScheduleGate(f.ws.ID, f.items[blocked].ID, f.items[blocker].ID, f.access())
	return err
}

func (f *linkPG) done(i int) error {
	_, err := f.s.ResolveOccurrence(f.ws.ID, f.items[i].ID, ResolveInput{OccurrenceAt: rt(*f.scs[i].NextAt), State: "done", Actor: "user"})
	return err
}

// Two gate writes that point at each other cannot deadlock: one is stored, the
// other is refused as a cycle.
func TestTwoCrossingGateWritesDoNotDeadlock(t *testing.T) {
	f := newLinkPG(t, 2)
	errs := f.run(t,
		func() error { return f.gate(0, 1) },
		func() error { return f.gate(1, 0) })
	if deadlocked(errs) != 0 {
		t.Fatalf("errs=%v", errs)
	}
	refused := 0
	for _, err := range errs {
		var lr *LinkRefusedError
		if errors.As(err, &lr) && lr.Code == "link_cycle" {
			refused++
		} else if err != nil {
			t.Fatalf("errs=%v", errs)
		}
	}
	if refused != 1 {
		t.Fatalf("errs=%v, want one gate stored and one refused as a cycle", errs)
	}
}

// A gate write against a cascade of the blocker's own links: the cascade holds
// the blocker item and the gate write holds the gated item, each waiting for
// the workspace, and neither needs the other's item row.
func TestGateWriteAgainstACascadeDoesNotDeadlock(t *testing.T) {
	f := newLinkPG(t, 3)
	f.link(t, 0, 1)
	f.link(t, 1, 2)
	errs := f.run(t,
		func() error { return f.move(0, "2030-03-10T09:00:00Z") },
		func() error { return f.gate(1, 0) },
		func() error { return f.gate(2, 1) })
	for _, err := range errs {
		if err != nil {
			t.Fatalf("errs=%v", errs)
		}
	}
	var n int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM schedule_gates`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("gates=%d %v", n, err)
	}
}

// A gate write against a resolve of the gated child: both want the child item
// row, the gate write as its root, and the pin takes it first.
func TestGateWriteAgainstAResolveOfTheGatedChildDoesNotDeadlock(t *testing.T) {
	f := newLinkPG(t, 2)
	errs := f.run(t,
		func() error { return f.gate(1, 0) },
		func() error { return f.done(1) })
	for _, err := range errs {
		if err != nil {
			t.Fatalf("errs=%v", errs)
		}
	}
	var n int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM schedule_gates`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("gates=%d %v", n, err)
	}
}

// A gate write against a resolve of its BLOCKER: the resolve holds the blocker
// item and waits for the workspace; the gate write holds the gated item and then
// needs the blocker's schedule row, never its item row, so there is no cycle.
func TestGateWriteAgainstAResolveOfTheBlockerDoesNotDeadlock(t *testing.T) {
	f := newLinkPG(t, 2)
	errs := f.run(t,
		func() error { return f.gate(1, 0) },
		func() error { return f.done(0) })
	for _, err := range errs {
		if err != nil {
			t.Fatalf("errs=%v", errs)
		}
	}
	var sched, key string
	if err := f.s.db.QueryRow(`SELECT blocker_schedule_id, blocker_occurrence_key FROM schedule_gates`).Scan(&sched, &key); err != nil || sched != f.scs[0].ID {
		t.Fatalf("gate schedule=%q key=%q %v", sched, key, err)
	}
}

// A gate delete whose blocker is soft-deleted still takes the root item first,
// then the workspace, and queues with a cascade without deadlock.
func TestGateDeleteWithASoftDeletedBlockerAgainstACascadeDoesNotDeadlock(t *testing.T) {
	f := newLinkPG(t, 3)
	f.link(t, 1, 2)
	if err := f.gate(1, 0); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := f.s.db.QueryRow(`SELECT id FROM schedule_gates`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeleteItem(f.items[0].ID); err != nil {
		t.Fatal(err)
	}
	errs := f.run(t,
		func() error { return f.s.DeleteScheduleGate(f.ws.ID, id, f.access()) },
		func() error { return f.move(1, "2030-03-20T09:00:00Z") })
	for _, err := range errs {
		if err != nil {
			t.Fatalf("errs=%v", errs)
		}
	}
	var n int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM schedule_gates`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("gates=%d %v", n, err)
	}
}

// A gate create queued behind a cross-workspace move of its blocker writes
// nothing: the pin re-validates under the locks.
func TestGateCreateQueuedBehindAMoveOfItsBlockerWritesNothing(t *testing.T) {
	f := newLinkPG(t, 2)
	commit := f.beginMoveOf(t, 0)
	err := f.queueBehindMove(t, commit, func() error { return f.gate(1, 0) })
	if err != ErrScheduleLinkNotFound {
		t.Fatalf("err=%v, want the ErrScheduleLinkNotFound value", err)
	}
	var n int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM schedule_gates`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("gates=%d %v", n, err)
	}
}

// ---- the item-keyed gate: deleting a blocker's commitment, and timing edits ----

func (f *linkPG) deleteSchedule(i int) error {
	_, err := f.s.DeleteSchedule(f.ws.ID, f.items[i].ID)
	return err
}

// Deleting a blocker's commitment sets the schedule id of its gates to NULL (the
// foreign key's own write on the gate rows) in the delete's transaction, under
// its single-item pin. It queues on the workspace with a gate write onto the same
// blocker, a delete of the gate, and a resolve of the gated child, and nothing
// crosses; a gate write that loses the race finds the blocker's schedule gone.
func TestDeletingABlockersCommitmentAgainstGateWritesDoesNotDeadlock(t *testing.T) {
	f := newLinkPG(t, 3)
	if err := f.gate(0, 1); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := f.s.db.QueryRow(`SELECT id FROM schedule_gates`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	errs := f.run(t,
		func() error { return f.deleteSchedule(1) },
		func() error { return f.gate(2, 1) },
		func() error { return f.s.DeleteScheduleGate(f.ws.ID, id, f.access()) },
		func() error { return f.done(0) })
	if deadlocked(errs) != 0 {
		t.Fatalf("errs=%v", errs)
	}
	for _, err := range errs {
		if err != nil && err != ErrScheduleLinkNotFound {
			t.Fatalf("errs=%v", errs)
		}
	}
	var dangling int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM schedule_gates g WHERE g.blocker_schedule_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM item_schedules s WHERE s.id=g.blocker_schedule_id)`).Scan(&dangling); err != nil || dangling != 0 {
		t.Fatalf("dangling=%d %v", dangling, err)
	}
}

// A timing edit of a blocker racing a gate write onto it: whichever goes first,
// the gate ends on the blocker's CURRENT outstanding occurrence. The queue order
// is the order of the functions, so both are run: with the gate first it is the
// edit's rekey that moves it.
func TestATimingEditOfTheBlockerRacingAGateWriteLeavesTheGateOnTheCurrentKey(t *testing.T) {
	for _, gateFirst := range []bool{true, false} {
		gateFirst := gateFirst
		name := "edit first"
		if gateFirst {
			name = "gate first"
		}
		t.Run(name, func(t *testing.T) {
			f := newLinkPG(t, 2)
			edit := func() error {
				_, err := f.s.UpdateSchedule(f.ws.ID, f.items[1].ID, models.ScheduleUpdate{Dtstart: sp("2030-03-10T09:00")})
				return err
			}
			gate := func() error { return f.gate(0, 1) }
			fns := []func() error{edit, gate}
			if gateFirst {
				fns = []func() error{gate, edit}
			}
			for _, err := range f.run(t, fns...) {
				if err != nil {
					t.Fatalf("%v", err)
				}
			}
			sc, err := f.s.GetSchedule(f.ws.ID, f.items[1].ID)
			if err != nil || sc == nil || sc.NextAt == nil {
				t.Fatalf("schedule %v %v", sc, err)
			}
			var key string
			if err := f.s.db.QueryRow(`SELECT blocker_occurrence_key FROM schedule_gates`).Scan(&key); err != nil {
				t.Fatal(err)
			}
			if want := *sc.NextAt + "|" + *sc.NextWall; key != want {
				t.Fatalf("gate key %q, want the blocker's current outstanding occurrence %q", key, want)
			}
		})
	}
}

// TestTASK4423_R12GateNoScheduleLocks checks the item pin even when the root
// has no schedule. The inverted pin stays entirely inside this test.
func TestTASK4423_R12GateNoScheduleLocks(t *testing.T) {
	for _, inverted := range []bool{true, false} {
		t.Run(fmt.Sprintf("pin/inverted=%v", inverted), func(t *testing.T) {
			f := newLinkPG(t, 2)
			if err := f.deleteSchedule(0); err != nil {
				t.Fatal(err)
			}
			release := f.holdWorkspace(t)
			defer release()
			done := make(chan error, 1)
			go func() {
				if !inverted {
					done <- f.gate(0, 1)
					return
				}
				tx, err := f.s.db.Begin()
				if err != nil {
					done <- err
					return
				}
				defer tx.Rollback()
				var id string
				if err = tx.QueryRow(`SELECT id FROM workspaces WHERE id=$1 FOR NO KEY UPDATE`, f.ws.ID).Scan(&id); err == nil {
					err = tx.QueryRow(`SELECT id FROM items WHERE id=$1 FOR NO KEY UPDATE`, f.items[0].ID).Scan(&id)
				}
				done <- err
			}()
			f.waitForWaiters(t, 1)
			probe, err := f.s.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			var id string
			err = probe.QueryRow(`SELECT id FROM items WHERE id=$1 FOR NO KEY UPDATE NOWAIT`, f.items[0].ID).Scan(&id)
			probe.Rollback()
			locked := err != nil
			if locked == inverted {
				t.Fatalf("root locked before workspace=%v, inverted=%v", locked, inverted)
			}
			if inverted {
				t.Log("inverted pin detected: root was not locked before workspace wait")
			}
			release()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("pin did not finish")
			}
		})
	}
	for _, competitor := range []string{"resolve", "cascade", "create", "delete"} {
		t.Run(competitor, func(t *testing.T) {
			f := newLinkPG(t, 4)
			if err := f.deleteSchedule(0); err != nil {
				t.Fatal(err)
			}
			f.link(t, 1, 2)
			var gate string
			if competitor == "delete" {
				if err := f.gate(3, 2); err != nil {
					t.Fatal(err)
				}
				if err := f.s.db.QueryRow(`SELECT id FROM schedule_gates WHERE blocked_item_id=$1`, f.items[3].ID).Scan(&gate); err != nil {
					t.Fatal(err)
				}
			}
			other := func() error { return f.done(1) }
			switch competitor {
			case "cascade":
				other = func() error { return f.move(1, "2030-03-10T09:00:00Z") }
			case "create":
				other = func() error { return f.gate(0, 2) }
			case "delete":
				other = func() error { return f.s.DeleteScheduleGate(f.ws.ID, gate, f.access()) }
			}
			for _, err := range f.run(t, func() error { return f.gate(0, 1) }, other) {
				if err != nil {
					t.Fatal(err)
				}
			}
			var n int
			if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM schedule_gates WHERE blocked_item_id=$1`, f.items[0].ID).Scan(&n); err != nil {
				t.Fatal(err)
			}
			want := 1
			if competitor == "create" {
				want = 2
			}
			if n != want {
				t.Fatalf("gates=%d want %d", n, want)
			}
			view, err := f.s.ScheduleGates(f.ws.ID, f.items[0].ID, f.access())
			if err != nil {
				t.Fatal(err)
			}
			if competitor == "resolve" {
				if view.State != models.GateStateReleased {
					t.Fatalf("resolved blocker state=%s", view.State)
				}
			} else if view.State != models.GateStateWaiting {
				t.Fatalf("pending blocker state=%s", view.State)
			}
		})
	}
}
