package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestUnparentedListFilterAndAliasConflicts(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	parent := createItem(t, srv, slug, "tasks", map[string]any{"title": "Parent", "fields": `{"status":"open"}`})
	orphan := createItem(t, srv, slug, "tasks", map[string]any{"title": "Orphan", "fields": `{"status":"open"}`})
	child := createItem(t, srv, slug, "tasks", map[string]any{
		"title": "Child", "fields": `{"status":"open","parent":"` + parent.Ref + `"}`,
	})

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/collections/tasks/items?unparented=true", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("unparented list: %d: %s", rr.Code, rr.Body.String())
	}
	var items []models.Item
	parseJSON(t, rr, &items)
	seen := map[string]bool{}
	for _, item := range items {
		seen[item.ID] = true
	}
	if !seen[orphan.ID] {
		t.Fatal("orphan missing from unparented results")
	}
	if seen[child.ID] {
		t.Fatal("parent-linked child returned by unparented filter")
	}

	for _, alias := range []string{"parent_id=" + parent.ID, "parent=" + parent.Ref, "plan=" + parent.Ref} {
		rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items?unparented=true&"+alias, nil)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("conflict %q: got %d, want 400 (%s)", alias, rr.Code, rr.Body.String())
		}
	}
}

func TestUnparentedRestrictedGateAndMetadataOmission(t *testing.T) {
	t.Parallel()
	f := newBearerGateItemFixture(t)

	rr := doRequestWithHeaders(f.srv, "GET", "/api/v1/workspaces/"+f.ws.Slug+"/items?unparented=true", nil, f.bearerHeaders())
	if rr.Code != http.StatusForbidden {
		t.Fatalf("restricted unparented: got %d, want 403 (%s)", rr.Code, rr.Body.String())
	}

	rr = doRequestWithHeaders(f.srv, "GET", "/api/v1/workspaces/"+f.ws.Slug+"/items-index", nil, f.bearerHeaders())
	if rr.Code != http.StatusOK {
		t.Fatalf("restricted index: %d: %s", rr.Code, rr.Body.String())
	}
	var restricted itemsIndexBody
	parseJSON(t, rr, &restricted)
	for _, item := range restricted.Items {
		if item.IsUnparented != nil {
			t.Fatalf("restricted index leaked is_unparented for %s", item.Ref)
		}
	}

	rr = doRequestWithCookie(f.srv, "GET", "/api/v1/workspaces/"+f.ws.Slug+"/items-index", nil, f.sessionToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("unrestricted index: %d: %s", rr.Code, rr.Body.String())
	}
	var unrestricted itemsIndexBody
	parseJSON(t, rr, &unrestricted)
	for _, item := range unrestricted.Items {
		if item.IsUnparented == nil {
			t.Fatalf("unrestricted index omitted is_unparented for %s", item.Ref)
		}
	}

	rr = doRequestWithHeaders(f.srv, "GET", "/api/v1/workspaces/"+f.ws.Slug+"/items-changes?since=0", nil, f.bearerHeaders())
	if rr.Code != http.StatusOK {
		t.Fatalf("restricted delta: %d: %s", rr.Code, rr.Body.String())
	}
	var delta itemsChangesBody
	parseJSON(t, rr, &delta)
	for _, change := range delta.Changes {
		if change.IsUnparented != nil {
			t.Fatalf("restricted delta leaked is_unparented for %s", change.Ref)
		}
	}

	savedView, err := f.srv.store.CreateView(f.ws.ID, models.ViewCreate{
		CollectionID: &f.visibleCollID, Name: "Loose", ViewType: "list",
		Config: `{"filters":[{"field":"$unparented","op":"eq","value":true}]}`,
	})
	if err != nil {
		t.Fatalf("create saved view: %v", err)
	}
	rr = doRequestWithHeaders(f.srv, "GET", "/api/v1/workspaces/"+f.ws.Slug+"/collections/visible/views", nil, f.bearerHeaders())
	if rr.Code != http.StatusOK {
		t.Fatalf("restricted views: %d: %s", rr.Code, rr.Body.String())
	}
	var restrictedViews []models.View
	parseJSON(t, rr, &restrictedViews)
	if len(restrictedViews) != 1 || strings.Contains(restrictedViews[0].Config, reservedUnparentedViewField) {
		t.Fatalf("restricted views leaked reserved filter: %+v", restrictedViews)
	}
	rr = doRequestWithCookie(f.srv, "GET", "/api/v1/workspaces/"+f.ws.Slug+"/collections/visible/views", nil, f.sessionToken)
	var unrestrictedViews []models.View
	parseJSON(t, rr, &unrestrictedViews)
	if len(unrestrictedViews) != 1 || !strings.Contains(unrestrictedViews[0].Config, reservedUnparentedViewField) {
		t.Fatalf("unrestricted views lost reserved filter: %+v", unrestrictedViews)
	}

	// A restricted PATCH that leaves config untouched must sanitize the
	// response too; otherwise the stored pseudo-filter leaks through the
	// update readback even though list/create paths are gated.
	rr = doRequestWithHeaders(f.srv, "PATCH", "/api/v1/workspaces/"+f.ws.Slug+"/collections/visible/views/"+savedView.ID, map[string]any{
		"name": "Loose renamed",
	}, f.bearerHeaders())
	if rr.Code != http.StatusOK {
		t.Fatalf("restricted view update: %d: %s", rr.Code, rr.Body.String())
	}
	var restrictedUpdate models.View
	parseJSON(t, rr, &restrictedUpdate)
	if strings.Contains(restrictedUpdate.Config, reservedUnparentedViewField) {
		t.Fatalf("restricted update response leaked reserved filter: %+v", restrictedUpdate)
	}
	storedView, err := f.srv.store.GetView(savedView.ID)
	if err != nil || storedView == nil || !strings.Contains(storedView.Config, reservedUnparentedViewField) {
		t.Fatalf("restricted response sanitization mutated stored config: view=%+v err=%v", storedView, err)
	}
}

