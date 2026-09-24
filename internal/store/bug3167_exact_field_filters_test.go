package store

import (
	"sort"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3167: a Fields value is matched EXACTLY, comma or not. It used to be
// split into an OR for every caller, so a stored "x,y" could never be matched
// as itself and a lookup for "x,y" also returned items holding "x" or "y". The
// OR now has to be asked for by name, through FieldsAnyOf. Both list paths are
// covered: the plain one and the FTS one (a non-empty Search), which carried
// their own copy of the split.
func TestBUG3167_FieldsMatchExactlyAndAnyOfIsExplicit(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "BUG-3167")
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Commas",
		Schema: `{"fields":[{"key":"status","type":"select","options":["x","y","x,y"],"default":"x"}]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	mk := func(title, status string) {
		if _, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: title, Content: "needle", Fields: `{"status":"` + status + `"}`}); err != nil {
			t.Fatalf("CreateItem %s: %v", title, err)
		}
	}
	mk("holds x", "x")
	mk("holds y", "y")
	mk("holds x,y", "x,y")

	titles := func(p models.ItemListParams) []string {
		t.Helper()
		items, err := s.ListItems(ws.ID, p)
		if err != nil {
			t.Fatalf("ListItems: %v", err)
		}
		var out []string
		for _, it := range items {
			out = append(out, it.Title)
		}
		sort.Strings(out)
		return out
	}
	for _, search := range []string{"", "needle"} {
		name := "plain"
		if search != "" {
			name = "fts"
		}
		exact := titles(models.ItemListParams{Search: search, Fields: map[string]string{"status": "x,y"}})
		if len(exact) != 1 || exact[0] != "holds x,y" {
			t.Errorf("%s: Fields{status: x,y} = %v, want only the literal x,y item", name, exact)
		}
		any := titles(models.ItemListParams{Search: search, FieldsAnyOf: map[string][]string{"status": {"x", "y"}}})
		if len(any) != 2 || any[0] != "holds x" || any[1] != "holds y" {
			t.Errorf("%s: FieldsAnyOf{status: [x y]} = %v, want the x and y items", name, any)
		}
		plain := titles(models.ItemListParams{Search: search, Fields: map[string]string{"status": "x"}})
		if len(plain) != 1 || plain[0] != "holds x" {
			t.Errorf("%s: control Fields{status: x} = %v, want only the x item", name, plain)
		}
	}
}

// An invalid field key is skipped in FieldsAnyOf exactly as it is in Fields:
// the key is interpolated into a JSON path, so isValidFieldKey is what keeps
// a query-parameter name out of the SQL. The payload is chosen so that the
// INJECTED query returns nothing (`AND 1=0`), which a skipped filter cannot:
// an `OR 1=1` payload returns everything, the same answer the guard gives, and
// a first draft of this test using it survived the guard's removal.
func TestBUG3167_FieldsAnyOfSkipsAnInvalidKey(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "BUG-3167 keys")
	coll := createTestCollection(t, s, ws.ID, "Tasks")
	createTestItem(t, s, ws.ID, coll.ID, "one", "")
	createTestItem(t, s, ws.ID, coll.ID, "two", "")
	items, err := s.ListItems(ws.ID, models.ItemListParams{
		FieldsAnyOf: map[string][]string{"status') AND 1=0 --": {"x"}},
	})
	if err != nil {
		t.Fatalf("ListItems with an invalid AnyOf key: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("invalid AnyOf key: got %d items, want the unfiltered 2 (the key must be skipped, not interpolated)", len(items))
	}
}
