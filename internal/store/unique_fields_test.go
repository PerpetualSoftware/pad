package store

import (
	"encoding/json"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2367. The not_unique report names the item holding the value ONLY when
// the caller may see it: the drop already says the value is taken, but the
// holder's ref would disclose an item behind a grant the caller lacks.
func TestDropCarriedUniqueCollisions_HolderNamedOnlyWhenVisible(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Unique WS")
	schema := `{"fields":[{"key":"invocation_slug","label":"Slug","type":"text","unique_scope":"workspace_collection"},{"key":"code","label":"Code","type":"text"}]}`
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Runbooks", Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	holder, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "Holder", Fields: `{"invocation_slug":"day"}`})
	if err != nil {
		t.Fatal(err)
	}
	holder, err = s.GetItem(holder.ID)
	if err != nil || holder.Ref == "" {
		t.Fatalf("holder has no ref to name (err %v)", err)
	}
	var defs models.CollectionSchema
	if err := json.Unmarshal([]byte(coll.Schema), &defs); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		canSee  RelationVisibilityFunc
		holder  string
		message string
	}{
		{"visible holder", func(Queryer, string, *models.Item) (bool, error) { return true, nil },
			holder.Ref, `invocation_slug "day" is taken by ` + holder.Ref},
		{"hidden holder", func(Queryer, string, *models.Item) (bool, error) { return false, nil },
			"", `invocation_slug "day" is already used by another item in Runbooks`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fields := map[string]any{"invocation_slug": "day", "code": "x"}
			drops, err := s.DropCarriedUniqueCollisionsQ(s.Q(), tc.canSee, ws.ID, coll, defs.Fields, fields, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(drops) != 1 {
				t.Fatalf("want one drop, got %+v", drops)
			}
			d := drops[0]
			if d.Key != "invocation_slug" || d.Value != "day" || d.Holder != tc.holder || d.Message != tc.message {
				t.Fatalf("drop = %+v, want holder %q message %q", d, tc.holder, tc.message)
			}
			if _, still := fields["invocation_slug"]; still {
				t.Fatal("the colliding carried value was reported dropped but left in the map")
			}
			if fields["code"] != "x" {
				t.Fatal("a field with no unique_scope was touched")
			}
		})
	}

	t.Run("a supplied value is left for the final check", func(t *testing.T) {
		fields := map[string]any{"invocation_slug": "day"}
		drops, err := s.DropCarriedUniqueCollisionsQ(s.Q(), nil, ws.ID, coll, defs.Fields, fields,
			func(k string) bool { return k == "invocation_slug" })
		if err != nil {
			t.Fatal(err)
		}
		if len(drops) != 0 || fields["invocation_slug"] != "day" {
			t.Fatalf("a supplied value must not be dropped: drops %+v fields %v", drops, fields)
		}
		keys, err := s.UniqueFieldConflictsQ(s.Q(), coll.ID, "", defs.Fields, fields)
		if err != nil || len(keys) != 1 || keys[0] != "invocation_slug" {
			t.Fatalf("the final check must refuse it: keys %v err %v", keys, err)
		}
	})

	t.Run("an item never conflicts with itself", func(t *testing.T) {
		keys, err := s.UniqueFieldConflictsQ(s.Q(), coll.ID, holder.ID, defs.Fields, map[string]any{"invocation_slug": "day"})
		if err != nil || len(keys) != 0 {
			t.Fatalf("keys %v err %v", keys, err)
		}
	})
}
