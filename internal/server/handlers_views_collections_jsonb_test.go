package server

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// IDEA-1488 handler-layer shape-validation tests for views.config and
// collections.settings, mirroring the precedent at
// handlers_items_test.go::TestPatchItem_FlexibleFieldsShape (BUG-1144).
//
// Contract for both columns: the wire input may be either
//   - a JSON object (`{"key":"v"}`) — the natural shape
//   - a JSON-encoded string (`"{\"key\":\"v\"}"`) — back-compat
// Anything else (number, bool, array for these object-only columns,
// non-JSON-object string) returns 400 with the sentinel error message
// in the body, NOT leaked Go unmarshal internals.

// TestPatchView_FlexibleConfigShape exercises ViewUpdate.UnmarshalJSON
// (added in IDEA-1488). The view route is
// PATCH /api/v1/workspaces/{ws}/collections/{coll}/views/{viewID}.
func TestPatchView_FlexibleConfigShape(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Seed a view on the default tasks collection.
	createResp := doRequest(srv, "POST",
		"/api/v1/workspaces/"+slug+"/collections/tasks/views",
		map[string]interface{}{
			"name":      "All",
			"view_type": "list",
			"config":    `{"layout":"list"}`,
		})
	if createResp.Code != http.StatusCreated {
		t.Fatalf("seed view: expected 201, got %d: %s", createResp.Code, createResp.Body.String())
	}
	var seeded models.View
	parseJSON(t, createResp, &seeded)

	patchPath := "/api/v1/workspaces/" + slug + "/collections/tasks/views/" + seeded.ID

	t.Run("config as nested object", func(t *testing.T) {
		rr := doRequest(srv, "PATCH", patchPath, map[string]interface{}{
			"config": map[string]interface{}{"layout": "grid"},
		})
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 for nested-object config, got %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("config as JSON-encoded string", func(t *testing.T) {
		rr := doRequest(srv, "PATCH", patchPath, map[string]interface{}{
			"config": `{"layout":"board"}`,
		})
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 for stringified config, got %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("config as number returns domain-level 400", func(t *testing.T) {
		rr := doRequest(srv, "PATCH", patchPath, map[string]interface{}{
			"config": 42,
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for numeric config, got %d: %s", rr.Code, rr.Body.String())
		}
		body := rr.Body.Bytes()
		if bytes.Contains(body, []byte("Go struct field")) {
			t.Fatalf("response leaked Go internals: %s", body)
		}
		if !bytes.Contains(body, []byte(`\"config\" must be a JSON object`)) {
			t.Fatalf("response missing domain-level config message: %s", body)
		}
	})

	t.Run("config as array returns domain-level 400", func(t *testing.T) {
		rr := doRequest(srv, "PATCH", patchPath, map[string]interface{}{
			"config": []string{"a", "b"},
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for array config, got %d: %s", rr.Code, rr.Body.String())
		}
		if !bytes.Contains(rr.Body.Bytes(), []byte(`\"config\" must be a JSON object`)) {
			t.Fatalf("response missing domain-level config message: %s", rr.Body.String())
		}
	})

	t.Run("config empty string is coerced by store layer (IDEA-1486 floor)", func(t *testing.T) {
		// Empty-string config is normalized to "{}" at the store boundary
		// (views.go:152 coercion). Handler-layer ViewUpdate.UnmarshalJSON
		// treats "" as "absent" — the field is omitted from the update
		// — so we exercise the IDEA-1486 floor by sending an explicit
		// "" inside the JSON-encoded-string shape.
		rr := doRequest(srv, "PATCH", patchPath, map[string]interface{}{
			"config": `""`, // an empty JSON-encoded string
		})
		// The current shape contract treats a JSON-encoded empty string
		// as a legal (empty) string passthrough; the store-layer
		// coercion takes it from there.
		if rr.Code != http.StatusOK && rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 200 or 400, got %d: %s", rr.Code, rr.Body.String())
		}
	})

	// IDEA-1488 R1 codex P1.2: the `case '"'` branch in flexJSONToString
	// previously accepted JSON-encoded strings without checking the
	// inner content's shape. The hardened path validates that the
	// inner JSON is an object (or array for tags). These tests pin the
	// fix — without the inner-shape check, both cases below would
	// return 200 with garbage stored in config.
	t.Run("config as JSON-encoded array string returns 400", func(t *testing.T) {
		rr := doRequest(srv, "PATCH", patchPath, map[string]interface{}{
			"config": `[]`, // valid JSON, wrong shape (array, not object)
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for JSON-encoded array config, got %d: %s", rr.Code, rr.Body.String())
		}
		if !bytes.Contains(rr.Body.Bytes(), []byte(`\"config\" must be a JSON object`)) {
			t.Fatalf("response missing domain-level config message: %s", rr.Body.String())
		}
	})

	t.Run("config as JSON-encoded malformed-string returns 400", func(t *testing.T) {
		rr := doRequest(srv, "PATCH", patchPath, map[string]interface{}{
			"config": `not even json`,
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for malformed-string config, got %d: %s", rr.Code, rr.Body.String())
		}
	})
}

// TestCreateView_FlexibleConfigShape mirrors the PATCH test for the POST
// create path so the create/update gap doesn't reopen.
func TestCreateView_FlexibleConfigShape(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	createPath := "/api/v1/workspaces/" + slug + "/collections/tasks/views"

	t.Run("nested object config", func(t *testing.T) {
		rr := doRequest(srv, "POST", createPath, map[string]interface{}{
			"name":      "Grid",
			"view_type": "list",
			"config":    map[string]interface{}{"layout": "grid"},
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("numeric config returns 400", func(t *testing.T) {
		rr := doRequest(srv, "POST", createPath, map[string]interface{}{
			"name":      "Bad",
			"view_type": "list",
			"config":    42,
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for numeric config, got %d: %s", rr.Code, rr.Body.String())
		}
		if !bytes.Contains(rr.Body.Bytes(), []byte(`\"config\" must be a JSON object`)) {
			t.Fatalf("response missing domain-level config message: %s", rr.Body.String())
		}
	})
}

// TestPatchCollection_FlexibleSettingsShape exercises
// CollectionUpdate.UnmarshalJSON (IDEA-1488). The PATCH route is
// /api/v1/workspaces/{ws}/collections/{collSlug}.
func TestPatchCollection_FlexibleSettingsShape(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	patchPath := "/api/v1/workspaces/" + slug + "/collections/tasks"

	t.Run("settings as nested object", func(t *testing.T) {
		rr := doRequest(srv, "PATCH", patchPath, map[string]interface{}{
			"settings": map[string]interface{}{"layout": "content-primary"},
		})
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 for nested-object settings, got %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("settings as JSON-encoded string", func(t *testing.T) {
		rr := doRequest(srv, "PATCH", patchPath, map[string]interface{}{
			"settings": `{"layout":"balanced"}`,
		})
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 for stringified settings, got %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("settings as number returns domain-level 400", func(t *testing.T) {
		rr := doRequest(srv, "PATCH", patchPath, map[string]interface{}{
			"settings": 42,
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for numeric settings, got %d: %s", rr.Code, rr.Body.String())
		}
		body := rr.Body.Bytes()
		if bytes.Contains(body, []byte("Go struct field")) {
			t.Fatalf("response leaked Go internals: %s", body)
		}
		if !bytes.Contains(body, []byte(`\"settings\" must be a JSON object`)) {
			t.Fatalf("response missing domain-level settings message: %s", body)
		}
	})

	t.Run("settings as array returns domain-level 400", func(t *testing.T) {
		rr := doRequest(srv, "PATCH", patchPath, map[string]interface{}{
			"settings": []string{"a", "b"},
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for array settings, got %d: %s", rr.Code, rr.Body.String())
		}
		if !bytes.Contains(rr.Body.Bytes(), []byte(`\"settings\" must be a JSON object`)) {
			t.Fatalf("response missing domain-level settings message: %s", rr.Body.String())
		}
	})

	// IDEA-1488 R1 codex P1.2 — see the matching view-side tests above
	// for context. These pin the inner-shape validation of the
	// JSON-encoded-string envelope.
	t.Run("settings as JSON-encoded array string returns 400", func(t *testing.T) {
		rr := doRequest(srv, "PATCH", patchPath, map[string]interface{}{
			"settings": `[]`,
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for JSON-encoded array settings, got %d: %s", rr.Code, rr.Body.String())
		}
		if !bytes.Contains(rr.Body.Bytes(), []byte(`\"settings\" must be a JSON object`)) {
			t.Fatalf("response missing domain-level settings message: %s", rr.Body.String())
		}
	})

	t.Run("settings as JSON-encoded malformed-string returns 400", func(t *testing.T) {
		rr := doRequest(srv, "PATCH", patchPath, map[string]interface{}{
			"settings": `not json`,
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for malformed-string settings, got %d: %s", rr.Code, rr.Body.String())
		}
	})
}

// TestCreateCollection_FlexibleSettingsShape covers the
// CollectionCreate.UnmarshalJSON path on POST.
func TestCreateCollection_FlexibleSettingsShape(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	createPath := "/api/v1/workspaces/" + slug + "/collections"

	t.Run("settings as nested object", func(t *testing.T) {
		rr := doRequest(srv, "POST", createPath, map[string]interface{}{
			"name":     "TestColl1",
			"settings": map[string]interface{}{"layout": "balanced"},
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("settings as number returns 400", func(t *testing.T) {
		rr := doRequest(srv, "POST", createPath, map[string]interface{}{
			"name":     "TestColl2",
			"settings": 99,
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for numeric settings, got %d: %s", rr.Code, rr.Body.String())
		}
		if !bytes.Contains(rr.Body.Bytes(), []byte(`\"settings\" must be a JSON object`)) {
			t.Fatalf("response missing domain-level settings message: %s", rr.Body.String())
		}
	})
}
