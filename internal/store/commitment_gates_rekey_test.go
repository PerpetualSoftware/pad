package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// A timing edit of a blocker changes the key of its outstanding occurrence. A
// PENDING gate fixed on that occurrence follows it to the new outstanding one,
// in the edit's own transaction, or it would wait for an occurrence that no
// longer exists. An OPEN gate stays open, and a gate fixed on another occurrence
// is not touched.

func (f *linkFix) gateKeyOf(t *testing.T, gateID string) string {
	t.Helper()
	var key string
	if err := f.s.db.QueryRow(f.s.q(`SELECT blocker_occurrence_key FROM schedule_gates WHERE id=?`), gateID).Scan(&key); err != nil {
		t.Fatal(err)
	}
	return key
}

func (f *linkFix) outstandingKeyOf(t *testing.T, i int) string {
	t.Helper()
	sc := f.reload(t, f.items[i].ID)
	if sc.NextAt == nil || sc.NextWall == nil {
		return ""
	}
	return *sc.NextAt + "|" + *sc.NextWall
}

func TestATimingEditOfTheBlockerRekeysAPendingGateFixedOnTheEditedOccurrence(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	acc := f.allEdit()
	g := f.gate(t, f.items[1].ID, f.items[0].ID, acc)
	before := f.gateKeyOf(t, g.ID)
	if _, err := f.s.UpdateSchedule(f.ws.ID, f.items[0].ID, models.ScheduleUpdate{Dtstart: sp("2030-03-10T09:00")}); err != nil {
		t.Fatal(err)
	}
	want := f.outstandingKeyOf(t, 0)
	if want == before {
		t.Fatal("setup: the edit did not change the occurrence key")
	}
	if got := f.gateKeyOf(t, g.ID); got != want {
		t.Fatalf("gate key %q after the edit, want the new outstanding occurrence %q", got, want)
	}
	f.expect(t, "after the edit", 1, acc, models.GateStateWaiting, models.GateOutcomePending)
	f.resolve(t, 0, "done")
	f.expect(t, "the rescheduled blocker done", 1, acc, models.GateStateReleased, models.GateOutcomeSucceeded)
}

func TestATimingEditLeavesAnOpenGateAlone(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.recurring(t, 0)
	acc := f.allEdit()
	g := f.gate(t, f.items[1].ID, f.items[0].ID, acc)
	f.resolve(t, 0, "done")
	f.expect(t, "done", 1, acc, models.GateStateReleased, models.GateOutcomeSucceeded)
	key, opened := f.gateKeyOf(t, g.ID), f.openedAt(t, g.ID)
	if _, err := f.s.UpdateSchedule(f.ws.ID, f.items[0].ID, models.ScheduleUpdate{Dtstart: sp("2030-03-04T10:00")}); err != nil {
		t.Fatal(err)
	}
	if f.gateKeyOf(t, g.ID) != key || f.openedAt(t, g.ID) != opened {
		t.Fatal("a timing edit changed an open gate")
	}
	f.expect(t, "after the edit", 1, acc, models.GateStateReleased, models.GateOutcomeSucceeded)
}

// An open gate whose key IS the outstanding occurrence (it opened some other way)
// is not followed to the new one either.
func TestATimingEditLeavesAnOpenGateOnTheEditedOccurrenceAlone(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	acc := f.allEdit()
	g := f.gate(t, f.items[1].ID, f.items[0].ID, acc)
	f.exec(t, `UPDATE schedule_gates SET opened_at=? WHERE id=?`, nowNano(), g.ID)
	key := f.gateKeyOf(t, g.ID)
	if _, err := f.s.UpdateSchedule(f.ws.ID, f.items[0].ID, models.ScheduleUpdate{Dtstart: sp("2030-03-10T09:00")}); err != nil {
		t.Fatal(err)
	}
	if f.outstandingKeyOf(t, 0) == key {
		t.Fatal("setup: the edit did not change the occurrence key")
	}
	if got := f.gateKeyOf(t, g.ID); got != key {
		t.Fatalf("an open gate followed the edit: %q -> %q", key, got)
	}
	f.expect(t, "open gate", 1, acc, models.GateStateReleased, models.GateOutcomeSucceeded)
}

