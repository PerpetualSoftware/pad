package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// BUG-3831 — two residuals of BUG-3049's field-level patch in bulkFieldUpdate.
//
//  1. A carried value whose only change is CoerceFields re-typing it (stored
//     "3" on a number field becoming 3) stayed in the patch, which is built
//     from the pre-lock read with no seq guard, so a concurrent edit to that
//     key was overwritten by the coerced OLD value.
//  2. The under-lock auto-date re-decision kept the concurrent value only when
//     it was a non-empty STRING, so a concurrent non-string value was replaced
//     by today's stamp.
//
// Both use the BUG-3049 interleaving, made deterministic: the door is handed a
// STALE snapshot read before a concurrent write lands. Each runs on SQLite and,
// under make test-pg, on Postgres.

func bug3831Backends(t *testing.T) map[string]func(*testing.T) *Server {
	t.Helper()
	newSrv := func(s *store.Store) func(*testing.T) *Server {
		return func(t *testing.T) *Server {
			srv := New(s)
			t.Cleanup(func() { srv.Stop() })
			return srv
		}
	}
	return map[string]func(*testing.T) *Server{
		"sqlite":   func(t *testing.T) *Server { return newSrv(storetest.NewSQLite(t))(t) },
		"postgres": func(t *testing.T) *Server { return newSrv(storetest.NewPostgres(t))(t) }, // skips without PAD_TEST_POSTGRES_URL
	}
}

// bug3831Item makes a collection with a terminal status, a number field and
// the end_date autoPopulateDates stamps, and one item in it.
func bug3831Item(t *testing.T, srv *Server) (wsID string, item models.Item) {
	t.Helper()
	wsSlug := createWSWithCollections(t, srv)
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/collections", map[string]interface{}{
		"name": "Pointed",
		"schema": `{"fields":[
			{"key":"status","type":"select","options":["planning","active","completed"],"terminal_options":["completed"],"default":"planning","required":true},
			{"key":"points","type":"number"},
			{"key":"end_date","type":"date"}
		]}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var coll models.Collection
	parseJSON(t, rr, &coll)
	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/collections/"+coll.Slug+"/items", map[string]interface{}{
		"title":  "Pointed item",
		"fields": `{"status":"planning"}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	parseJSON(t, rr, &item)
	ws, err := srv.store.GetWorkspaceBySlug(wsSlug)
	if err != nil || ws == nil {
		t.Fatalf("load workspace: %v", err)
	}
	return ws.ID, item
}

func TestBUG3831_CoercionOnlyKeyDoesNotOverwriteAConcurrentEdit(t *testing.T) {
	for name, mk := range bug3831Backends(t) {
		t.Run(name, func(t *testing.T) {
			srv := mk(t)
			wsID, item := bug3831Item(t, srv)

			// A legacy, non-canonical stored value: the string "3" on a number
			// field, written below the coercing doors.
			if _, err := srv.store.UpdateItem(item.ID, models.ItemUpdate{FieldsPatch: map[string]any{"points": "3"}}); err != nil {
				t.Fatalf("seed stored \"3\": %v", err)
			}
			stale, err := srv.store.GetItem(item.ID)
			if err != nil || stale == nil {
				t.Fatalf("load snapshot: %v", err)
			}
			if got := decodeItemFields(t, stale.Fields)["points"]; got != "3" {
				t.Fatalf("premise failed: the snapshot's points is %#v, want the string \"3\"", got)
			}

			// The concurrent edit, after the bulk handler's read.
			if _, err := srv.store.UpdateItem(item.ID, models.ItemUpdate{FieldsPatch: map[string]any{"points": float64(5)}}); err != nil {
				t.Fatalf("concurrent edit: %v", err)
			}
			if got := decodeItemFields(t, mustGetItemFields(t, srv, item.ID))["points"]; got != float64(5) {
				t.Fatalf("premise failed: the concurrent edit did not land, points=%#v", got)
			}

			req := httptest.NewRequest("POST", "/api/v1/workspaces/x/items/bulk", nil)
			var dropped []string
			updated, opErr := srv.bulkFieldUpdate(req, wsID, stale, map[string]any{"status": "active"},
				true, nil, "user", "web", "", &dropped)
			if opErr != nil {
				t.Fatalf("bulkFieldUpdate: %v", opErr.message)
			}
			fields := decodeItemFields(t, updated.Fields)
			if fields["status"] != "active" {
				t.Fatalf("the bulk op did not apply its own change: status=%v", fields["status"])
			}
			if fields["points"] != float64(5) {
				t.Errorf("BUG-3831: a coercion-only carried key overwrote a concurrent edit: points=%#v want 5", fields["points"])
			}
		})
	}
}

