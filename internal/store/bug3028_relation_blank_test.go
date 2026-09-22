package store_test

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3028 on BOTH backends: the empty relation filter and the in-lock
// normalisation of carried blanks are SQL/JSON the server tests only ever run
// on SQLite.

func blankRelFixture(t *testing.T, s *store.Store) (*models.Workspace, *models.Collection) {
	t.Helper()
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Blank", Slug: "blank"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if _, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "People", Slug: "people", Prefix: "PEOP", Schema: `{"fields":[]}`}); err != nil {
		t.Fatalf("CreateCollection(people): %v", err)
	}
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Owned", Slug: "owned", Prefix: "OWN",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"]},{"key":"helper","type":"relation","collection":"people"}]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection(owned): %v", err)
	}
	return ws, coll
}

func TestEmptyRelationFilterMatchesEverySpelling(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			ws, coll := blankRelFixture(t, s)
			var want []string
			for i, fields := range []string{`{"status":"open"}`, `{"helper":""}`, `{"helper":"   "}`, `{"helper":"not-blank"}`} {
				it, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "i" + string(rune('a'+i)), Fields: fields})
				if err != nil {
					t.Fatal(err)
				}
				if i < 3 {
					want = append(want, it.ID)
				}
			}
			got, err := s.ListItems(ws.ID, models.ItemListParams{CollectionSlug: coll.Slug, Fields: map[string]string{"helper": ""}})
			if err != nil {
				t.Fatalf("ListItems: %v", err)
			}
			var ids []string
			for _, it := range got {
				ids = append(ids, it.ID)
			}
			sort.Strings(ids)
			sort.Strings(want)
			if strings.Join(ids, ",") != strings.Join(want, ",") {
				t.Fatalf("helper= matched %v, want the three no-target rows %v", ids, want)
			}
		})
	}
}

func TestFieldsPatchNormalisesCarriedBlankUnderTheLock(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			ws, coll := blankRelFixture(t, s)
			it, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "legacy", Fields: `{"status":"open","helper":"  "}`})
			if err != nil {
				t.Fatal(err)
			}
			updated, err := s.UpdateItem(it.ID, models.ItemUpdate{
				FieldsPatch:       map[string]any{"status": "done"},
				BlankRelationKeys: []string{"helper"},
			})
			if err != nil {
				t.Fatalf("UpdateItem: %v", err)
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(updated.Fields), &m); err != nil {
				t.Fatal(err)
			}
			if _, ok := m["helper"]; ok || m["status"] != "done" {
				t.Fatalf("fields = %v, want status=done and helper absent", m)
			}
		})
	}
}
