package store

import (
	"reflect"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4590: a snooze on a moved occurrence changes only when the reminder
// re-fires. The moved target stays where the move put it (the role=target
// override), and the snooze is reported as snoozed_until.

const (
	t4590Move   = "2026-10-30T08:00:00Z" // when the move is made
	t4590D1     = "2026-10-31T10:00:00Z" // the moved target
	t4590Snooze = "2026-10-31T09:45:00Z" // when the snooze is made, before D1
	t4590S      = "2026-10-31T12:00:00Z" // the snooze re-fire instant, after D1
	t4590Read   = "2026-10-31T09:50:00Z" // between the snooze and D1
)

func t4590Create(floating bool) models.ScheduleCreate {
	in := models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "UTC", LeadMinutes: 30}
	if floating {
		in.Floating, in.Tz = true, ""
	}
	return in
}

func t4590Occurrence(t *testing.T, sc *models.ItemSchedule) time.Time {
	t.Helper()
	at, ok, err := OutstandingInstant(sc, time.UTC)
	if err != nil || !ok {
		t.Fatalf("outstanding instant: %v %v", ok, err)
	}
	return at
}

// t4590MoveSnooze moves the outstanding occurrence to D1, then snoozes it to S.
func t4590MoveSnooze(t *testing.T, f *schedFixture, item *models.Item, sc *models.ItemSchedule) *ResolveResult {
	t.Helper()
	occ := t4590Occurrence(t, sc)
	f.mustResolve(t, item, ResolveInput{OccurrenceAt: occ, State: "move", Until: ptr(rt(t4590D1)), Actor: "user"}, t4590Move)
	return f.mustResolve(t, item, ResolveInput{OccurrenceAt: occ, State: "snooze", Until: ptr(rt(t4590S)), Actor: "user"}, t4590Snooze)
}

func t4590Outstanding(t *testing.T, f *schedFixture, item *models.Item, now string) AgendaEntry {
	t.Helper()
	var out []AgendaEntry
	for _, e := range f.expand(t, item, time.UTC, "2026-10-29T00:00:00Z", "2026-11-03T00:00:00Z", now) {
		if e.State == "outstanding" {
			out = append(out, e)
		}
	}
	if len(out) != 1 {
		t.Fatalf("outstanding entries = %+v, want 1", out)
	}
	return out[0]
}

func TestTASK4590MoveThenSnoozeKeepsTarget(t *testing.T) {
	for _, floating := range []bool{false, true} {
		name := "pinned"
		if floating {
			name = "floating"
		}
		t.Run(name, func(t *testing.T) {
			f := newSchedFixture(t)
			item, sc := f.mk(t, t4590Create(floating), "2026-10-29T00:00:00Z")
			occ := t4590Occurrence(t, sc)
			f.mustResolve(t, item, ResolveInput{OccurrenceAt: occ, State: "move", Until: ptr(rt(t4590D1)), Actor: "user"}, t4590Move)
			before, err := f.s.scheduleOverridesQ(f.s.db, f.ws.ID, sc.ID)
			if err != nil {
				t.Fatal(err)
			}
			f.mustResolve(t, item, ResolveInput{OccurrenceAt: occ, State: "snooze", Until: ptr(rt(t4590S)), Actor: "user"}, t4590Snooze)
			after, err := f.s.scheduleOverridesQ(f.s.db, f.ws.ID, sc.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("snooze changed the override rows: %v\nbefore %+v\nafter %+v", err, before, after)
			}
			e := t4590Outstanding(t, f, item, t4590Read)
			d1, s := rt(t4590D1), rt(t4590S)
			if !e.At.Equal(d1) {
				t.Errorf("at = %s, want the moved target %s", e.At, d1)
			}
			if e.EffectiveAt == nil || !e.EffectiveAt.Equal(d1) {
				t.Errorf("effective_at = %v, want the moved target %s", e.EffectiveAt, d1)
			}
			if e.SnoozedUntil == nil || !e.SnoozedUntil.Equal(s) {
				t.Errorf("snoozed_until = %v, want %s", e.SnoozedUntil, s)
			}
			assertMoveArm(t, f, sc, t4590S)
			view, err := f.s.GetCommitment(f.ws.ID, item.ID)
			if err != nil {
				t.Fatal(err)
			}
			if view.Roles.Target == nil || !rt(*view.Roles.Target).Equal(d1) {
				t.Errorf("commitment target = %v, want %s", view.Roles.Target, d1)
			}
		})
	}
}

