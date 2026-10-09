package cli

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func act(id, action, batch string) models.Activity {
	meta := "{}"
	if batch != "" {
		meta = `{"reorder_batch":"` + batch + `","sort_order_from":"1","sort_order_to":"2"}`
	}
	return models.Activity{ID: id, Action: action, Metadata: meta}
}

// TASK-3517: `pad project activity` text output shows one line per reorder
// batch; runs never merge across another row or another batch.
func TestTASK3517_CollapseReorderBatches(t *testing.T) {
	got := CollapseReorderBatches([]models.Activity{
		act("a", "reordered", "b1"), act("b", "reordered", "b1"), act("c", "reordered", "b1"),
		act("x", "updated", ""),
		act("d", "reordered", "b1"),
		act("e", "reordered", "b2"), act("f", "reordered", "b2"),
	})
	want := []struct {
		id    string
		count int
	}{{"a", 3}, {"x", 0}, {"d", 0}, {"e", 2}}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].ID != w.id || got[i].ReorderCount != w.count {
			t.Errorf("line %d = %s/%d, want %s/%d", i, got[i].ID, got[i].ReorderCount, w.id, w.count)
		}
	}
}
