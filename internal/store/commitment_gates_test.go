package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Commitment gates (TASK-4353, 2c): the write side. A gate is one row per
// (blocker, gated schedule); its writes follow the link store's rules for who may
// write, what the refusal names, and the lock order.

// gateAccess is edit on every fixture item plus the extra items given.
func (f *linkFix) gateAccess(extra ...*models.Item) models.CommitmentAccess {
	a := f.allEdit()
	for _, it := range extra {
		a.Readable[it.ID], a.Editable[it.ID] = true, true
	}
	return a
}

// gate writes a gate and fails the test when it is refused.
func (f *linkFix) gate(t *testing.T, blocked, blocker string, acc models.CommitmentAccess) *models.ScheduleGate {
	t.Helper()
	g, err := f.s.CreateScheduleGate(f.ws.ID, blocked, blocker, acc)
	if err != nil {
		t.Fatalf("CreateScheduleGate: %v", err)
	}
	return g
}

func (f *linkFix) gateRows(t *testing.T) int {
	t.Helper()
	return f.count(t, `SELECT COUNT(*) FROM schedule_gates`)
}

func TestGateWriteNeedsEditOnBothEnds(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	plain := f.another(t, "build")
	for name, acc := range map[string]models.CommitmentAccess{
		"edit on the blocked item, read-only blocker": func() models.CommitmentAccess {
			a := f.access(0)
			a.Readable[plain.ID] = true
			return a
		}(),
		"edit on the blocker, read-only blocked item": func() models.CommitmentAccess {
			a := f.access()
			a.Readable[plain.ID], a.Editable[plain.ID] = true, true
			return a
		}(),
		"read-only on both": func() models.CommitmentAccess {
			a := f.access()
			a.Readable[plain.ID] = true
			return a
		}(),
	} {
		if _, err := f.s.CreateScheduleGate(f.ws.ID, f.items[0].ID, plain.ID, acc); !errors.Is(err, ErrLinkEditRequired) {
			t.Fatalf("%s: err=%v, want ErrLinkEditRequired", name, err)
		}
	}
	if n := f.gateRows(t); n != 0 {
		t.Fatalf("%d gates written by refused creates", n)
	}
	g := f.gate(t, f.items[0].ID, plain.ID, f.gateAccess(plain))
	ro := f.access(0)
	ro.Readable[plain.ID] = true
	if err := f.s.DeleteScheduleGate(f.ws.ID, g.ID, ro); !errors.Is(err, ErrLinkEditRequired) {
		t.Fatalf("delete with a read-only blocker: %v", err)
	}
	if n := f.gateRows(t); n != 1 {
		t.Fatalf("a refused delete removed the gate (%d left)", n)
	}
}

