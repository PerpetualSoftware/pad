package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3428 phase 1: the reads BUG-3425 left out. Global search, workspace tags,
// and child items / progress still included the items of a soft-deleted
// collection. Each test keeps a live item beside the doomed one, so a read
// that returns nothing cannot pass. Reuses bug3425World (same package).

func searchIDs(t *testing.T, s *Store, ws *models.Workspace, q string) (map[string]bool, *SearchResponse) {
	t.Helper()
	resp, err := s.Search(SearchParams{Query: q, Workspace: ws.Slug, Unrestricted: true})
	if err != nil {
		t.Fatalf("Search(%q): %v", q, err)
	}
	ids := map[string]bool{}
	for _, r := range resp.Results {
		ids[r.Item.ID] = true
	}
	return ids, resp
}

func TestSearch_ExcludesSoftDeletedCollections(t *testing.T) {
	t.Parallel()
	w := newBug3425World(t)

	ids, _ := searchIDs(t, w.s, w.ws, "Zephyr")
	if !ids[w.doomed.ID] || !ids[w.live.ID] {
		t.Fatalf("control: text search should find both items before the delete")
	}
	if ref, _ := searchIDs(t, w.s, w.ws, refOf(w.doomed)); !ref[w.doomed.ID] {
		t.Fatalf("control: a ref search should find the doomed item before the delete")
	}

	w.deleteDoomed(t)
	ids, resp := searchIDs(t, w.s, w.ws, "Zephyr")
	if ids[w.doomed.ID] {
		t.Errorf("text search still finds an item from a soft-deleted collection")
	}
	if !ids[w.live.ID] {
		t.Errorf("text search lost the live item")
	}
	if resp.Total != len(resp.Results) {
		t.Errorf("total %d disagrees with %d results", resp.Total, len(resp.Results))
	}
	if resp.Facets != nil && resp.Facets.Collections[w.doomColl.Slug] != 0 {
		t.Errorf("facets still count the soft-deleted collection: %v", resp.Facets.Collections)
	}
	if ref, _ := searchIDs(t, w.s, w.ws, refOf(w.doomed)); ref[w.doomed.ID] {
		t.Errorf("a ref search still finds an item from a soft-deleted collection")
	}
}

func TestListWorkspaceTags_ExcludesSoftDeletedCollections(t *testing.T) {
	t.Parallel()
	w := newBug3425World(t)
	tag := func(it *models.Item, tags string) {
		t.Helper()
		if _, err := w.s.UpdateItem(it.ID, models.ItemUpdate{Tags: &tags}); err != nil {
			t.Fatalf("tag %s: %v", it.Title, err)
		}
	}
	tag(w.live, `["shared"]`)
	tag(w.doomed, `["shared","doomed-only"]`)
	counts := func() map[string]int {
		t.Helper()
		tc, err := w.s.ListWorkspaceTags(w.ws.ID, nil, nil)
		if err != nil {
			t.Fatalf("ListWorkspaceTags: %v", err)
		}
		m := map[string]int{}
		for _, c := range tc {
			m[c.Tag] = c.Count
		}
		return m
	}
	if c := counts(); c["shared"] != 2 || c["doomed-only"] != 1 {
		t.Fatalf("control: want shared=2 doomed-only=1 before the delete, got %v", c)
	}
	w.deleteDoomed(t)
	c := counts()
	if c["shared"] != 1 {
		t.Errorf("shared counts the soft-deleted collection's item: %d, want 1", c["shared"])
	}
	if _, ok := c["doomed-only"]; ok {
		t.Errorf("a tag only a soft-deleted collection's item carries still lists: %v", c)
	}
}

func TestChildrenAndProgress_ExcludeSoftDeletedCollections(t *testing.T) {
	t.Parallel()
	w := newBug3425World(t)
	s := w.s
	parentColl := createTestCollection(t, s, w.ws.ID, "Plans")
	parent := createTestItem(t, s, w.ws.ID, parentColl.ID, "Parent", "")
	for _, child := range []*models.Item{w.live, w.doomed} {
		if _, err := s.CreateItemLink(w.ws.ID, models.ItemLinkCreate{TargetID: parent.ID, LinkType: "parent"}, child.ID); err != nil {
			t.Fatalf("link %s: %v", child.Title, err)
		}
	}
	check := func(when string, wantDoomed bool, wantTotal int) {
		t.Helper()
		kids, err := s.GetChildItems(parent.ID)
		if err != nil {
			t.Fatalf("%s GetChildItems: %v", when, err)
		}
		if !hasItem(kids, w.live.ID) || hasItem(kids, w.doomed.ID) != wantDoomed {
			t.Errorf("%s GetChildItems: live=%v doomed=%v, want doomed=%v", when, hasItem(kids, w.live.ID), hasItem(kids, w.doomed.ID), wantDoomed)
		}
		batch, err := s.GetChildItemsForParents([]string{parent.ID})
		if err != nil {
			t.Fatalf("%s GetChildItemsForParents: %v", when, err)
		}
		if !hasItem(batch[parent.ID], w.live.ID) || hasItem(batch[parent.ID], w.doomed.ID) != wantDoomed {
			t.Errorf("%s GetChildItemsForParents disagrees: doomed=%v, want %v", when, hasItem(batch[parent.ID], w.doomed.ID), wantDoomed)
		}
		total, _, err := s.GetItemProgress(parent.ID)
		if err != nil {
			t.Fatalf("%s GetItemProgress: %v", when, err)
		}
		if total != wantTotal {
			t.Errorf("%s GetItemProgress total = %d, want %d", when, total, wantTotal)
		}
		all, err := s.GetAllItemProgress(w.ws.ID, parentColl.Slug, false)
		if err != nil {
			t.Fatalf("%s GetAllItemProgress: %v", when, err)
		}
		for _, p := range all {
			if p.ItemID == parent.ID && p.Total != wantTotal {
				t.Errorf("%s GetAllItemProgress total = %d, want %d", when, p.Total, wantTotal)
			}
		}
	}
	check("before the delete (control)", true, 2)
	w.deleteDoomed(t)
	check("after the delete", false, 1)
}

// The open-children guard reads children through GetChildItemsTx and is NOT
// narrowed by BUG-3428: whether an open child in a soft-deleted collection
// should still block closing its parent is a write-path rule, left for the
// lead's ruling. This pins the current behaviour so changing it is a decision.
func TestGetChildItemsTx_StillIncludesSoftDeletedCollections(t *testing.T) {
	t.Parallel()
	w := newBug3425World(t)
	s := w.s
	parentColl := createTestCollection(t, s, w.ws.ID, "Plans")
	parent := createTestItem(t, s, w.ws.ID, parentColl.ID, "Parent", "")
	if _, err := s.CreateItemLink(w.ws.ID, models.ItemLinkCreate{TargetID: parent.ID, LinkType: "parent"}, w.doomed.ID); err != nil {
		t.Fatalf("link: %v", err)
	}
	w.deleteDoomed(t)
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	kids, err := s.GetChildItemsTx(tx, parent.ID)
	if err != nil {
		t.Fatalf("GetChildItemsTx: %v", err)
	}
	if !hasItem(kids, w.doomed.ID) {
		t.Errorf("the guard's child read no longer sees a soft-deleted collection's child; that changes what blocks closing a parent")
	}
}
