package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3344: a soft-deleted COLLECTION leaves its items live (deleted_at is
// set on the collection row only), so the wiki backlink queries, which
// checked s.deleted_at alone, kept listing sources from a collection nobody
// can open any more, for an unrestricted viewer too, who has no collection
// filter to catch it. Relation backlinks already check c.deleted_at
// (relation_links.go). Each test keeps a live source beside the doomed one,
// so the assertion cannot pass by the query returning nothing.

func TestGetBacklinks_ExcludesSourcesFromSoftDeletedCollections(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Backlinks")
	targetColl := createTestCollection(t, s, ws.ID, "Tasks")
	liveColl := createTestCollection(t, s, ws.ID, "Notes")
	doomedColl := createTestCollection(t, s, ws.ID, "Drafts")

	target := createTestItem(t, s, ws.ID, targetColl.ID, "Target", "")
	link := "See [[" + refOf(target) + "]]."
	live := createTestItem(t, s, ws.ID, liveColl.ID, "Live source", link)
	doomed := createTestItem(t, s, ws.ID, doomedColl.ID, "Doomed source", link)

	all := BacklinksVisibility{Unrestricted: true}
	before, err := s.GetBacklinks(target.ID, ws.ID, 50, 0, all)
	if err != nil {
		t.Fatalf("GetBacklinks before: %v", err)
	}
	if !hasBacklinkSource(before, doomed.ID) || !hasBacklinkSource(before, live.ID) {
		t.Fatalf("control: both sources should be listed before the delete, got %+v", before)
	}

	if err := s.DeleteCollection(doomedColl.ID, ""); err != nil {
		t.Fatalf("soft-delete collection: %v", err)
	}
	after, err := s.GetBacklinks(target.ID, ws.ID, 50, 0, all)
	if err != nil {
		t.Fatalf("GetBacklinks after: %v", err)
	}
	if hasBacklinkSource(after, doomed.ID) {
		t.Errorf("GetBacklinks still lists a source from a soft-deleted collection")
	}
	if !hasBacklinkSource(after, live.ID) {
		t.Errorf("GetBacklinks lost the live source")
	}
}

func TestCountBacklinks_ExcludesSourcesFromSoftDeletedCollections(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "BacklinkCount")
	targetColl := createTestCollection(t, s, ws.ID, "Tasks")
	liveColl := createTestCollection(t, s, ws.ID, "Notes")
	doomedColl := createTestCollection(t, s, ws.ID, "Drafts")

	target := createTestItem(t, s, ws.ID, targetColl.ID, "Target", "")
	link := "See [[" + refOf(target) + "]]."
	createTestItem(t, s, ws.ID, liveColl.ID, "Live source", link)
	createTestItem(t, s, ws.ID, doomedColl.ID, "Doomed source", link)

	all := BacklinksVisibility{Unrestricted: true}
	if n, err := s.CountBacklinks(target.ID, ws.ID, all); err != nil || n != 2 {
		t.Fatalf("control: count before the delete = %d (err %v), want 2", n, err)
	}
	if err := s.DeleteCollection(doomedColl.ID, ""); err != nil {
		t.Fatalf("soft-delete collection: %v", err)
	}
	n, err := s.CountBacklinks(target.ID, ws.ID, all)
	if err != nil {
		t.Fatalf("CountBacklinks after: %v", err)
	}
	if n != 1 {
		t.Errorf("CountBacklinks after the delete = %d, want 1 (the live source only)", n)
	}
	// The count and the page must agree, or a pager shows a total it can
	// never reach.
	page, err := s.GetBacklinks(target.ID, ws.ID, 50, 0, all)
	if err != nil {
		t.Fatalf("GetBacklinks: %v", err)
	}
	if len(page) != n {
		t.Errorf("count %d disagrees with the page's %d rows", n, len(page))
	}
}

func TestGetCrossWorkspaceBacklinks_ExcludesSourcesFromSoftDeletedCollections(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	user := createTestUser(t, s, "xws-3344@example.com", "Owner", "password123")
	wsA := createTestWorkspace(t, s, "Target WS")
	wsB := createTestWorkspace(t, s, "Source WS")
	for _, ws := range []string{wsA.ID, wsB.ID} {
		if err := s.AddWorkspaceMember(ws, user.ID, "owner"); err != nil {
			t.Fatalf("add owner: %v", err)
		}
	}
	targetColl := createTestCollection(t, s, wsA.ID, "Tasks")
	liveColl := createTestCollection(t, s, wsB.ID, "Notes")
	doomedColl := createTestCollection(t, s, wsB.ID, "Drafts")

	target := createTestItem(t, s, wsA.ID, targetColl.ID, "Target", "")
	targetRef := refOf(target)
	link := "See [[" + wsA.Slug + "::" + targetRef + "]]."
	live := createTestItem(t, s, wsB.ID, liveColl.ID, "Live source", link)
	doomed := createTestItem(t, s, wsB.ID, doomedColl.ID, "Doomed source", link)

	before, err := s.GetCrossWorkspaceBacklinks(wsA.ID, targetRef, user.ID, nil, 50, 0, false)
	if err != nil {
		t.Fatalf("GetCrossWorkspaceBacklinks before: %v", err)
	}
	if !hasBacklinkSource(before, doomed.ID) || !hasBacklinkSource(before, live.ID) {
		t.Fatalf("control: both sources should be listed before the delete, got %+v", before)
	}

	if err := s.DeleteCollection(doomedColl.ID, ""); err != nil {
		t.Fatalf("soft-delete collection: %v", err)
	}
	after, err := s.GetCrossWorkspaceBacklinks(wsA.ID, targetRef, user.ID, nil, 50, 0, false)
	if err != nil {
		t.Fatalf("GetCrossWorkspaceBacklinks after: %v", err)
	}
	if hasBacklinkSource(after, doomed.ID) {
		t.Errorf("cross-workspace backlinks still list a source from a soft-deleted collection")
	}
	if !hasBacklinkSource(after, live.ID) {
		t.Errorf("cross-workspace backlinks lost the live source")
	}
}

func hasBacklinkSource(bls []models.Backlink, id string) bool {
	for _, b := range bls {
		if b.SourceItemID == id {
			return true
		}
	}
	return false
}
