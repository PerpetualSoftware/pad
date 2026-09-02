package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2850 at the HTTP door — the door that was broken.
//
// The remote /mcp transport builds its field map in ingestFieldKVP with
// `dst[key] = val`, so every value reaches this handler as a STRING. Before
// the fix, validateFieldType then refused a string for a declared number or
// json field and the write 400'd: an MCP agent on that transport could not
// write those fields at all. The CLI and local stdio MCP were unaffected —
// they coerce by schema before the request is built — which is why this
// reproduced only against Pad Cloud.
//
// These assert the stored NATIVE TYPE, not that the request succeeded. A test
// that only checked for 201 would pass on an implementation that stored the
// string, which is the shape the reporter described.
func TestItemFieldsCoercedFromStringsAtTheHTTPDoor(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	sessionToken := bootstrapFirstUser(t, srv, "admin@example.com", "Admin")

	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces",
		map[string]string{"name": "Coercion Test"}, sessionToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create ws: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)

	// A collection whose schema declares the two types the bug made unwritable.
	schema := `{"fields":[{"key":"cost","type":"number"},{"key":"spec","type":"json"},{"key":"note","type":"text"}]}`
	rr = doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/collections",
		map[string]interface{}{"name": "Jobs", "schema": schema}, sessionToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var coll models.Collection
	parseJSON(t, rr, &coll)

	// Exactly what ingestFieldKVP produces: every value a string.
	rr = doRequestWithHeaders(srv, "POST",
		"/api/v1/workspaces/"+ws.Slug+"/collections/"+coll.Slug+"/items",
		map[string]interface{}{
			"title":  "from the remote mcp door",
			"fields": `{"cost":"42","spec":"[{\"name\":\"a\"}]","note":"42"}`,
		},
		map[string]string{"Authorization": "Bearer " + sessionToken},
	)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var created models.Item
	parseJSON(t, rr, &created)

	stored := map[string]any{}
	if err := json.Unmarshal([]byte(created.Fields), &stored); err != nil {
		t.Fatalf("stored fields are not JSON: %v (%s)", err, created.Fields)
	}

	if got, ok := stored["cost"].(float64); !ok || got != 42 {
		t.Fatalf("cost: want a JSON number, got %[1]T(%[1]v) — stored blob: %s", stored["cost"], created.Fields)
	}
	if _, ok := stored["spec"].([]any); !ok {
		t.Fatalf("spec: want a JSON array, got %[1]T(%[1]v) — stored blob: %s", stored["spec"], created.Fields)
	}
	// The text field holding "42" must STAY a string. Fixing the bug by
	// coercing anything that parses would silently retype real data.
	if got, ok := stored["note"].(string); !ok || got != "42" {
		t.Fatalf("note: want the string \"42\" untouched, got %[1]T(%[1]v)", stored["note"])
	}
}

// The same door on UPDATE, which is a separate call site and would not have
// been covered by the create test above (BUG-2850 wires eight sites).
func TestItemFieldsCoercedFromStringsOnUpdate(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	sessionToken := bootstrapFirstUser(t, srv, "admin@example.com", "Admin")

	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces",
		map[string]string{"name": "Coercion Update"}, sessionToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create ws: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)

	schema := `{"fields":[{"key":"cost","type":"number"}]}`
	rr = doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/collections",
		map[string]interface{}{"name": "Jobs", "schema": schema}, sessionToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var coll models.Collection
	parseJSON(t, rr, &coll)

	rr = doRequestWithHeaders(srv, "POST",
		"/api/v1/workspaces/"+ws.Slug+"/collections/"+coll.Slug+"/items",
		map[string]interface{}{"title": "job", "fields": `{}`},
		map[string]string{"Authorization": "Bearer " + sessionToken})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)

	rr = doRequestWithHeaders(srv, "PATCH",
		"/api/v1/workspaces/"+ws.Slug+"/items/"+item.Slug,
		map[string]interface{}{"fields": `{"cost":"99.5"}`},
		map[string]string{"Authorization": "Bearer " + sessionToken})
	if rr.Code != http.StatusOK {
		t.Fatalf("update: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var updated models.Item
	parseJSON(t, rr, &updated)

	stored := map[string]any{}
	if err := json.Unmarshal([]byte(updated.Fields), &stored); err != nil {
		t.Fatalf("stored fields are not JSON: %v (%s)", err, updated.Fields)
	}
	if got, ok := stored["cost"].(float64); !ok || got != 99.5 {
		t.Fatalf("cost: want a JSON number, got %[1]T(%[1]v) — stored blob: %s", stored["cost"], updated.Fields)
	}
}

// A value that will not coerce must still be REFUSED, with the validator's
// existing message. Coercion removes the cases where a correct value arrived
// in the wrong clothes; it must not start accepting wrong values.
func TestUncoercibleFieldValueStillRefused(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	sessionToken := bootstrapFirstUser(t, srv, "admin@example.com", "Admin")

	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces",
		map[string]string{"name": "Coercion Refusal"}, sessionToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create ws: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)

	schema := `{"fields":[{"key":"cost","type":"number"}]}`
	rr = doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/collections",
		map[string]interface{}{"name": "Jobs", "schema": schema}, sessionToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var coll models.Collection
	parseJSON(t, rr, &coll)

	rr = doRequestWithHeaders(srv, "POST",
		"/api/v1/workspaces/"+ws.Slug+"/collections/"+coll.Slug+"/items",
		map[string]interface{}{"title": "bad", "fields": `{"cost":"not-a-number"}`},
		map[string]string{"Authorization": "Bearer " + sessionToken})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an un-coercible number, got %d: %s", rr.Code, rr.Body.String())
	}
}
