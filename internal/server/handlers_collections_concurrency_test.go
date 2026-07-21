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
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for collection_updated event")
		}
	}
}
