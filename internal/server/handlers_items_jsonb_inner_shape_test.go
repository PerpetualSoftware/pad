package server

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// IDEA-1488 R1 codex P1.2: the `case '"'` branch in
// models.flexJSONToString previously accepted JSON-encoded strings
// without checking the inner content's shape. The hardened path
// validates that the inner JSON matches the expected start byte ('{'
// for fields/config/settings, '[' for tags). These tests pin the fix
// for ItemUpdate; the parallel view/collection tests live in
// handlers_views_collections_jsonb_test.go.
//
// Pre-fix behavior these tests would have failed against:
//   - PATCH item with `{"fields": "[]"}` → 200 (stored verbatim) — wrong
//   - PATCH item with `{"fields": "not json"}` → 200 — wrong
//   - PATCH item with `{"tags": "{}"}` → 200 — wrong
//   - PATCH item with `{"tags": "garbage"}` → 200 — wrong

func TestPatchItem_JSONEncodedStringInnerShapeValidated(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	createResp := doRequest(srv, "POST",
		"/api/v1/workspaces/"+slug+"/collections/tasks/items",
		map[string]interface{}{
			"title":  "shape fixture",
			"fields": `{"status":"open"}`,
		})
	if createResp.Code != http.StatusCreated {
		t.Fatalf("seed: expected 201, got %d: %s", createResp.Code, createResp.Body.String())
	}
	var seeded models.Item
	parseJSON(t, createResp, &seeded)

	patchPath := "/api/v1/workspaces/" + slug + "/items/" + seeded.Ref

	t.Run("fields as JSON-encoded array string returns 400", func(t *testing.T) {
		rr := doRequest(srv, "PATCH", patchPath, map[string]interface{}{
			"fields": `[]`, // wrong inner shape — array, not object
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for JSON-encoded array fields, got %d: %s", rr.Code, rr.Body.String())
		}
		if !bytes.Contains(rr.Body.Bytes(), []byte(`\"fields\" must be a JSON object`)) {
			t.Fatalf("response missing domain-level fields message: %s", rr.Body.String())
		}
	})

	t.Run("fields as JSON-encoded malformed-string returns 400", func(t *testing.T) {
		rr := doRequest(srv, "PATCH", patchPath, map[string]interface{}{
			"fields": `not json`,
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for malformed-string fields, got %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("tags as JSON-encoded object string returns 400", func(t *testing.T) {
		rr := doRequest(srv, "PATCH", patchPath, map[string]interface{}{
			"tags": `{}`, // wrong inner shape — object, not array
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for JSON-encoded object tags, got %d: %s", rr.Code, rr.Body.String())
		}
		if !bytes.Contains(rr.Body.Bytes(), []byte(`\"tags\" must be a JSON array`)) {
			t.Fatalf("response missing domain-level tags message: %s", rr.Body.String())
		}
	})

	t.Run("tags as JSON-encoded malformed-string returns 400", func(t *testing.T) {
		rr := doRequest(srv, "PATCH", patchPath, map[string]interface{}{
			"tags": `garbage`,
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for malformed-string tags, got %d: %s", rr.Code, rr.Body.String())
		}
	})

	// Positive controls — the well-shaped JSON-encoded strings the
	// fixed branch still accepts.
	t.Run("fields as JSON-encoded object string still works", func(t *testing.T) {
		rr := doRequest(srv, "PATCH", patchPath, map[string]interface{}{
			"fields": `{"status":"done"}`,
		})
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 for valid stringified fields, got %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("tags as JSON-encoded array string still works", func(t *testing.T) {
		rr := doRequest(srv, "PATCH", patchPath, map[string]interface{}{
			"tags": `["a","b"]`,
		})
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 for valid stringified tags, got %d: %s", rr.Code, rr.Body.String())
		}
	})
}

// TestCreateItem_JSONEncodedStringInnerShapeValidated is the codex R2
// P2 regression test. handleCreateItem was missed in the original
// IDEA-1488 work — it returned `invalid JSON: <wrapped>` instead of
// the clean sentinel-based 400 that PATCH and the view/collection
// POST/PATCH handlers do. These tests assert the POST path now
// matches the rest of the surface.
func TestCreateItem_JSONEncodedStringInnerShapeValidated(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	createPath := "/api/v1/workspaces/" + slug + "/collections/tasks/items"

	t.Run("fields as JSON-encoded array string returns sentinel 400", func(t *testing.T) {
		rr := doRequest(srv, "POST", createPath, map[string]interface{}{
			"title":  "p2-fields-array",
			"fields": `[]`,
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
		}
		body := rr.Body.Bytes()
		// Must be the domain-level sentinel message, NOT the
		// `invalid JSON: ...` wrapper from decodeJSON.
		if bytes.Contains(body, []byte("invalid JSON:")) {
			t.Fatalf("response still wraps with decodeJSON's invalid-JSON prefix: %s", body)
		}
		if !bytes.Contains(body, []byte(`\"fields\" must be a JSON object`)) {
			t.Fatalf("response missing domain-level fields message: %s", body)
		}
	})

	t.Run("fields as numeric returns sentinel 400", func(t *testing.T) {
		rr := doRequest(srv, "POST", createPath, map[string]interface{}{
			"title":  "p2-fields-num",
			"fields": 42,
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
		}
		if !bytes.Contains(rr.Body.Bytes(), []byte(`\"fields\" must be a JSON object`)) {
			t.Fatalf("response missing domain-level fields message: %s", rr.Body.String())
		}
	})

	t.Run("tags as JSON-encoded object string returns sentinel 400", func(t *testing.T) {
		rr := doRequest(srv, "POST", createPath, map[string]interface{}{
			"title": "p2-tags-obj",
			"tags":  `{}`,
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
		}
		body := rr.Body.Bytes()
		if bytes.Contains(body, []byte("invalid JSON:")) {
			t.Fatalf("response still wraps with decodeJSON's invalid-JSON prefix: %s", body)
		}
		if !bytes.Contains(body, []byte(`\"tags\" must be a JSON array`)) {
			t.Fatalf("response missing domain-level tags message: %s", body)
		}
	})

	t.Run("tags as nested-object returns sentinel 400", func(t *testing.T) {
		rr := doRequest(srv, "POST", createPath, map[string]interface{}{
			"title": "p2-tags-nested",
			"tags":  map[string]interface{}{"x": 1},
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
		}
		if !bytes.Contains(rr.Body.Bytes(), []byte(`\"tags\" must be a JSON array`)) {
			t.Fatalf("response missing domain-level tags message: %s", rr.Body.String())
		}
	})

	// Positive control: a valid POST still succeeds.
	t.Run("valid POST still works", func(t *testing.T) {
		rr := doRequest(srv, "POST", createPath, map[string]interface{}{
			"title":  "p2-ok",
			"fields": map[string]interface{}{"status": "open"},
			"tags":   []string{"a"},
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
		}
	})
}
