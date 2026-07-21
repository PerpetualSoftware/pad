package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// settingsLayout parses a collection's settings JSON and returns its layout.
// Postgres re-serializes the JSONB settings column (spaces / key order differ
// from the literal sent), so tests compare parsed values, not raw strings.
func settingsLayout(t *testing.T, raw string) string {
	t.Helper()
	var s models.CollectionSettings
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("parse settings %q: %v", raw, err)
	}
	return s.Layout
}

// BUG-2265: collection settings writes are optimistic-concurrency guarded when
// the caller round-trips expected_updated_at. These tests cover the handler
// boundary (400 on a malformed token, 409 on a stale token with the shared
// update_conflict envelope, 200 when the token matches) and confirm a rejected
// write does NOT land.

func TestUpdateCollection_OptimisticConcurrency(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	seed := func(t *testing.T, name string) models.Collection {
		t.Helper()
		createRR := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{
			"name":     name,
			"settings": `{"layout":"balanced"}`,
		})
		if createRR.Code != http.StatusCreated {
			t.Fatalf("seed create: expected 201, got %d: %s", createRR.Code, createRR.Body.String())
		}
		var coll models.Collection
		parseJSON(t, createRR, &coll)
		return coll
	}

	t.Run("malformed expected_updated_at is a 400", func(t *testing.T) {
		coll := seed(t, "OCC Malformed")
		rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/collections/"+coll.Slug, map[string]interface{}{
			"settings":            `{"layout":"content-primary"}`,
			"expected_updated_at": "not-a-timestamp",
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for malformed token, got %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("stale expected_updated_at is a 409 update_conflict and does not land", func(t *testing.T) {
		coll := seed(t, "OCC Stale")
		rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/collections/"+coll.Slug, map[string]interface{}{
			"settings":            `{"layout":"content-primary"}`,
			"expected_updated_at": "2000-01-01T00:00:00Z",
		})
		if rr.Code != http.StatusConflict {
			t.Fatalf("expected 409 for stale token, got %d: %s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "update_conflict") {
			t.Fatalf("expected update_conflict envelope, got: %s", rr.Body.String())
		}

		// The rejected write must NOT have landed.
		getRR := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/collections/"+coll.Slug, nil)
		var refetched models.Collection
		parseJSON(t, getRR, &refetched)
		if got := settingsLayout(t, refetched.Settings); got != "balanced" {
			t.Fatalf("expected layout unchanged (balanced) after a rejected conflict, got %q (%q)", got, refetched.Settings)
		}
	})

	t.Run("matching expected_updated_at is accepted", func(t *testing.T) {
		coll := seed(t, "OCC Match")
		token := coll.UpdatedAt.UTC().Format(time.RFC3339)
		rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/collections/"+coll.Slug, map[string]interface{}{
			"settings":            `{"layout":"content-primary"}`,
			"expected_updated_at": token,
		})
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 for matching token, got %d: %s", rr.Code, rr.Body.String())
		}
		var updated models.Collection
		parseJSON(t, rr, &updated)
		if got := settingsLayout(t, updated.Settings); got != "content-primary" {
			t.Fatalf("expected layout content-primary written, got %q (%q)", got, updated.Settings)
		}
	})

	// Codex round-2 P2: the 409's actual_updated_at must be FULL sub-second
	// precision so the client can round-trip it as the token on retry. A
	// truncated (second-precision) token would never match the row's real
	// sub-second updated_at, 409-looping forever.
	t.Run("409 actual_updated_at round-trips as a usable retry token", func(t *testing.T) {
		coll := seed(t, "OCC RoundTrip")
		// First update establishes a sub-second updated_at on the row.
		first := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/collections/"+coll.Slug, map[string]interface{}{
			"settings": `{"layout":"content-primary"}`,
		})
		if first.Code != http.StatusOK {
			t.Fatalf("first update: expected 200, got %d: %s", first.Code, first.Body.String())
		}
		var afterFirst models.Collection
		parseJSON(t, first, &afterFirst)

		// A stale token loses the race → 409 carrying the row's real token.
		conflictRR := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/collections/"+coll.Slug, map[string]interface{}{
			"settings":            `{"layout":"fields-primary"}`,
			"expected_updated_at": coll.UpdatedAt.UTC().Format(time.RFC3339),
		})
		if conflictRR.Code != http.StatusConflict {
			t.Fatalf("expected 409, got %d: %s", conflictRR.Code, conflictRR.Body.String())
		}
		var envelope struct {
			Error struct {
				Details struct {
					ActualUpdatedAt string `json:"actual_updated_at"`
				} `json:"details"`
			} `json:"error"`
		}
		if err := json.Unmarshal(conflictRR.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("parse 409 envelope: %v", err)
		}
		actual := envelope.Error.Details.ActualUpdatedAt
		if actual == "" {
			t.Fatalf("409 missing actual_updated_at: %s", conflictRR.Body.String())
		}
		// It must equal the row's current updated_at at full precision, not a
		// second-truncated version of it.
		if actual != afterFirst.UpdatedAt.UTC().Format(time.RFC3339Nano) {
			t.Fatalf("actual_updated_at %q not full-precision (want %q)", actual, afterFirst.UpdatedAt.UTC().Format(time.RFC3339Nano))
		}

		// Retrying with the returned token must now SUCCEED — proving it's a
		// usable token, not a truncated one that loops forever.
		retryRR := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/collections/"+coll.Slug, map[string]interface{}{
			"settings":            `{"layout":"fields-primary"}`,
			"expected_updated_at": actual,
		})
		if retryRR.Code != http.StatusOK {
			t.Fatalf("retry with returned token should succeed, got %d: %s", retryRR.Code, retryRR.Body.String())
		}
	})

	t.Run("omitting the token keeps last-write-wins (200)", func(t *testing.T) {
		coll := seed(t, "OCC NoToken")
		rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/collections/"+coll.Slug, map[string]interface{}{
			"settings": `{"layout":"fields-primary"}`,
		})
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 without a token, got %d: %s", rr.Code, rr.Body.String())
		}
	})
}

