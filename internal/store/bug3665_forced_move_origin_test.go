package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3665: a move with no precheck (a forced move, or a direct MoveItem)
// recorded its origin from the item as read BEFORE the lock. When another
// writer moved the item first, the record named the collection the item had
// already left: A->C instead of B->C. A client that cached the item in B,
// and cannot see A or C, then never got the moved-out tombstone.
func TestBUG3665_MoveRecordsTheOriginUnderTheLock(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Forced move")
	schema := `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`
	coll := func(name string) *models.Collection {
		c, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: name, Schema: schema})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	a, b, c := coll("A"), coll("B"), coll("C")
	item, err := s.CreateItem(ws.ID, a.ID, models.ItemCreate{Title: "Mover", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatal(err)
	}

	// Between the forced move's pre-lock read (item in A) and its lock,
	// another writer moves the item to B.
	s.beforeMoveLock = func(id string) {
		s.beforeMoveLock = nil
		if _, err := s.MoveItem(id, b.ID, `{"status":"open"}`); err != nil {
			t.Errorf("the concurrent move: %v", err)
		}
	}
	if _, err := s.MoveItem(item.ID, c.ID, `{"status":"open"}`); err != nil {
		t.Fatal(err)
	}
	if s.beforeMoveLock != nil {
		t.Fatal("instrument: the concurrent move never ran")
	}

	rows, err := s.db.Query(s.q(`SELECT from_collection_id, to_collection_id FROM item_collection_moves WHERE item_id = ? ORDER BY seq`), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	name := map[string]string{a.ID: "A", b.ID: "B", c.ID: "C"}
	var got []string
	for rows.Next() {
		var from, to string
		if err := rows.Scan(&from, &to); err != nil {
			t.Fatal(err)
		}
		got = append(got, name[from]+"->"+name[to])
	}
	if len(got) != 2 || got[0] != "A->B" || got[1] != "B->C" {
		t.Errorf("move records %v, want [A->B B->C]", got)
	}
}