// A missing, unreadable, soft-deleted or other-workspace end all answer the one
// ErrScheduleLinkNotFound value, so the answer cannot say which it was.
func TestGateWriteAnswersOneNotFoundForAnyEndTheCallerCannotUse(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	plain := f.another(t, "build")
	deleted := f.another(t, "deleted build")
	if err := f.s.DeleteItem(deleted.ID); err != nil {
		t.Fatal(err)
	}
	other := createTestWorkspace(t, f.s, "Elsewhere")
	col := createTestCollection(t, f.s, other.ID, "Tasks")
	foreign := createTestItem(t, f.s, other.ID, col.ID, "foreign", "")

	unreadable := f.gateAccess(plain)
	delete(unreadable.Readable, plain.ID)
	missing := f.gateAccess()
	missing.Readable["no-such-item"], missing.Editable["no-such-item"] = true, true
	for name, c := range map[string]struct {
		blocked, blocker string
		acc              models.CommitmentAccess
	}{
		"unreadable blocker":        {f.items[0].ID, plain.ID, unreadable},
		"nonexistent blocker":       {f.items[0].ID, "no-such-item", missing},
		"soft-deleted blocker":      {f.items[0].ID, deleted.ID, f.gateAccess(deleted)},
		"blocker of another space":  {f.items[0].ID, foreign.ID, f.gateAccess(foreign)},
		"soft-deleted blocked item": {deleted.ID, plain.ID, f.gateAccess(deleted, plain)},
		"unreadable blocked item":   {f.items[1].ID, plain.ID, func() models.CommitmentAccess { a := f.gateAccess(plain); delete(a.Readable, f.items[1].ID); return a }()},
		"nonexistent blocked item": {"no-such-item", plain.ID, func() models.CommitmentAccess {
			a := f.gateAccess(plain)
			a.Readable["no-such-item"], a.Editable["no-such-item"] = true, true
			return a
		}()},
		"blocked item of another ws": {foreign.ID, plain.ID, f.gateAccess(foreign, plain)},
	} {
		if _, err := f.s.CreateScheduleGate(f.ws.ID, c.blocked, c.blocker, c.acc); err != ErrScheduleLinkNotFound {
			t.Fatalf("%s: err=%v, want the ErrScheduleLinkNotFound value", name, err)
		}
	}
	if n := f.gateRows(t); n != 0 {
		t.Fatalf("%d gates written", n)
	}
	if err := f.s.DeleteScheduleGate(f.ws.ID, "no-such-gate", f.allEdit()); err != ErrScheduleLinkNotFound {
		t.Fatalf("delete of a gate that does not exist: %v", err)
	}
	g := f.gate(t, f.items[0].ID, plain.ID, f.gateAccess(plain))
	hidden := f.gateAccess(plain)
	delete(hidden.Readable, plain.ID)
	if err := f.s.DeleteScheduleGate(f.ws.ID, g.ID, hidden); err != ErrScheduleLinkNotFound {
		t.Fatalf("delete with an unreadable blocker: %v, want the ErrScheduleLinkNotFound value", err)
	}
	if n := f.gateRows(t); n != 1 {
		t.Fatalf("an unreadable delete removed the gate")
	}
}

// A gate whose end was soft-deleted can still be removed, with the authority of
// a live one, and the remover cannot tell which end it was from the answer.
func TestGateDeleteWorksOnASoftDeletedEnd(t *testing.T) {
	t.Parallel()
	for _, end := range []string{"blocker", "blocked"} {
		end := end
		t.Run(end, func(t *testing.T) {
			t.Parallel()
			f := newLinkFix(t, 2)
			blocker := f.another(t, "build")
			bsc := f.native(t, blocker, "2030-03-09T09:00", models.CommitmentRolesInput{})
			_ = bsc
			g := f.gate(t, f.items[0].ID, blocker.ID, f.gateAccess(blocker))
			dead := blocker.ID
			if end == "blocked" {
				dead = f.items[0].ID
			}
			if err := f.s.DeleteItem(dead); err != nil {
				t.Fatal(err)
			}
			readOnly := f.gateAccess(blocker)
			readOnly.Editable = map[string]bool{}
			if err := f.s.DeleteScheduleGate(f.ws.ID, g.ID, readOnly); !errors.Is(err, ErrLinkEditRequired) {
				t.Fatalf("read-only caller on a soft-deleted end: %v", err)
			}
			if err := f.s.DeleteScheduleGate(f.ws.ID, g.ID, f.gateAccess(blocker)); err != nil {
				t.Fatalf("delete with a soft-deleted %s: %v", end, err)
			}
			if n := f.gateRows(t); n != 0 {
				t.Fatalf("%d gates left", n)
			}
		})
	}
}

func TestGatesAreRefusedForSelfDuplicateAndCycle(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 3)
	acc := f.allEdit()
	var refused *LinkRefusedError
	if _, err := f.s.CreateScheduleGate(f.ws.ID, f.items[0].ID, f.items[0].ID, acc); !errors.As(err, &refused) || refused.Code != "link_self" {
		t.Fatalf("self gate: %v", err)
	}
	f.gate(t, f.items[1].ID, f.items[0].ID, acc)
	if _, err := f.s.CreateScheduleGate(f.ws.ID, f.items[1].ID, f.items[0].ID, acc); !errors.As(err, &refused) || refused.Code != "gate_duplicate" {
		t.Fatalf("duplicate gate: %v", err)
	}
	f.gate(t, f.items[2].ID, f.items[1].ID, acc)
	_, err := f.s.CreateScheduleGate(f.ws.ID, f.items[0].ID, f.items[2].ID, acc)
	if !errors.As(err, &refused) || refused.Code != "link_cycle" {
		t.Fatalf("gate cycle: %v", err)
	}
	for _, id := range append(scheduleIDs(f.scheds), itemIDs(f.items)...) {
		if strings.Contains(err.Error(), id) {
			t.Fatalf("the cycle refusal names %s: %v", id, err)
		}
	}
	if n := f.gateRows(t); n != 2 {
		t.Fatalf("%d gates after the refusals, want 2", n)
	}
}

