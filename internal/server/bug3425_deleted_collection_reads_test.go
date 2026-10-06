package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3425: the workspace graph builds its nodes from ListItems, so a
// soft-deleted collection's items showed up as nodes for an unrestricted
// viewer. The live task is the control.
func TestGraphExcludesItemsOfSoftDeletedCollections(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("workspace: %v", err)
	}
	doomColl, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Drafts", Slug: "drafts", Prefix: "DRF"})
	if err != nil {
		t.Fatalf("create collection: %v", err)
	}

	keep := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Kept task", "fields": map[string]interface{}{"status": "open"},
	})
	doomed := createItem(t, srv, slug, "drafts", map[string]interface{}{"title": "Draft in a doomed collection"})
	createBlocksLink(t, srv, slug, keep.Slug, doomed.ID)

	before := getGraph(t, srv, slug, "")
	if graphNode(before, keep.Ref) == nil || graphNode(before, doomed.Ref) == nil {
		t.Fatalf("control: both nodes should be in the graph before the delete")
	}

	if err := srv.store.DeleteCollection(doomColl.ID, ""); err != nil {
		t.Fatalf("soft-delete collection: %v", err)
	}
	after := getGraph(t, srv, slug, "")
	if graphNode(after, doomed.Ref) != nil {
		t.Errorf("graph still has a node for an item of a soft-deleted collection")
	}
	if graphNode(after, keep.Ref) == nil {
		t.Errorf("graph lost the live node")
	}
	if len(after.Edges) != 0 {
		t.Errorf("graph still has an edge to the deleted collection's item: %+v", after.Edges)
	}
}

// The account data export keeps a user's items from collections they deleted,
// exactly as before BUG-3425 (it opts back in with IncludeDeletedCollections).
func TestExportAccount_KeepsItemsOfSoftDeletedCollections(t *testing.T) {
	srv := testServer(t)
	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "export-3425@example.com", Name: "Export Owner", Username: "export-3425",
		Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Export3425", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	liveColl, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Notes", Slug: "notes", Prefix: "NOT"})
	if err != nil {
		t.Fatalf("create live collection: %v", err)
	}
	doomColl, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Drafts", Slug: "drafts", Prefix: "DRF"})
	if err != nil {
		t.Fatalf("create doomed collection: %v", err)
	}
	if _, err := srv.store.CreateItem(ws.ID, liveColl.ID, models.ItemCreate{Title: "Export live marker"}); err != nil {
		t.Fatalf("create live item: %v", err)
	}
	if _, err := srv.store.CreateItem(ws.ID, doomColl.ID, models.ItemCreate{Title: "Export doomed marker"}); err != nil {
		t.Fatalf("create doomed item: %v", err)
	}
	if err := srv.store.DeleteCollection(doomColl.ID, ""); err != nil {
		t.Fatalf("soft-delete collection: %v", err)
	}
	sessTok, err := srv.store.CreateSession(owner.ID, "go-test", "192.0.2.1", "", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	rr := doRequestWithCookie(srv, "GET", "/api/v1/auth/export", nil, sessTok)
	if rr.Code != http.StatusOK {
		t.Fatalf("export: %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Export live marker") {
		t.Fatalf("control: the export lost the live item")
	}
	if !strings.Contains(body, "Export doomed marker") {
		t.Errorf("the account export dropped an item of a soft-deleted collection")
	}
}