func TestTASK4590MovedSnoozeReadPaths(t *testing.T) {
	f := newBootFixture(t)
	item, sc := f.mkFor(t, f.me.ID, t4590Create(false), "2026-10-29T00:00:00Z")
	t4590MoveSnooze(t, f.schedFixture, item, sc)
	s, now := rt(t4590S), rt(t4590Read)
	cur := f.reload(t, item.ID)

	e, err := f.s.AgendaOutstanding(cur, time.UTC, now)
	if err != nil || e == nil {
		t.Fatalf("AgendaOutstanding: %v %v", e, err)
	}
	if e.SnoozedUntil == nil || !e.SnoozedUntil.Equal(s) {
		t.Errorf("AgendaOutstanding snoozed_until = %v, want %s", e.SnoozedUntil, s)
	}
	got, err := f.s.ReadScheduleSnoozedUntil(cur, now)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || !got.Equal(s) {
		t.Errorf("ReadScheduleSnoozedUntil = %v, want %s", got, s)
	}
	set := f.read(t, false, time.UTC, t4590Read)
	var rows []BootstrapAgendaRow
	for _, list := range [][]BootstrapAgendaRow{set.DueToday, set.Carried, set.Stale} {
		rows = append(rows, list...)
	}
	if len(rows) != 1 {
		t.Fatalf("bootstrap rows = %+v, want 1", rows)
	}
	if rows[0].SnoozedUntil == nil || !rows[0].SnoozedUntil.Equal(s) || !rows[0].Due.Equal(rt(t4590D1)) {
		t.Errorf("bootstrap row snoozed_until = %v due = %s, want %s and %s", rows[0].SnoozedUntil, rows[0].Due, s, t4590D1)
	}
}

func TestTASK4590MovedSnoozeNeedsDue(t *testing.T) {
	between := rt("2026-10-31T11:00:00Z") // past D1, before S
	t.Run("move and snooze", func(t *testing.T) {
		f := newBootFixture(t)
		item, sc := f.mkFor(t, f.me.ID, t4590Create(false), "2026-10-29T00:00:00Z")
		t4590MoveSnooze(t, f.schedFixture, item, sc)
		got, err := f.s.NeedsDueOccurrences(f.ws.ID, CatchUpScope{}, f.me.ID, between)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := got[item.ID]; ok {
			t.Fatalf("a snoozed moved occurrence claims before the snooze ends: %v", got)
		}
	})
	t.Run("move alone", func(t *testing.T) {
		f := newBootFixture(t)
		item, sc := f.mkFor(t, f.me.ID, t4590Create(false), "2026-10-29T00:00:00Z")
		f.mustResolve(t, item, ResolveInput{OccurrenceAt: t4590Occurrence(t, sc), State: "move", Until: ptr(rt(t4590D1)), Actor: "user"}, t4590Move)
		got, err := f.s.NeedsDueOccurrences(f.ws.ID, CatchUpScope{}, f.me.ID, between)
		if err != nil {
			t.Fatal(err)
		}
		if d, ok := got[item.ID]; !ok || !d.Earliest.Equal(rt(t4590D1)) {
			t.Fatalf("a moved occurrence past D1 = %v, want claimed at %s", got, t4590D1)
		}
	})
}

