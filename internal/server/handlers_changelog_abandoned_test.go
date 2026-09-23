package server

import (
	"net/http"
	"strings"
	"testing"
)

// BUG-2347: a collection's own `abandoned_options` decides which terminal
// values close WITHOUT delivering. The live instance: plans declare
// terminal_options [completed, overturned], and "overturned" is not in the
// global NegativeTerminals names, so the changelog reported an overturned
// plan (PLAN-2326, the plan this bug was filed from) as shipped.

func reviewsCollection(t *testing.T, srv *Server, slug, abandoned string) {
	t.Helper()
	schema := `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","completed","overturned"],"terminal_options":["completed","overturned"]` + abandoned + `}]}`
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{
		"name": "Reviews", "schema": schema,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection: %d %s", rr.Code, rr.Body.String())
	}
}

func changelogRefs(t *testing.T, srv *Server, slug string) map[string]bool {
	t.Helper()
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/changelog", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("changelog: %d %s", rr.Code, rr.Body.String())
	}
	var resp ChangelogResponse
	parseJSON(t, rr, &resp)
	refs := map[string]bool{}
	for _, g := range resp.Groups {
		for _, it := range g.Items {
			refs[it.Ref] = true
		}
	}
	return refs
}

func TestChangelogOmitsAValueTheCollectionDeclaresAbandoned(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	reviewsCollection(t, srv, slug, `,"abandoned_options":["overturned"]`)

	overturned := createItem(t, srv, slug, "reviews", map[string]interface{}{"title": "Overturned", "fields": `{"status":"overturned"}`})
	completed := createItem(t, srv, slug, "reviews", map[string]interface{}{"title": "Completed", "fields": `{"status":"completed"}`})

	refs := changelogRefs(t, srv, slug)
	if !refs[completed.Ref] {
		t.Fatalf("the completed item is missing from the changelog: %v", refs)
	}
	if refs[overturned.Ref] {
		t.Fatalf("the changelog reports an item the collection declares abandoned as shipped: %v", refs)
	}
}

// The counterfactual, and the fallback pinned: WITHOUT a declaration the
// global names decide, and "overturned" is not one of them, so it still
// counts as shipped. This is the live defect; a collection fixes it by
// declaring abandoned_options.
func TestChangelogWithoutAbandonedOptionsFallsBackToTheGlobalNames(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	reviewsCollection(t, srv, slug, "")

	overturned := createItem(t, srv, slug, "reviews", map[string]interface{}{"title": "Overturned", "fields": `{"status":"overturned"}`})
	if !changelogRefs(t, srv, slug)[overturned.Ref] {
		t.Fatal("without a declaration, overturned is not a global negative name and must still count (fallback unchanged)")
	}
}

func TestAbandonedOptionsMustBeASubsetOfTerminalOptions(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{
		"name":   "Bad",
		"schema": `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done","dropped"],"terminal_options":["done"],"abandoned_options":["dropped"]}]}`,
	})
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "dropped") {
		t.Fatalf("create: want 400 naming the value, got %d %s", rr.Code, rr.Body.String())
	}

	// The update door refuses it too.
	reviewsCollection(t, srv, slug, "")
	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/collections/reviews", map[string]interface{}{
		"schema": `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","completed","overturned"],"terminal_options":["completed"],"abandoned_options":["overturned"]}]}`,
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("update: want 400, got %d %s", rr.Code, rr.Body.String())
	}
}
