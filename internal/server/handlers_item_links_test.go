package server

import (
	"net/http"
	"testing"

	"github.com/xarmian/pad/internal/models"
)

func TestCreateItemLinkAcceptsCanonicalLineageTypes(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "Child Task",
		"fields": `{"status":"open"}`,
	})
	var child models.Item
	parseJSON(t, rr, &child)

	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "Parent Task",
		"fields": `{"status":"open"}`,
	})
	var parent models.Item
	parseJSON(t, rr, &parent)

	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+child.Slug+"/links", map[string]interface{}{
		"target_id": parent.ID,
		"link_type": "split-from",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create lineage link: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	var link models.ItemLink
	parseJSON(t, rr, &link)
	if link.LinkType != models.ItemLinkTypeSplitFrom {
		t.Fatalf("expected canonical split_from link type, got %q", link.LinkType)
	}
}

func TestCreateItemLinkRejectsInvalidLineageTypes(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "Task A",
		"fields": `{"status":"open"}`,
	})
	var itemA models.Item
	parseJSON(t, rr, &itemA)

	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "Task B",
		"fields": `{"status":"open"}`,
	})
	var itemB models.Item
	parseJSON(t, rr, &itemB)

	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+itemA.Slug+"/links", map[string]interface{}{
		"target_id": itemB.ID,
		"link_type": "implemented-by",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid link type, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateItemLinkRejectsDuplicatesAndSelfLinks(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "Task A",
		"fields": `{"status":"open"}`,
	})
	var itemA models.Item
	parseJSON(t, rr, &itemA)

	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "Task B",
		"fields": `{"status":"open"}`,
	})
	var itemB models.Item
	parseJSON(t, rr, &itemB)

	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+itemA.Slug+"/links", map[string]interface{}{
		"target_id": itemB.ID,
		"link_type": models.ItemLinkTypeSupersedes,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected first supersedes link to succeed, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+itemA.Slug+"/links", map[string]interface{}{
		"target_id": itemB.ID,
		"link_type": models.ItemLinkTypeSupersedes,
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected duplicate link to return 409, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+itemA.Slug+"/links", map[string]interface{}{
		"target_id": itemA.ID,
		"link_type": models.ItemLinkTypeSupersedes,
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected self-link to return 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestGetItemLinksIncludesEnrichedMetadata(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "Replacement Task",
		"fields": `{"status":"done"}`,
	})
	var replacement models.Item
	parseJSON(t, rr, &replacement)

	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "Legacy Task",
		"fields": `{"status":"open"}`,
	})
	var legacy models.Item
	parseJSON(t, rr, &legacy)

	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+replacement.Slug+"/links", map[string]interface{}{
		"target_id": legacy.ID,
		"link_type": models.ItemLinkTypeSupersedes,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create supersedes link: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+legacy.Slug+"/links", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list links: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var links []models.ItemLink
	parseJSON(t, rr, &links)
	if len(links) != 1 {
		t.Fatalf("expected 1 link, got %d", len(links))
	}

	link := links[0]
	if link.SourceSlug != replacement.Slug {
		t.Fatalf("expected source slug %q, got %q", replacement.Slug, link.SourceSlug)
	}
	if link.TargetSlug != legacy.Slug {
		t.Fatalf("expected target slug %q, got %q", legacy.Slug, link.TargetSlug)
	}
	if link.SourceCollectionSlug != "tasks" || link.TargetCollectionSlug != "tasks" {
		t.Fatalf("expected tasks collection slugs, got source=%q target=%q", link.SourceCollectionSlug, link.TargetCollectionSlug)
	}
	if link.SourceRef != "TASK-1" {
		t.Fatalf("expected source ref TASK-1, got %q", link.SourceRef)
	}
	if link.TargetRef != "TASK-2" {
		t.Fatalf("expected target ref TASK-2, got %q", link.TargetRef)
	}
	if link.SourceStatus != "done" {
		t.Fatalf("expected source status done, got %q", link.SourceStatus)
	}
	if link.TargetStatus != "open" {
		t.Fatalf("expected target status open, got %q", link.TargetStatus)
	}
}