func TestATimingEditLeavesAGateFixedOnAnotherOccurrenceAlone(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 3)
	f.recurring(t, 0)
	acc := f.allEdit()
	first := f.gate(t, f.items[1].ID, f.items[0].ID, acc) // fixed on the 4th
	f.resolve(t, 0, "skip")
	second := f.gate(t, f.items[2].ID, f.items[0].ID, acc) // fixed on the 5th
	f.expect(t, "first occurrence skipped", 1, acc, models.GateStateBlockedNeedsDecision, models.GateOutcomeSkipped)
	firstKey, secondKey := f.gateKeyOf(t, first.ID), f.gateKeyOf(t, second.ID)
	if firstKey == secondKey {
		t.Fatal("setup: both gates are on one occurrence")
	}
	if _, err := f.s.UpdateSchedule(f.ws.ID, f.items[0].ID, models.ScheduleUpdate{Dtstart: sp("2030-03-04T10:00")}); err != nil {
		t.Fatal(err)
	}
	if got := f.gateKeyOf(t, first.ID); got != firstKey {
		t.Fatalf("the gate on the skipped occurrence moved: %q -> %q", firstKey, got)
	}
	want := f.outstandingKeyOf(t, 0)
	if got := f.gateKeyOf(t, second.ID); got != want || got == secondKey {
		t.Fatalf("the gate on the edited occurrence is %q, want %q (was %q)", got, want, secondKey)
	}
	f.expect(t, "skipped occurrence still skipped", 1, acc, models.GateStateBlockedNeedsDecision, models.GateOutcomeSkipped)
}

// An edit that leaves the outstanding occurrence's key as it was changes nothing.
func TestATimingEditThatKeepsTheKeyLeavesTheGateAlone(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	acc := f.allEdit()
	g := f.gate(t, f.items[1].ID, f.items[0].ID, acc)
	key := f.gateKeyOf(t, g.ID)
	if _, err := f.s.UpdateSchedule(f.ws.ID, f.items[0].ID, models.ScheduleUpdate{Rrule: sp("FREQ=WEEKLY")}); err != nil {
		t.Fatal(err)
	}
	if got := f.outstandingKeyOf(t, 0); got != key {
		t.Fatalf("setup: the key changed to %q", got)
	}
	if f.gateKeyOf(t, g.ID) != key {
		t.Fatal("the gate key changed")
	}
}

// An edit that leaves the schedule with no outstanding occurrence leaves the
// gate with no occurrence to wait for: the blocker item's status decides, as for
// a commitment that had already ended.
func TestATimingEditThatEndsTheBlockersScheduleFallsBackToTheItemStatus(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	acc := f.allEdit()
	g := f.gate(t, f.items[1].ID, f.items[0].ID, acc)
	// Pacific/Kiritimati is UTC+14: 09:00 there on the 4th is already past at the 4th, 00:00 UTC.
	if _, err := f.s.updateScheduleAt(f.ws.ID, f.items[0].ID, models.ScheduleUpdate{Tz: sp("Pacific/Kiritimati")}, rt("2030-03-04T00:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if sc := f.reload(t, f.items[0].ID); sc.EndedAt == nil {
		t.Fatalf("setup: the edit did not end the schedule: %+v", sc)
	}
	if got := f.gateKeyOf(t, g.ID); got != "" {
		t.Fatalf("gate key %q, want none: there is no outstanding occurrence", got)
	}
	f.expect(t, "blocker item open", 1, acc, models.GateStateWaiting, models.GateOutcomePending)
	f.exec(t, `UPDATE items SET fields=? WHERE id=?`, `{"status":"done"}`, f.items[0].ID)
	f.expect(t, "blocker item done", 1, acc, models.GateStateReleased, models.GateOutcomeSucceeded)
}