// Limits are checked at write, and the refusal carries a code and no count.
func TestGateLimitsAreCheckedAtWriteAndShowNoCount(t *testing.T) {
	t.Parallel()
	t.Run("gates on one schedule", func(t *testing.T) {
		t.Parallel()
		f := newLinkFix(t, 1)
		acc := f.allEdit()
		for i := 0; i < 50; i++ {
			b := f.another(t, "blocker")
			acc.Readable[b.ID], acc.Editable[b.ID] = true, true
			f.gate(t, f.items[0].ID, b.ID, acc)
		}
		extra := f.another(t, "one too many")
		acc.Readable[extra.ID], acc.Editable[extra.ID] = true, true
		_, err := f.s.CreateScheduleGate(f.ws.ID, f.items[0].ID, extra.ID, acc)
		var refused *LinkRefusedError
		if !errors.As(err, &refused) || refused.Code != "link_limit" || strings.ContainsAny(err.Error(), "0123456789") {
			t.Fatalf("err=%v, want link_limit with no count", err)
		}
		if n := f.gateRows(t); n != 50 {
			t.Fatalf("%d gates, want 50", n)
		}
	})
	t.Run("gates below the new gated item", func(t *testing.T) {
		t.Parallel()
		// 34 scheduled items, each gating every later one: 561 edges below item 0.
		f := newLinkFix(t, 34)
		for i := 0; i < 34; i++ {
			for j := i + 1; j < 34; j++ {
				f.exec(t, `INSERT INTO schedule_gates (id,workspace_id,blocked_schedule_id,blocker_item_id,blocker_schedule_id,blocker_occurrence_key,created_at) VALUES (?,?,?,?,?,'',?)`,
					newID(), f.ws.ID, f.scheds[j].ID, f.items[i].ID, f.scheds[i].ID, nowNano())
			}
		}
		extra := f.another(t, "blocker")
		_, err := f.s.CreateScheduleGate(f.ws.ID, f.items[0].ID, extra.ID, f.gateAccess(extra))
		var refused *LinkRefusedError
		if !errors.As(err, &refused) || refused.Code != "link_limit" || strings.ContainsAny(err.Error(), "0123456789") {
			t.Fatalf("err=%v, want link_limit with no count", err)
		}
		// A small graph is not refused.
		g := newLinkFix(t, 3)
		g.gate(t, g.items[1].ID, g.items[0].ID, g.allEdit())
		g.gate(t, g.items[2].ID, g.items[1].ID, g.allEdit())
	})
}

// The blocked item needs a commitment (the gate row names its schedule); the
// blocker may have none.
func TestGateNeedsACommitmentOnTheBlockedItemOnly(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 1)
	bare := f.another(t, "no date yet")
	if _, err := f.s.CreateScheduleGate(f.ws.ID, bare.ID, f.items[0].ID, f.gateAccess(bare)); !errors.Is(err, ErrScheduleNotFound) {
		t.Fatalf("a blocked item with no commitment: %v, want ErrScheduleNotFound", err)
	}
	plain := f.another(t, "build")
	f.gate(t, f.items[0].ID, plain.ID, f.gateAccess(plain))
}

