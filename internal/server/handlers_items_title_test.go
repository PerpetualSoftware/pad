package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// HTTP-layer regressions for item title validation (BUG-2833 empty,
// BUG-2831 length).
//
// These sit alongside the store-level tests in internal/store rather than
// duplicating them, and they assert something the store tests cannot: the
// STATUS CODE. A refusal that reaches the client as writeInternalError's 500
// tells the caller the server broke and that a retry might work, when the
// request was understood and deliberately declined — which is exactly the shape
// BUG-2831 filed against the Postgres path (SQLSTATE 54000 reaching the generic
// error arm).

// TestPatchItemEmptyTitleRefused is the verbatim BUG-2833 repro: the filing
// measured PATCH {"title": ""} being accepted and applied while POST refused
// the same input with 400 "Title is required".
func TestPatchItemEmptyTitleRefused(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, ws, "Original Title", `{"status":"open"}`)

	for _, title := range []string{"", "   ", "\t\n"} {
		rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+ws+"/items/"+item.Slug, map[string]interface{}{
			"title": title,
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("PATCH title=%q: got %d, want 400 (body: %s)", title, rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "Title is required") {
			t.Errorf("PATCH title=%q: body %s, want it to say the title is required", title, rr.Body.String())
		}
	}

	// The item must be untouched — a 400 that already wrote the row would
	// satisfy every assertion above and still be the bug.
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items/"+item.Slug, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET after refusals: %d", rr.Code)
	}
	var after models.Item
	parseJSON(t, rr, &after)
	if after.Title != "Original Title" {
		t.Errorf("title = %q, want it untouched (%q)", after.Title, "Original Title")
	}
}

// TestPatchItemOverlongTitleRefusedAs400 is the BUG-2831 shape assertion. The
// filing explicitly flagged the status code as READ, not measured end-to-end
// ("NOT independently verified end-to-end: I read the store error and the
// handler's fall-through"). This measures it.
func TestPatchItemOverlongTitleRefusedAs400(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, ws, "Short", `{"status":"open"}`)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+ws+"/items/"+item.Slug, map[string]interface{}{
		"title": strings.Repeat("a", models.MaxItemTitleRunes+1),
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "too long") {
		t.Errorf("body = %s, want it to say the title is too long", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "internal_error") {
		t.Errorf("body = %s, must not be an internal_error envelope", rr.Body.String())
	}
}

// TestCreateItemWhitespaceOnlyTitleRefused pins the deliberate widening of the
// CREATE door. "   " was accepted here before this unit and refused by the
// artifact-import door whose comment claimed to mirror this one.
func TestCreateItemWhitespaceOnlyTitleRefused(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections/tasks/items", map[string]interface{}{
		"title": "   ",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Title is required") {
		t.Errorf("body = %s, want it to say the title is required", rr.Body.String())
	}
}

// TestCreateItemOverlongTitleRefused is the create half of BUG-2831 — the
// original measurement was on CreateItem, where a 2 MiB title succeeded on
// SQLite and produced `index row requires 24064 bytes, maximum size is 8191`
// on Postgres.
func TestCreateItemOverlongTitleRefused(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections/tasks/items", map[string]interface{}{
		"title": strings.Repeat("a", models.MaxItemTitleRunes+1),
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "too long") {
		t.Errorf("body = %s, want it to say the title is too long", rr.Body.String())
	}
}

// TestItemTitleTrimmedOverTheWire: the API must persist what it validated. A
// door that validates the trimmed string and stores the raw one leaves the row
// holding a value nothing checked.
func TestItemTitleTrimmedOverTheWire(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections/tasks/items", map[string]interface{}{
		"title": "  Padded  ",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: got %d, want 201 (body: %s)", rr.Code, rr.Body.String())
	}
	var created models.Item
	parseJSON(t, rr, &created)
	if created.Title != "Padded" {
		t.Errorf("created title = %q, want %q", created.Title, "Padded")
	}

	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+ws+"/items/"+created.Slug, map[string]interface{}{
		"title": "  Renamed  ",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("patch: got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var updated models.Item
	parseJSON(t, rr, &updated)
	if updated.Title != "Renamed" {
		t.Errorf("updated title = %q, want %q", updated.Title, "Renamed")
	}
}
