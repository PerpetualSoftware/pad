package store

import (
	"testing"
)

// BUG-3253: AppendSyncFrames persists a run of relay frames in one
// transaction. Each frame must get exactly what AppendSyncFrame alone gives
// it, in order, including the duplicate check seeing the batch's OWN earlier
// rows; and a failure stores nothing.
func TestBUG3253_AppendSyncFramesMatchesOneByOne(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	one := createTestItem(t, s, ws.ID, col.ID, "One by one", "")
	batched := createTestItem(t, s, ws.ID, col.ID, "Batched", "")

	a, b, c := []byte{0, 2, 10, 1}, []byte{0, 2, 10, 2}, []byte{0, 2, 10, 3}
	frames := [][]byte{a, b, a, c} // the third is a byte-identical re-send of the first

	var want []SyncFrameAppend
	for _, f := range frames {
		r, err := s.AppendSyncFrame(one.ID, f, "1")
		if err != nil {
			t.Fatalf("AppendSyncFrame: %v", err)
		}
		want = append(want, r)
	}
	got, err := s.AppendSyncFrames(batched.ID, frames, "1")
	if err != nil {
		t.Fatalf("AppendSyncFrames: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d results, want %d", len(got), len(want))
	}
	// Ids differ between the items, so compare each id's offset from the
	// item's first row, and the Persisted flags exactly.
	for i := range want {
		if got[i].Persisted != want[i].Persisted {
			t.Errorf("frame %d: Persisted %v, one by one %v", i, got[i].Persisted, want[i].Persisted)
		}
		if got[i].ID-got[0].ID != want[i].ID-want[0].ID {
			t.Errorf("frame %d: id offset %d, one by one %d", i, got[i].ID-got[0].ID, want[i].ID-want[0].ID)
		}
	}
	if got[2].Persisted || got[2].ID != got[1].ID {
		t.Errorf("the in-batch re-send = %+v, want skipped and acknowledged at the batch's max id %d", got[2], got[1].ID)
	}
	rows, err := s.LoadYjsUpdatesSince(batched.ID, 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("batched item holds %d rows, want 3", len(rows))
	}
}

func TestBUG3253_AppendSyncFramesIsAllOrNothing(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Item", "")

	if _, err := s.AppendSyncFrames(item.ID, [][]byte{{0, 2, 1}, {}, {0, 2, 3}}, "1"); err == nil {
		t.Fatal("a batch with an empty frame was accepted")
	}
	rows, err := s.LoadYjsUpdatesSince(item.ID, 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("a failed batch left %d rows behind", len(rows))
	}
}
