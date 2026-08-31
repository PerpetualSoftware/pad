package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
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

// TestWriteInvalidItemTitleMapsTo400 covers the store-refusal arm DIRECTLY.
//
// It exists because a mutation test showed the arm surviving: the handlers'
// own pre-lock checks catch every case reachable from an HTTP test, so the
// store's typed refusal never arrives through the wire in the suite. The arm is
// still load-bearing — it is what a title that only becomes invalid UNDER THE
// LOCK lands on (the handler compares against an item read before any lock, so
// a concurrent rename can turn an echoed legacy title into a genuine one) — and
// an untested error mapping is how a deliberate 400 becomes a 500 in a later
// refactor. Testing the mapping directly is the honest way to cover a branch
// whose only production trigger is a race.
func TestWriteInvalidItemTitleMapsTo400(t *testing.T) {
	t.Run("maps the typed refusal", func(t *testing.T) {
		rr := httptest.NewRecorder()
		if !writeInvalidItemTitle(rr, &store.InvalidItemTitleError{Reason: "Title is required"}) {
			t.Fatal("writeInvalidItemTitle returned false for its own error type")
		}
		if rr.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "Title is required") {
			t.Errorf("body = %s, want the typed Reason", rr.Body.String())
		}
	})

	t.Run("maps through a wrapper without publishing it", func(t *testing.T) {
		// The call path wraps this error on the way up. The client must get the
		// typed Reason, not the accumulated wrapper text.
		wrapped := fmt.Errorf("update item: %w", &store.InvalidItemTitleError{Reason: "Title is too long: 300 characters, maximum 255"})
		rr := httptest.NewRecorder()
		if !writeInvalidItemTitle(rr, wrapped) {
			t.Fatal("writeInvalidItemTitle returned false for a wrapped refusal; errors.As must see through wrappers")
		}
		if rr.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rr.Code)
		}
		if strings.Contains(rr.Body.String(), "update item:") {
			t.Errorf("body = %s, must not publish the wrapper text", rr.Body.String())
		}
	})

	t.Run("declines everything else", func(t *testing.T) {
		// The control leg. Without it this test would pass against a helper
		// that returns true unconditionally, which would swallow every other
		// store error into a 400.
		rr := httptest.NewRecorder()
		if writeInvalidItemTitle(rr, errors.New("some unrelated store failure")) {
			t.Error("writeInvalidItemTitle claimed an unrelated error; it must fall through")
		}
		if rr.Code != http.StatusOK || rr.Body.Len() != 0 {
			t.Errorf("declining must write nothing, got status %d body %q", rr.Code, rr.Body.String())
		}
	})
}

// TestCreateItemCheckedMapsStoreTitleRefusalTo400 covers the create path's
// store-refusal arm directly, for the same reason as the update path's helper
// test above: handleCreateItem's own check catches everything reachable over
// the wire, so the arm's absence is a latent 500 rather than a visible one — and
// a mutation test is what surfaced it.
//
// createItemChecked is a separate function from handleCreateItem, and the
// pre-check lives in the handler, so calling it directly reaches the store
// refusal that no HTTP request can.
func TestCreateItemCheckedMapsStoreTitleRefusalTo400(t *testing.T) {
	srv := testServer(t)
	wsSlug := createWSWithCollections(t, srv)
	ws, err := srv.store.GetWorkspaceBySlug(wsSlug)
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspaceBySlug(%q): %v", wsSlug, err)
	}
	coll, err := srv.store.GetCollectionBySlug(ws.ID, "tasks")
	if err != nil || coll == nil {
		t.Fatalf("GetCollectionBySlug: %v", err)
	}
	var schema models.CollectionSchema
	if err := json.Unmarshal([]byte(coll.Schema), &schema); err != nil {
		t.Fatalf("parse schema: %v", err)
	}

	req := httptest.NewRequest("POST", "/", nil)
	for _, tc := range []struct{ name, title, want string }{
		{"empty", "", "Title is required"},
		{"over the bound", strings.Repeat("a", models.MaxItemTitleRunes+1), "too long"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, cerr := srv.createItemChecked(req, ws.ID, coll, schema,
				models.ItemCreate{Title: tc.title}, map[string]any{}, "")
			if cerr == nil {
				t.Fatal("createItemChecked accepted an invalid title")
			}
			if cerr.status != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 — a deliberate refusal must not read as a server fault", cerr.status)
			}
			if !strings.Contains(cerr.message, tc.want) {
				t.Errorf("message = %q, want it to mention %q", cerr.message, tc.want)
			}
		})
	}

	// Control: a valid title still creates, so the arm above is not swallowing
	// the success path.
	if _, cerr := srv.createItemChecked(req, ws.ID, coll, schema,
		models.ItemCreate{Title: "Perfectly Fine"}, map[string]any{}, ""); cerr != nil {
		t.Fatalf("a valid title must still create: %d %s", cerr.status, cerr.message)
	}
}