// TestUpdateCollection_PublishesCollectionUpdatedEvent covers BUG-2265 part 3:
// a successful collection update broadcasts a collection_updated event carrying
// the workspace + collection slug so sibling ItemDetails / pages can refresh
// their independent snapshot.
func TestUpdateCollection_PublishesCollectionUpdatedEvent(t *testing.T) {
	srv := testServerWithEvents(t)
	slug := createWSWithCollections(t, srv)
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("workspace: %v", err)
	}

	createRR := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{
		"name": "Broadcast Me",
	})
	if createRR.Code != http.StatusCreated {
		t.Fatalf("seed create: expected 201, got %d: %s", createRR.Code, createRR.Body.String())
	}
	var coll models.Collection
	parseJSON(t, createRR, &coll)

	ch := srv.events.Subscribe(ws.ID)
	defer srv.events.Unsubscribe(ch)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/collections/"+coll.Slug, map[string]interface{}{
		"settings": `{"quick_actions":[{"label":"Go","prompt":"/pad go","scope":"item"}]}`,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("update: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-ch:
			if event.Type == events.CollectionUpdated {
				if event.WorkspaceID != ws.ID {
					t.Fatalf("collection_updated workspace=%q want %q", event.WorkspaceID, ws.ID)
				}
				if event.Collection != coll.Slug {
					t.Fatalf("collection_updated collection=%q want %q", event.Collection, coll.Slug)
				}
				if event.CollectionID != coll.ID {
					t.Fatalf("collection_updated collection_id=%q want %q", event.CollectionID, coll.ID)
				}
				if event.NewSlug != "" {
					t.Fatalf("settings-only update should not carry new_slug, got %q", event.NewSlug)
				}
				// Sanitized: no owner identity/source leaked to (item-grant) guests.
				if event.Actor != "" || event.ActorName != "" || event.Source != "" {
					t.Fatalf("collection_updated leaked actor metadata: actor=%q name=%q source=%q",
						event.Actor, event.ActorName, event.Source)
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for collection_updated event")
		}
	}
}

