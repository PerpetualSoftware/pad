package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3447: creating a collection published nothing, so a tab open on the
// workspace (the first-run launchpad while an agent onboards) learned of it
// only when some other event refreshed the sidebar, 12-40 s later. Create now
// publishes the existing collection_updated kind (lead: prefer an existing
// kind), which the workspace layout already answers by reloading the
// collection list, carrying the new collection's stable id and slug.
func TestCreateCollection_PublishesCollectionUpdated(t *testing.T) {
	srv := testServerWithEvents(t)
	slug := createWSWithCollections(t, srv)
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("workspace: %v", err)
	}

	ch, _, _ := srv.events.Subscribe(context.Background(), ws.ID)
	defer srv.events.Unsubscribe(ch)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{
		"name": "Bugs",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var coll models.Collection
	parseJSON(t, rr, &coll)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-ch:
			if event.Type != events.CollectionUpdated {
				continue
			}
			if event.CollectionID != coll.ID || event.Collection != coll.Slug || event.WorkspaceID != ws.ID {
				t.Fatalf("collection_updated = {id %q, slug %q, ws %q}, want the new collection {%q, %q, %q}",
					event.CollectionID, event.Collection, event.WorkspaceID, coll.ID, coll.Slug, ws.ID)
			}
			if event.NewSlug != "" || event.ItemsChanged {
				t.Fatalf("a create is not a rename or a migration: new_slug=%q items_changed=%v", event.NewSlug, event.ItemsChanged)
			}
			if event.Actor != "" || event.ActorName != "" || event.Source != "" {
				t.Fatalf("collection_updated leaked actor metadata: %q %q %q", event.Actor, event.ActorName, event.Source)
			}
			return
		case <-deadline:
			t.Fatal("creating a collection published no collection_updated event")
		}
	}
}
