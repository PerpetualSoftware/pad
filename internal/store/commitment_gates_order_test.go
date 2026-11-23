package store

import (
	"fmt"
	"sort"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// An item's gates, both the ones holding it and the ones it releases, come out
// in creation order, not in the order of their random ids (TASK-4705).

// creationStamps are valid RFC3339 stamps in time order whose text order is the
// reverse: the server's RFC3339Nano drops trailing zeros (so "00Z" sorts after
// "00.1Z"), and a bundle import keeps a stamp's own offset.
var creationStamps = []string{
	"2026-01-01T00:00:00Z",
	"2026-01-01T00:00:00.1Z",
	"2025-12-31T21:00:01-03:00",
}

// stampReverseIDOrder rewrites the gates' created_at so creation order is the
// reverse of id order and of text order, and returns the ids in the creation
// order it set.
func stampReverseIDOrder(t *testing.T, s *Store, gates []*models.ScheduleGate) []string {
	t.Helper()
	if len(gates) != len(creationStamps) {
		t.Fatalf("%d gates, want %d", len(gates), len(creationStamps))
	}
	ids := make([]string, len(gates))
	for i, g := range gates {
		ids[i] = g.ID
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	for i, id := range ids {
		at := creationStamps[i]
		if _, err := s.db.Exec(s.q(`UPDATE schedule_gates SET created_at=? WHERE id=?`), at, id); err != nil {
			t.Fatalf("stamp gate %s: %v", id, err)
		}
	}
	return ids
}

func gateIDs(gates []models.ScheduleGate) []string {
	out := make([]string, len(gates))
	for i, g := range gates {
		out[i] = g.ID
	}
	return out
}

func TestScheduleGatesComeOutInCreationOrder(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 4) // items 1, 2 and 3 each hold item 0
	var gates []*models.ScheduleGate
	for _, b := range []int{1, 2, 3} {
		gates = append(gates, f.gate(t, f.items[0].ID, f.items[b].ID, f.allEdit()))
	}
	want := stampReverseIDOrder(t, f.s, gates)
	view, err := f.s.ScheduleGates(f.ws.ID, f.items[0].ID, f.allEdit())
	if err != nil {
		t.Fatalf("ScheduleGates: %v", err)
	}
	if got := gateIDs(view.Gates); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("gates in order %v, want creation order %v", got, want)
	}
}

func TestScheduleGateDependentsComeOutInCreationOrder(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 4) // item 0 holds items 1, 2 and 3
	var gates []*models.ScheduleGate
	for _, b := range []int{1, 2, 3} {
		gates = append(gates, f.gate(t, f.items[b].ID, f.items[0].ID, f.allEdit()))
	}
	want := stampReverseIDOrder(t, f.s, gates)
	if got := gateIDs(f.dependents(t, 0, f.allEdit())); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("dependents in order %v, want creation order %v", got, want)
	}
}