// TestUpdateCollection_RenameEventCarriesNewSlug covers BUG-2265 Codex-round
// P2: a rename broadcasts collection_updated routed by the OLD slug and
// carrying the NEW slug so remote tabs can re-target instead of hitting the
// dead old slug.
func TestUpdateCollection_RenameEventCarriesNewSlug(t *testing.T) {
	srv := testServerWithEvents(t)
	slug := createWSWithCollections(t, srv)
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("workspace: %v", err)
	}

	createRR := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{
		"name": "Old Name",
	})
	if createRR.Code != http.StatusCreated {
		t.Fatalf("seed create: expected 201, got %d: %s", createRR.Code, createRR.Body.String())
	}
	var coll models.Collection
	parseJSON(t, createRR, &coll)
	oldSlug := coll.Slug

	ch := srv.events.Subscribe(ws.ID)
	defer srv.events.Unsubscribe(ch)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/collections/"+oldSlug, map[string]interface{}{
		"name": "Brand New Name",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("rename: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var updated models.Collection
	parseJSON(t, rr, &updated)
	if updated.Slug == oldSlug {
		t.Fatalf("expected the rename to change the slug, still %q", oldSlug)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-ch:
			if event.Type == events.CollectionUpdated {
				if event.Collection != oldSlug {
					t.Fatalf("rename event should route by OLD slug, got %q want %q", event.Collection, oldSlug)
				}
				if event.CollectionID != updated.ID {
					t.Fatalf("rename event collection_id=%q want %q (stable identity)", event.CollectionID, updated.ID)
				}
				if event.NewSlug != updated.Slug {
					t.Fatalf("rename event new_slug=%q want %q", event.NewSlug, updated.Slug)
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for rename collection_updated event")
		}
	}
}

// TestUpdateCollection_MigrationSetsItemsChanged covers BUG-2265 Codex round
// 6/7: a schema change carrying migrations sets the SANITIZED items_changed
// flag on collection_updated — keyed on whether a migration was REQUESTED, not
// how many rows changed (round 7 P1: an affected-row count would let an
// item-grant subscriber infer that hidden items matched). The event carries the
// STABLE CollectionID for id-based matching. A settings-only update leaves
// items_changed false. No separate items_bulk_updated is emitted.
func TestUpdateCollection_MigrationSetsItemsChanged(t *testing.T) {
	srv := testServerWithEvents(t)
	slug := createWSWithCollections(t, srv)
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("workspace: %v", err)
	}
	coll, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Tasks",
		Slug:   "tasks-migrate-evt",
		Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["a","b"]}]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	if _, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "T", Fields: `{"status":"a"}`}); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	// awaitCollectionUpdated returns the next collection_updated event (or fails).
	awaitCollectionUpdated := func(t *testing.T, ch <-chan events.Event) events.Event {
		t.Helper()
		deadline := time.After(2 * time.Second)
		for {
			select {
			case event := <-ch:
				if event.Type == events.CollectionUpdated {
					return event
				}
				if event.Type == events.ItemsBulkUpdated {
					t.Fatalf("collection update must not emit items_bulk_updated (folded into collection_updated)")
				}
			case <-deadline:
				t.Fatal("timed out waiting for collection_updated")
			}
		}
	}

	t.Run("migration touching an item sets items_changed", func(t *testing.T) {
		ch := srv.events.Subscribe(ws.ID)
		defer srv.events.Unsubscribe(ch)

		rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/collections/"+coll.Slug, map[string]interface{}{
			"schema": `{"fields":[{"key":"status","label":"Status","type":"select","options":["c","b"]}]}`,
			"migrations": []map[string]interface{}{
				{"field": "status", "rename_options": map[string]string{"a": "c"}},
			},
		})
		if rr.Code != http.StatusOK {
			t.Fatalf("migration update: expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		event := awaitCollectionUpdated(t, ch)
		if event.Collection != coll.Slug {
			t.Fatalf("collection_updated collection=%q want %q", event.Collection, coll.Slug)
		}
		if event.CollectionID != coll.ID {
			t.Fatalf("collection_updated collection_id=%q want %q", event.CollectionID, coll.ID)
		}
		if !event.ItemsChanged {
			t.Fatalf("migration collection_updated must set items_changed")
		}
		// Still sanitized — no actor/source/count leaked to item-grant subscribers.
		if event.Actor != "" || event.Source != "" || event.Count != 0 {
			t.Fatalf("migration event leaked non-sanitized fields: actor=%q source=%q count=%d",
				event.Actor, event.Source, event.Count)
		}
	})

	t.Run("migration matching ZERO items still sets items_changed (no row-count leak)", func(t *testing.T) {
		ch := srv.events.Subscribe(ws.ID)
		defer srv.events.Unsubscribe(ch)

		// Rename an option value that NO item currently has → 0 rows migrated.
		// items_changed must STILL be true (keyed on request, not row count), so
		// an item-grant subscriber can't infer whether hidden items matched.
		rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/collections/"+coll.Slug, map[string]interface{}{
			"schema": `{"fields":[{"key":"status","label":"Status","type":"select","options":["b","z"]}]}`,
			"migrations": []map[string]interface{}{
				{"field": "status", "rename_options": map[string]string{"nonexistent": "z"}},
			},
		})
		if rr.Code != http.StatusOK {
			t.Fatalf("zero-match migration: expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		event := awaitCollectionUpdated(t, ch)
		if !event.ItemsChanged {
			t.Fatalf("a REQUESTED migration must set items_changed even when 0 rows matched")
		}
	})

	t.Run("settings-only update leaves items_changed false", func(t *testing.T) {
		ch := srv.events.Subscribe(ws.ID)
		defer srv.events.Unsubscribe(ch)

		rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/collections/"+coll.Slug, map[string]interface{}{
			"settings": `{"layout":"content-primary"}`,
		})
		if rr.Code != http.StatusOK {
			t.Fatalf("settings update: expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		event := awaitCollectionUpdated(t, ch)
		if event.ItemsChanged {
			t.Fatalf("settings-only update must not set items_changed")
		}
	})
}
