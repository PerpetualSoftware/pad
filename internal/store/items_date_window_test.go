package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4104 D1: the date-window clause of ListItems. SQLite in make test,
// Postgres in make test-pg (the one dialect-sensitive piece of the agenda).

func TestListItemsFieldDateWindow(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Dated")
	col, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Schema: `{"fields":[` +
		`{"key":"status","type":"select","options":["open","done"],"default":"open"},` +
		`{"key":"due_date","type":"date"},{"key":"end_date","type":"date"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(title, fields string) {
		if _, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: title, Fields: fields}); err != nil {
			t.Fatalf("CreateItem(%s): %v", title, err)
		}
	}
	mk("day first", `{"status":"open","due_date":"2026-11-05"}`)
	mk("offset shape", `{"status":"open","due_date":"2026-11-06T23:30:00-08:00"}`)
	mk("end key z", `{"status":"open","end_date":"2026-11-06T01:00:00Z"}`)
	mk("before", `{"status":"open","due_date":"2026-11-04"}`)
	mk("after", `{"status":"open","due_date":"2026-11-07"}`)
	mk("done", `{"status":"done","due_date":"2026-11-05"}`)
	mk("no date", `{"status":"open"}`)

	list := func(scope []string) []string {
		items, err := s.ListItems(ws.ID, models.ItemListParams{
			FieldDateWindow: &models.FieldDateWindow{Keys: []string{"due_date", "end_date"}, From: "2026-11-05", To: "2026-11-06"},
			NonTerminal:     true, NoContent: true, CollectionIDs: scope,
		})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, it := range items {
			out = append(out, it.Title)
		}
		return out
	}
	got := list(nil)
	want := map[string]bool{"day first": true, "offset shape": true, "end key z": true}
	if len(got) != len(want) {
		t.Fatalf("window 2026-11-05..2026-11-06 = %v, want exactly %v (inclusive bounds, both shapes, both keys, done and undated out)", got, want)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected %q in %v", g, got)
		}
	}
	if got := list([]string{}); len(got) != 0 {
		t.Errorf("an empty scope sees nothing, got %v", got)
	}
}