func TestBUG3831_AutoDateKeepsAConcurrentNonStringValue(t *testing.T) {
	for name, mk := range bug3831Backends(t) {
		t.Run(name, func(t *testing.T) {
			srv := mk(t)
			wsID, item := bug3831Item(t, srv)

			stale, err := srv.store.GetItem(item.ID)
			if err != nil || stale == nil {
				t.Fatalf("load snapshot: %v", err)
			}
			if got := decodeItemFields(t, stale.Fields)["end_date"]; got != nil && got != "" {
				t.Fatalf("premise failed: the snapshot already has an end_date: %#v", got)
			}

			// The concurrent writer leaves a NON-STRING end_date (a number,
			// written below the validating doors).
			const concurrent = float64(20260101)
			if _, err := srv.store.UpdateItem(item.ID, models.ItemUpdate{FieldsPatch: map[string]any{"end_date": concurrent}}); err != nil {
				t.Fatalf("concurrent edit: %v", err)
			}
			if got := decodeItemFields(t, mustGetItemFields(t, srv, item.ID))["end_date"]; got != concurrent {
				t.Fatalf("premise failed: the concurrent value did not land, end_date=%#v", got)
			}

			req := httptest.NewRequest("POST", "/api/v1/workspaces/x/items/bulk", nil)
			var dropped []string
			updated, opErr := srv.bulkFieldUpdate(req, wsID, stale, map[string]any{"status": "completed"},
				true, nil, "user", "web", "", &dropped)
			if opErr != nil {
				t.Fatalf("bulkFieldUpdate: %v", opErr.message)
			}
			fields := decodeItemFields(t, updated.Fields)
			if fields["status"] != "completed" {
				t.Fatalf("the bulk op did not apply its own change: status=%v", fields["status"])
			}
			if fields["end_date"] != concurrent {
				t.Errorf("BUG-3831: the auto-date stamp replaced a concurrent non-string value: end_date=%#v want %v", fields["end_date"], concurrent)
			}
		})
	}
}

// The over-drop control: a key the CALLER sets is never dropped, and a stored
// value that is already canonical is untouched.
func TestDropCoercionOnlyKeys(t *testing.T) {
	stored := map[string]any{"points": "3", "status": "planning", "size": float64(2)}
	coerced := map[string]any{"points": float64(3), "status": "planning", "size": float64(2)}
	patch := map[string]any{"points": float64(3), "status": "active", "size": float64(4)}
	dropCoercionOnlyKeys(patch, stored, coerced, map[string]any{"status": "active"})
	if _, ok := patch["points"]; ok {
		t.Errorf("points (coercion-only) should be dropped: %#v", patch)
	}
	if patch["status"] != "active" {
		t.Errorf("status (set by the caller) must stay: %#v", patch)
	}
	if patch["size"] != float64(4) {
		t.Errorf("size (a real change) must stay: %#v", patch)
	}

	setByCaller := map[string]any{"points": float64(3)}
	dropCoercionOnlyKeys(setByCaller, stored, coerced, map[string]any{"points": "3"})
	if setByCaller["points"] != float64(3) {
		t.Errorf("a key the caller set must never be dropped: %#v", setByCaller)
	}
}
