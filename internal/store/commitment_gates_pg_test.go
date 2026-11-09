package store

import (
	"errors"
	"testing"
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
