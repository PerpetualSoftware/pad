package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-1912 (IDEA-1746 stage 1): a collection schema field keyed exactly
// "parent" or "plan" makes schemaHasField (handlers_items.go:2190) return
// true for that key, which makes the parent-link extraction sites
// (handlers_items.go:584, 851, 2147) silently skip fields-JSON extraction,
// disabling subtask linking with no error anywhere. These tests cover the
// create/update guard added in handlers_collections.go.

// TestCreateCollection_RejectsReservedFieldKeys covers the create path:
// "parent" and "plan" are rejected, everything else still works.
func TestCreateCollection_RejectsReservedFieldKeys(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	createPath := "/api/v1/workspaces/" + slug + "/collections"

	t.Run("schema field keyed parent is rejected", func(t *testing.T) {
		rr := doRequest(srv, "POST", createPath, map[string]interface{}{
			"name":   "Projects",
			"schema": `{"fields":[{"key":"parent","label":"Parent","type":"text"}]}`,
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for schema field keyed 'parent', got %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("schema field keyed plan is rejected", func(t *testing.T) {
		rr := doRequest(srv, "POST", createPath, map[string]interface{}{
			"name":   "Initiatives",
			"schema": `{"fields":[{"key":"plan","label":"Plan","type":"text"}]}`,
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for schema field keyed 'plan', got %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("differently-cased key is NOT rejected (server match is exact and case-sensitive, mirroring schemaHasField)", func(t *testing.T) {
		// This is intentional, not an oversight: schemaHasField
		// (handlers_items.go:2190) does a plain f.Key == key comparison
		// with no case-folding, so only the exact-lowercase keys
		// "parent"/"plan" ever trip the silent-skip bug this guard
		// exists to prevent. The web layer's RESERVED_FIELD_KEYS check
		// lowercases before comparing (stricter than the server) — that
		// asymmetry is fine; a client stricter than the server is safe.
		// Do not "fix" this into a case-insensitive server-side match.
		rr := doRequest(srv, "POST", createPath, map[string]interface{}{
			"name":   "CasedKeyColl",
			"schema": `{"fields":[{"key":"Parent","label":"Parent","type":"text"}]}`,
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("expected 201 for differently-cased key 'Parent', got %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("unrelated key still passes", func(t *testing.T) {
		rr := doRequest(srv, "POST", createPath, map[string]interface{}{
			"name":   "Widgets",
			"schema": `{"fields":[{"key":"priority","label":"Priority","type":"select","options":["low","high"]}]}`,
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("expected 201 for unrelated schema key, got %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("malformed schema JSON returns 400", func(t *testing.T) {
		rr := doRequest(srv, "POST", createPath, map[string]interface{}{
			"name":   "Broken",
			"schema": `not json`,
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for malformed schema JSON, got %d: %s", rr.Code, rr.Body.String())
		}
	})
}

// TestUpdateCollection_ReservedFieldKeys covers the update path: a NEWLY
// added "parent"/"plan" field is rejected, but a key already present in
// the collection's prior schema is grandfathered in.
func TestUpdateCollection_ReservedFieldKeys(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	t.Run("adding a new parent-keyed field is rejected", func(t *testing.T) {
		createRR := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{
			"name":   "Alpha",
			"schema": `{"fields":[{"key":"priority","label":"Priority","type":"text"}]}`,
		})
		if createRR.Code != http.StatusCreated {
			t.Fatalf("seed create: expected 201, got %d: %s", createRR.Code, createRR.Body.String())
		}
		var coll models.Collection
		parseJSON(t, createRR, &coll)

		rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/collections/"+coll.Slug, map[string]interface{}{
			"schema": `{"fields":[{"key":"priority","label":"Priority","type":"text"},{"key":"parent","label":"Parent","type":"text"}]}`,
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for newly-added 'parent' key, got %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("a parent-keyed field already in the prior schema is grandfathered", func(t *testing.T) {
		// Simulate a pre-existing workspace whose schema already has the
		// reserved key, via the store directly — bypassing the new
		// create-path guard, the same way an old on-disk workspace or an
		// imported bundle would already have this schema before the
		// guard existed.
		ws, err := srv.store.GetWorkspaceBySlug(slug)
		if err != nil || ws == nil {
			t.Fatalf("GetWorkspaceBySlug(%q): %v", slug, err)
		}
		coll, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
			Name:   "Legacy",
			Schema: `{"fields":[{"key":"parent","label":"Parent","type":"text"}]}`,
		})
		if err != nil {
			t.Fatalf("seed legacy collection via store: %v", err)
		}

		rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/collections/"+coll.Slug, map[string]interface{}{
			"schema": `{"fields":[{"key":"parent","label":"Parent","type":"text"},{"key":"priority","label":"Priority","type":"text"}]}`,
		})
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 for grandfathered 'parent' key, got %d: %s", rr.Code, rr.Body.String())
		}
		var updated models.Collection
		parseJSON(t, rr, &updated)
		var schema models.CollectionSchema
		if err := json.Unmarshal([]byte(updated.Schema), &schema); err != nil {
			t.Fatalf("parse updated schema: %v", err)
		}
		found := false
		for _, f := range schema.Fields {
			if f.Key == "parent" {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected grandfathered 'parent' field to survive the update, schema: %s", updated.Schema)
		}
	})

	t.Run("unrelated schema change on update still passes", func(t *testing.T) {
		createRR := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{
			"name":   "Beta",
			"schema": `{"fields":[]}`,
		})
		if createRR.Code != http.StatusCreated {
			t.Fatalf("seed create: expected 201, got %d: %s", createRR.Code, createRR.Body.String())
		}
		var coll models.Collection
		parseJSON(t, createRR, &coll)

		rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/collections/"+coll.Slug, map[string]interface{}{
			"schema": `{"fields":[{"key":"priority","label":"Priority","type":"text"}]}`,
		})
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 for unrelated schema update, got %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("empty-string schema is rejected rather than stored verbatim", func(t *testing.T) {
		// Regression guard: input.Schema is a non-nil *string pointing at
		// "" here (the client explicitly sent `"schema": ""`), which is
		// distinct from omitting the field entirely (nil, see the sibling
		// subtest below). "" is not valid JSON, so it must be rejected up
		// front by this validation block rather than flowing through to
		// store.UpdateCollection, which would write it verbatim
		// (store/collections.go) and break every later item-create
		// against the collection with a 500 at handlers_items.go:565
		// ("Failed to parse collection schema") instead of failing the
		// mutation that caused it.
		createRR := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{
			"name":   "Gamma",
			"schema": `{"fields":[]}`,
		})
		if createRR.Code != http.StatusCreated {
			t.Fatalf("seed create: expected 201, got %d: %s", createRR.Code, createRR.Body.String())
		}
		var coll models.Collection
		parseJSON(t, createRR, &coll)

		rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/collections/"+coll.Slug, map[string]interface{}{
			"schema": "",
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for empty-string schema, got %d: %s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "Invalid schema JSON") {
			t.Fatalf("expected 'Invalid schema JSON' message, got: %s", rr.Body.String())
		}

		// Confirm the rejected PATCH didn't get partially applied: the
		// collection's schema must still be the original, valid one.
		getRR := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/collections/"+coll.Slug, nil)
		var refetched models.Collection
		parseJSON(t, getRR, &refetched)
		if refetched.Schema != `{"fields":[]}` {
			t.Fatalf("expected schema to remain %q after rejected update, got %q", `{"fields":[]}`, refetched.Schema)
		}
	})

	t.Run("omitting schema entirely (nil) leaves it untouched", func(t *testing.T) {
		createRR := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{
			"name":   "Delta",
			"schema": `{"fields":[{"key":"priority","label":"Priority","type":"text"}]}`,
		})
		if createRR.Code != http.StatusCreated {
			t.Fatalf("seed create: expected 201, got %d: %s", createRR.Code, createRR.Body.String())
		}
		var coll models.Collection
		parseJSON(t, createRR, &coll)

		// No "schema" key at all in the PATCH body — input.Schema stays
		// nil, so the validation block (and store.UpdateCollection's own
		// `if input.Schema != nil` SET clause) is skipped entirely.
		rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/collections/"+coll.Slug, map[string]interface{}{
			"description": "updated description, no schema field present",
		})
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 for PATCH omitting schema, got %d: %s", rr.Code, rr.Body.String())
		}
		var updated models.Collection
		parseJSON(t, rr, &updated)
		if updated.Schema != coll.Schema {
			t.Fatalf("expected schema unchanged when omitted from PATCH, got %q, want %q", updated.Schema, coll.Schema)
		}
	})
}
