package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// An item's dependents are the gates it blocks: the items its success releases
// (TASK-4611). They are display only, and name only dependents the reader can
// read.

func (f *linkFix) dependents(t *testing.T, i int, acc models.CommitmentAccess) []models.ScheduleGate {
	t.Helper()
	d, err := f.s.ScheduleGateDependents(f.ws.ID, f.items[i].ID, acc)
	if err != nil {
		t.Fatalf("ScheduleGateDependents: %v", err)
	}
	if d == nil {
		t.Fatal("ScheduleGateDependents returned a nil slice")
	}
	return d
}

func TestScheduleGateDependentsListsTheItemsABlockerReleases(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 3) // item 2 blocks items 0 and 1
	g0 := f.gate(t, f.items[0].ID, f.items[2].ID, f.allEdit())
	g1 := f.gate(t, f.items[1].ID, f.items[2].ID, f.allEdit())
	want := map[string]string{g0.ID: f.items[0].ID, g1.ID: f.items[1].ID}
	check := func(what, outcome string, opened bool) {
		t.Helper()
		d := f.dependents(t, 2, f.allEdit())
		if len(d) != 2 {
			t.Fatalf("%s: %d dependents, want 2: %+v", what, len(d), d)
		}
		for _, g := range d {
			if want[g.ID] != g.BlockedItemID || g.BlockerItemID != f.items[2].ID || g.Outcome != outcome || (g.OpenedAt != nil) != opened || g.CreatedAt == "" {
				t.Fatalf("%s: dependent %+v, want outcome %s opened=%v", what, g, outcome, opened)
			}
		}
	}
	check("blocker pending", models.GateOutcomePending, false)
	if d := f.dependents(t, 0, f.allEdit()); len(d) != 0 {
		t.Fatalf("a gated item that blocks nothing has dependents: %+v", d)
	}
	f.resolve(t, 2, "done")
	check("blocker done", models.GateOutcomeSucceeded, true)
}

func TestScheduleGateDependentsHidesAnUnreadableDependent(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 3) // item 2 blocks item 1, which the reader cannot read
	g := f.gate(t, f.items[1].ID, f.items[2].ID, f.allEdit())
	reader := f.withoutRead(f.items[1])
	hidden := f.dependents(t, 2, reader)
	none := f.dependents(t, 0, reader) // item 0 blocks nothing
	hb, _ := json.Marshal(hidden)
	nb, _ := json.Marshal(none)
	if string(hb) != string(nb) {
		t.Fatalf("a hidden dependent reads %s, an item with none reads %s", hb, nb)
	}
	for _, id := range []string{f.items[1].ID, g.ID} {
		if strings.Contains(string(hb), id) {
			t.Fatalf("the dependents name %s: %s", id, hb)
		}
	}
	if d := f.dependents(t, 2, f.allEdit()); len(d) != 1 || d[0].ID != g.ID {
		t.Fatalf("a reader of both ends: %+v", d)
	}
}

func TestScheduleGateDependentsOfAnUnreadableItemIsNotFound(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.gate(t, f.items[0].ID, f.items[1].ID, f.allEdit())
	if _, err := f.s.ScheduleGateDependents(f.ws.ID, f.items[1].ID, f.withoutRead(f.items[1])); !errors.Is(err, ErrScheduleNotFound) {
		t.Fatalf("err=%v, want ErrScheduleNotFound", err)
	}
}