func TestCreateWithParentPublishesFreshSequence(t *testing.T) {
	t.Parallel()
	srv := testServerWithEvents(t)
	slug := createWSWithCollections(t, srv)
	parent := createItem(t, srv, slug, "plans", map[string]any{"title": "Plan", "fields": `{"status":"active"}`})
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("workspace: %v", err)
	}
	ch := srv.events.Subscribe(ws.ID)
	defer srv.events.Unsubscribe(ch)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]any{
		"title": "Fresh child", "fields": `{"status":"open","parent":"` + parent.Ref + `"}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create child: %d: %s", rr.Code, rr.Body.String())
	}
	var child models.Item
	parseJSON(t, rr, &child)
	event := awaitItemEvent(t, ch, events.ItemCreated, child.ID)
	if event.Seq != child.Seq {
		t.Fatalf("create event seq=%d, response seq=%d", event.Seq, child.Seq)
	}
	stored, err := srv.store.GetItem(child.ID)
	if err != nil || stored == nil {
		t.Fatalf("stored child: item=%v err=%v", stored, err)
	}
	if stored.Seq != child.Seq {
		t.Fatalf("response seq=%d is stale; stored=%d", child.Seq, stored.Seq)
	}
}

func TestStructuralLinkHandlersPublishFreshSourceEvent(t *testing.T) {
	t.Parallel()
	srv := testServerWithEvents(t)
	slug := createWSWithCollections(t, srv)
	source := createItem(t, srv, slug, "tasks", map[string]any{"title": "Source", "fields": `{"status":"open"}`})
	target := createItem(t, srv, slug, "ideas", map[string]any{"title": "Target", "fields": `{"status":"new"}`})
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("workspace: %v", err)
	}
	ch := srv.events.Subscribe(ws.ID)
	defer srv.events.Unsubscribe(ch)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+source.Slug+"/links", map[string]any{
		"target_id": target.ID, "link_type": models.ItemLinkTypeImplements,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create implements: %d: %s", rr.Code, rr.Body.String())
	}
	var link models.ItemLink
	parseJSON(t, rr, &link)
	createdEvent := awaitItemEvent(t, ch, events.ItemUpdated, source.ID)
	createdSource, _ := srv.store.GetItem(source.ID)
	if createdSource == nil || createdEvent.Seq != createdSource.Seq {
		t.Fatalf("create link event seq=%d, stored=%v", createdEvent.Seq, createdSource)
	}

	rr = doRequest(srv, "DELETE", "/api/v1/workspaces/"+slug+"/links/"+link.ID, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete implements: %d: %s", rr.Code, rr.Body.String())
	}
	deletedEvent := awaitItemEvent(t, ch, events.ItemUpdated, source.ID)
	deletedSource, _ := srv.store.GetItem(source.ID)
	if deletedSource == nil || deletedEvent.Seq != deletedSource.Seq || deletedEvent.Seq <= createdEvent.Seq {
		t.Fatalf("delete link event seq=%d, create seq=%d, stored=%v", deletedEvent.Seq, createdEvent.Seq, deletedSource)
	}
}

func awaitItemEvent(t *testing.T, ch <-chan events.Event, eventType, itemID string) events.Event {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-ch:
			if event.Type == eventType && event.ItemID == itemID {
				return event
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s event for %s", eventType, itemID)
			return events.Event{}
		}
	}
}