// The blocker's outstanding occurrence is fixed at creation and stored as its key.
func TestGateStoresTheBlockersOutstandingOccurrenceKey(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 3)
	acc := f.gateAccess()
	plain := f.another(t, "build")
	acc.Readable[plain.ID], acc.Editable[plain.ID] = true, true
	read := func(g *models.ScheduleGate) (sched sql.NullString, key string) {
		t.Helper()
		if err := f.s.db.QueryRow(f.s.q(`SELECT blocker_schedule_id, blocker_occurrence_key FROM schedule_gates WHERE id=?`), g.ID).Scan(&sched, &key); err != nil {
			t.Fatal(err)
		}
		return
	}
	// A blocker with a commitment: its schedule and the key of its outstanding occurrence.
	g := f.gate(t, f.items[0].ID, f.items[1].ID, acc)
	sc := f.reload(t, f.items[1].ID)
	sched, key := read(g)
	if want := *sc.NextAt + "|" + *sc.NextWall; !sched.Valid || sched.String != sc.ID || key != want {
		t.Fatalf("schedule=%v key=%q, want %s %q", sched, key, sc.ID, want)
	}
	// A blocker with no commitment: no schedule, no key.
	g = f.gate(t, f.items[0].ID, plain.ID, acc)
	if sched, key = read(g); sched.Valid || key != "" {
		t.Fatalf("schedule=%v key=%q, want none", sched, key)
	}
	// A blocker whose commitment already ended has no outstanding occurrence: an empty key.
	f.exec(t, `UPDATE item_schedules SET ended_at=?, next_at=NULL, next_wall=NULL WHERE id=?`, nowNano(), f.scheds[2].ID)
	g = f.gate(t, f.items[0].ID, f.items[2].ID, acc)
	if sched, key = read(g); !sched.Valid || key != "" {
		t.Fatalf("schedule=%v key=%q, want the schedule and an empty key", sched, key)
	}
}

// ---- re-validation under the locks, as for a link write ----

func TestAGateCreateWhoseBlockerMovedAwayWhileItWaitedWritesNothing(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.s.afterCommitmentLocks = f.movedAway(1)
	if _, err := f.s.CreateScheduleGate(f.ws.ID, f.items[0].ID, f.items[1].ID, f.allEdit()); err != ErrScheduleLinkNotFound {
		t.Fatalf("err=%v, want the ErrScheduleLinkNotFound value", err)
	}
	if n := f.gateRows(t); n != 0 {
		t.Fatalf("%d gates on the moved-away end", n)
	}
}

func TestAGateCreateWhoseBlockedItemMovedAwayWhileItWaitedWritesNothing(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.s.afterCommitmentLocks = f.movedAway(0)
	_, err := f.s.CreateScheduleGate(f.ws.ID, f.items[0].ID, f.items[1].ID, f.allEdit())
	if err != ErrScheduleLinkNotFound {
		t.Fatalf("err=%v, want the ErrScheduleLinkNotFound value", err)
	}
	if n := f.gateRows(t); n != 0 {
		t.Fatalf("%d gates on the moved-away end", n)
	}
}

func TestAGateDeleteOfAGateThatVanishedWhileItWaitedIsNotFound(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	g := f.gate(t, f.items[0].ID, f.items[1].ID, f.allEdit())
	f.s.afterCommitmentLocks = func(tx *sql.Tx) {
		if _, err := tx.Exec(f.s.q(`DELETE FROM schedule_gates WHERE id=?`), g.ID); err != nil {
			panic(err)
		}
	}
	if err := f.s.DeleteScheduleGate(f.ws.ID, g.ID, f.allEdit()); err != ErrScheduleLinkNotFound {
		t.Fatalf("err=%v, want the ErrScheduleLinkNotFound value", err)
	}
}

// A gate write is one transaction behind the pin: the SQLite one-connection
// rule (no pool query inside it) holds.
func TestGateWritesWorkWithOneConnection(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.s.db.SetMaxOpenConns(1)
	plain := f.another(t, "build")
	g := f.gate(t, f.items[0].ID, plain.ID, f.gateAccess(plain))
	f.gate(t, f.items[1].ID, f.items[0].ID, f.allEdit())
	if err := f.s.DeleteScheduleGate(f.ws.ID, g.ID, f.gateAccess(plain)); err != nil {
		t.Fatal(err)
	}
	_ = fmt.Sprint
}
