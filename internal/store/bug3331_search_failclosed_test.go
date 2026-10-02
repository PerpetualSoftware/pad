package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3331: a search with no permission filter is not an unfiltered search.
// Unless Unrestricted says so, empty CollectionIDs and ItemIDs (nil or not)
// match nothing, on every query path (FTS, direct ref, bare number) and in
// the total and facets. searchFacets and the ref/number lookups are tested
// directly too, so a path that skipped Search's early return is still caught.
func TestBUG3331_SearchWithoutPermissionFilterMatchesNothing(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "FailClosed")
	s.SeedDefaultCollections(ws.ID)
	docs, err := s.GetCollectionBySlug(ws.ID, "docs")
	if err != nil || docs == nil {
		t.Fatalf("docs collection: %v", err)
	}
	item, err := s.CreateItem(ws.ID, docs.ID, models.ItemCreate{Title: "Quokka notes", Content: "quokka quokka"})
	if err != nil {
		t.Fatal(err)
	}
	ref := item.Ref
	if ref == "" {
		t.Fatal("item has no ref")
	}

	for _, q := range []string{"quokka", ref, "1"} {
		for name, p := range map[string]SearchParams{
			"nil filter":               {Query: q, Workspace: ws.Slug},
			"non-nil empty filter":     {Query: q, Workspace: ws.Slug, CollectionIDs: []string{}, ItemIDs: []string{}},
			"workspace IDs, no filter": {Query: q, WorkspaceIDs: []string{ws.ID}},
		} {
			resp, err := s.Search(p)
			if err != nil {
				t.Fatalf("%s q=%q: %v", name, q, err)
			}
			if len(resp.Results) != 0 || resp.Total != 0 {
				t.Errorf("%s q=%q: %d results (total %d), want none", name, q, len(resp.Results), resp.Total)
			}
		}
		// The same query, explicitly unrestricted, does find it: the empty
		// answers above are the filter, not a broken query.
		resp, err := s.Search(SearchParams{Query: q, Workspace: ws.Slug, Unrestricted: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Results) == 0 {
			t.Errorf("unrestricted q=%q found nothing; the control is broken", q)
		}
	}

	// The predicate itself, independent of Search's early return.
	query, args := appendSearchPermissionFilter("SELECT 1 FROM items i WHERE 1=1", nil, SearchParams{})
	if query != "SELECT 1 FROM items i WHERE 1=1 AND 1 = 0" || len(args) != 0 {
		t.Errorf("empty restricted filter: %q %v, want a match-nothing predicate", query, args)
	}
	query, _ = appendSearchPermissionFilter("q", nil, SearchParams{Unrestricted: true})
	if query != "q" {
		t.Errorf("unrestricted filter added %q", query)
	}
	query, args = appendSearchPermissionFilter("q", nil, SearchParams{Unrestricted: true, ItemIDs: []string{"x"}})
	if query != "q AND i.id IN (?)" || len(args) != 1 {
		t.Errorf("unrestricted with IDs must still apply them: %q %v", query, args)
	}
	if f := s.searchFacets(SearchParams{Query: "quokka", Workspace: ws.Slug}); f != nil {
		for k, v := range f.Collections {
			t.Errorf("facets leak a collection count without a filter: %s=%d", k, v)
		}
		for k, v := range f.Statuses {
			t.Errorf("facets leak a status count without a filter: %s=%d", k, v)
		}
	}
}
