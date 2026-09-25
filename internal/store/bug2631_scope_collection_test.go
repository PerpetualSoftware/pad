package store_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-2631: ScopeCollectionID restricts a list to one collection BY ID and is
// ANDed on its own. It must never behave like the CollectionIDs/ItemIDs
// permission pair (which is OR-ed), in either direction: it must not widen a
// grant, and a grant must not reach past it.

type scopeFixture struct {
	ws         *models.Workspace
	a, b       *models.Collection
	a1, a2, b1 *models.Item
}

func newScopeFixture(t *testing.T, s *store.Store) scopeFixture {
	t.Helper()
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Scope", Slug: "scope"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	mk := func(name, slug, prefix string) *models.Collection {
		c, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: name, Slug: slug, Prefix: prefix, Schema: `{"fields":[]}`})
		if err != nil {
			t.Fatalf("CreateCollection(%s): %v", slug, err)
		}
		return c
	}
	f := scopeFixture{ws: ws, a: mk("Alpha", "alpha", "ALP"), b: mk("Beta", "beta", "BET")}
	item := func(c *models.Collection, title string) *models.Item {
		it, err := s.CreateItem(ws.ID, c.ID, models.ItemCreate{Title: title, Fields: `{}`})
		if err != nil {
			t.Fatalf("CreateItem(%s): %v", title, err)
		}
		return it
	}
	// A shared word in every title so the FTS path has something to match
	// in both collections.
	f.a1, f.a2, f.b1 = item(f.a, "shared alpha one"), item(f.a, "shared alpha two"), item(f.b, "shared beta one")
	return f
}

func idsOf(items []models.Item) string {
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

func wantIDs(items ...*models.Item) string {
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

func TestScopeCollectionIDIsAScopeNotAPermission(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			f := newScopeFixture(t, s)

			cases := []struct {
				name   string
				params models.ItemListParams
				want   string
			}{
				{"scope alone", models.ItemListParams{ScopeCollectionID: f.a.ID}, wantIDs(f.a1, f.a2)},
				// A grant on an item in ANOTHER collection must not reach past
				// the scope. Were the scope folded into the permission pair,
				// this would read `collection_id IN (a) OR id IN (b1)`.
				{"grant outside the scope", models.ItemListParams{
					ScopeCollectionID: f.a.ID, CollectionIDs: []string{f.a.ID}, ItemIDs: []string{f.b1.ID},
				}, wantIDs(f.a1, f.a2)},
				// The scope must not WIDEN a grant: a caller whose only claim
				// on Alpha is one item grant sees that item, not its sibling.
				{"item grant inside the scope", models.ItemListParams{
					ScopeCollectionID: f.a.ID, CollectionIDs: []string{f.b.ID}, ItemIDs: []string{f.a1.ID},
				}, wantIDs(f.a1)},
				{"FTS path", models.ItemListParams{ScopeCollectionID: f.a.ID, Search: "shared"}, wantIDs(f.a1, f.a2)},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					got, err := s.ListItems(f.ws.ID, tc.params)
					if err != nil {
						t.Fatalf("ListItems: %v", err)
					}
					if idsOf(got) != tc.want {
						t.Errorf("got %s, want %s", idsOf(got), tc.want)
					}
				})
			}

			t.Run("index", func(t *testing.T) {
				got, err := s.ListItemsIndex(f.ws.ID, store.ItemIndexParams{ScopeCollectionID: f.a.ID})
				if err != nil {
					t.Fatalf("ListItemsIndex: %v", err)
				}
				if idsOf(got) != wantIDs(f.a1, f.a2) {
					t.Errorf("got %s, want %s", idsOf(got), wantIDs(f.a1, f.a2))
				}
			})
		})
	}
}

// The race itself, made deterministic by doing its two halves in order: a
// caller resolves Alpha, Alpha is renamed away from its slug, and another
// collection takes that slug. The scoped query still answers for the
// collection that was resolved; a slug filter answers for the newcomer.
func TestScopeCollectionIDSurvivesSlugReuse(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			f := newScopeFixture(t, s)
			resolved := f.a // what a handler's gate checked

			// A rename re-slugifies, which is what frees "alpha".
			newName := "Alpha Renamed"
			renamed, err := s.UpdateCollection(resolved.ID, models.CollectionUpdate{Name: &newName})
			if err != nil {
				t.Fatalf("rename alpha: %v", err)
			}
			if renamed.Slug == resolved.Slug {
				t.Fatalf("rename kept the slug %q; the fixture cannot free it", renamed.Slug)
			}
			squatter, err := s.CreateCollection(f.ws.ID, models.CollectionCreate{Name: "Squatter", Slug: "alpha", Prefix: "SQT", Schema: `{"fields":[]}`})
			if err != nil {
				t.Fatalf("create squatter on the freed slug: %v", err)
			}
			squatterItem, err := s.CreateItem(f.ws.ID, squatter.ID, models.ItemCreate{Title: "not yours", Fields: `{}`})
			if err != nil {
				t.Fatalf("CreateItem squatter: %v", err)
			}

			got, err := s.ListItems(f.ws.ID, models.ItemListParams{ScopeCollectionID: resolved.ID})
			if err != nil {
				t.Fatalf("ListItems: %v", err)
			}
			if idsOf(got) != wantIDs(f.a1, f.a2) {
				t.Errorf("scoped by the resolved ID: got %s, want %s", idsOf(got), wantIDs(f.a1, f.a2))
			}
			// CONTROL: the slug the gate saw now names the squatter. This is
			// the answer every BUG-2631 site used to give.
			bySlug, err := s.ListItems(f.ws.ID, models.ItemListParams{CollectionSlug: resolved.Slug})
			if err != nil {
				t.Fatalf("ListItems by slug: %v", err)
			}
			if idsOf(bySlug) != wantIDs(squatterItem) {
				t.Fatalf("control: by stale slug got %s, want the squatter's %s — the fixture did not reproduce the reuse", idsOf(bySlug), wantIDs(squatterItem))
			}
		})
	}
}