func TestTASK4590SnoozeThenMoveLandsOnTarget(t *testing.T) {
	f := newSchedFixture(t)
	item, sc := f.mk(t, t4590Create(false), "2026-10-29T00:00:00Z")
	occ := t4590Occurrence(t, sc)
	f.mustResolve(t, item, ResolveInput{OccurrenceAt: occ, State: "snooze", Until: ptr(rt("2026-10-30T11:00:00Z")), Actor: "user"}, "2026-10-30T09:30:00Z")
	f.mustResolve(t, item, ResolveInput{OccurrenceAt: occ, State: "move", Until: ptr(rt(t4590D1)), Actor: "user"}, "2026-10-30T10:00:00Z")
	e := t4590Outstanding(t, f, item, "2026-10-30T10:05:00Z")
	d1 := rt(t4590D1)
	if !e.At.Equal(d1) || e.EffectiveAt == nil || !e.EffectiveAt.Equal(d1) {
		t.Errorf("at = %s effective_at = %v, want %s", e.At, e.EffectiveAt, d1)
	}
	if e.SnoozedUntil != nil {
		t.Errorf("a move after a snooze is not snoozed, got %s", e.SnoozedUntil)
	}
	assertMoveArm(t, f, sc, "2026-10-31T09:30:00Z")
}

func TestTASK4590SnoozedUntilMovedUnit(t *testing.T) {
	wall, override := "2026-10-30T09:00", "2026-10-31T12:30:00Z"
	sc := &models.ItemSchedule{NextWall: &wall, NextOverrideAt: &override, LeadMinutes: 30}
	now := rt("2026-10-31T09:50:00Z")
	same := rt(override)
	if got := ScheduleSnoozedUntil(sc, "moved", &same, now); got != nil {
		t.Errorf("target equal to the override is a plain move, got %s", got)
	}
	d1 := rt(t4590D1)
	if got := ScheduleSnoozedUntil(sc, "moved", &d1, now); got == nil || !got.Equal(rt(t4590S)) {
		t.Errorf("target apart from the override = %v, want %s", got, t4590S)
	}
	if got := ScheduleSnoozedUntil(sc, "moved", nil, now); got != nil {
		t.Errorf("moved with no target row, got %s", got)
	}
	if got := ScheduleSnoozedUntil(sc, "done", &d1, now); got != nil {
		t.Errorf("a resolved occurrence is not snoozed, got %s", got)
	}
}

func TestTASK4590UndoSnoozeOnMovedOccurrence(t *testing.T) {
	f := newSchedFixture(t)
	item, sc := f.mk(t, t4590Create(false), "2026-10-29T00:00:00Z")
	occ := t4590Occurrence(t, sc)
	f.mustResolve(t, item, ResolveInput{OccurrenceAt: occ, State: "move", Until: ptr(rt(t4590D1)), Actor: "user"}, t4590Move)
	overrides, err := f.s.scheduleOverridesQ(f.s.db, f.ws.ID, sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	res := f.resolveForReopenTest(t, item, ResolveInput{OccurrenceAt: occ, State: "snooze", Until: ptr(rt(t4590S)), Actor: "user"}, t4590Snooze)
	if res.ScheduleLastResolution == nil || res.ResolutionID == "" {
		t.Fatal("snooze has no resolution id")
	}
	if _, err := f.s.reopenOccurrenceAt(f.ws.ID, item.ID, ReopenInput{ResolutionID: res.ResolutionID, Actor: "user"}, rt(t4590Read)); err != nil {
		t.Fatal(err)
	}
	cur := f.reload(t, item.ID)
	if cur.NextOverrideAt == nil || !rt(*cur.NextOverrideAt).Equal(rt(t4590D1)) {
		t.Errorf("next_override_at = %v, want %s", cur.NextOverrideAt, t4590D1)
	}
	if got, err := f.s.ReadScheduleSnoozedUntil(cur, rt(t4590Read)); err != nil || got != nil {
		t.Errorf("snoozed_until after undo = %v %v, want nil", got, err)
	}
	assertMoveArm(t, f, sc, "2026-10-31T09:30:00Z")
	if n := f.occRows(t, sc.ID, "moved"); n != 1 {
		t.Errorf("moved rows = %d, want 1", n)
	}
	after, err := f.s.scheduleOverridesQ(f.s.db, f.ws.ID, sc.ID)
	if err != nil || !reflect.DeepEqual(overrides, after) {
		t.Errorf("undo changed the override rows: %v\nbefore %+v\nafter %+v", err, overrides, after)
	}
}
