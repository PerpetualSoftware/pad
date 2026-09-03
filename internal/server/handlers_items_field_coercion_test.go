package server

import (
	"encoding/json"
	"net/http"
	"strings"
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

// fields_patch is a THIRD call site (the field-level merge from IDEA-1480),
// separate from the full-fields update above. CONVE-19: wiring is a claim, so
// each site that threads CoerceFields needs a test that fails if that site
// alone is missed — this one caught nothing when written, which is the point:
// it exists so a future edit that drops the call here is red.
func TestItemFieldsCoercedOnFieldsPatch(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	sessionToken := bootstrapFirstUser(t, srv, "admin@example.com", "Admin")

	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces",
		map[string]string{"name": "Coercion Patch"}, sessionToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create ws: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)

	schema := `{"fields":[{"key":"cost","type":"number"},{"key":"note","type":"text"}]}`
	rr = doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/collections",
		map[string]interface{}{"name": "Jobs", "schema": schema}, sessionToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var coll models.Collection
	parseJSON(t, rr, &coll)

	rr = doRequestWithHeaders(srv, "POST",
		"/api/v1/workspaces/"+ws.Slug+"/collections/"+coll.Slug+"/items",
		map[string]interface{}{"title": "job", "fields": `{"note":"keep me"}`},
		map[string]string{"Authorization": "Bearer " + sessionToken})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)

	rr = doRequestWithHeaders(srv, "PATCH",
		"/api/v1/workspaces/"+ws.Slug+"/items/"+item.Slug,
		map[string]interface{}{"fields_patch": map[string]any{"cost": "7"}},
		map[string]string{"Authorization": "Bearer " + sessionToken})
	if rr.Code != http.StatusOK {
		t.Fatalf("fields_patch: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var updated models.Item
	parseJSON(t, rr, &updated)

	stored := map[string]any{}
	if err := json.Unmarshal([]byte(updated.Fields), &stored); err != nil {
		t.Fatalf("stored fields are not JSON: %v (%s)", err, updated.Fields)
	}
	if got, ok := stored["cost"].(float64); !ok || got != 7 {
		t.Fatalf("cost: want a JSON number, got %[1]T(%[1]v) — stored blob: %s", stored["cost"], updated.Fields)
	}
	// The merge must not have eaten the untouched field.
	if got, ok := stored["note"].(string); !ok || got != "keep me" {
		t.Fatalf("note: want it preserved by the merge, got %[1]T(%[1]v)", stored["note"])
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

// The preflight and the copy must coerce IDENTICALLY (BUG-2850).
//
// They validate in different PACKAGES — the preflight in
// handlers_items_copy_preflight.go, the copy in
// internal/store/items_cross_workspace_copy.go — which is exactly how they
// would drift unnoticed. The preflight exists to PREDICT what the copy does,
// so coercion on one side only makes it report a field as failing that the
// copy accepts, which is worse than either behaviour alone.
//
// Both files carry a comment saying they must match. This is the test that
// makes that comment more than a wish.
func TestCopyAndPreflightCoerceIdentically(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	sessionToken := bootstrapFirstUser(t, srv, "admin@example.com", "Admin")

	mkWS := func(name string) models.Workspace {
		rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces",
			map[string]string{"name": name}, sessionToken)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create ws %s: expected 201, got %d: %s", name, rr.Code, rr.Body.String())
		}
		var ws models.Workspace
		parseJSON(t, rr, &ws)
		return ws
	}
	mkColl := func(ws models.Workspace, name, schema string) models.Collection {
		rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/collections",
			map[string]interface{}{"name": name, "schema": schema}, sessionToken)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create collection %s: expected 201, got %d: %s", name, rr.Code, rr.Body.String())
		}
		var c models.Collection
		parseJSON(t, rr, &c)
		return c
	}

	src := mkWS("Coercion Copy Src")
	dst := mkWS("Coercion Copy Dst")
	schema := `{"fields":[{"key":"cost","type":"number"}]}`
	srcColl := mkColl(src, "Jobs", schema)
	dstColl := mkColl(dst, "Jobs", schema)

	rr := doRequestWithHeaders(srv, "POST",
		"/api/v1/workspaces/"+src.Slug+"/collections/"+srcColl.Slug+"/items",
		map[string]interface{}{"title": "job", "fields": `{"cost":1}`},
		map[string]string{"Authorization": "Bearer " + sessionToken})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)

	// A STRING override for a number field — what the remote MCP door sends.
	body := map[string]interface{}{
		"target_workspace":  dst.Slug,
		"target_collection": dstColl.Slug,
		"field_overrides":   map[string]any{"cost": "42"},
	}

	pre := doRequestWithHeaders(srv, "POST",
		"/api/v1/workspaces/"+src.Slug+"/items/"+item.Slug+"/copy/preflight",
		body, map[string]string{"Authorization": "Bearer " + sessionToken})
	cp := doRequestWithHeaders(srv, "POST",
		"/api/v1/workspaces/"+src.Slug+"/items/"+item.Slug+"/copy",
		body, map[string]string{"Authorization": "Bearer " + sessionToken})

	// The agreement itself: whatever they do, they must do the same thing. A
	// preflight that previews success against a copy that refuses (or the
	// reverse) is the failure this guards, independent of which is "right".
	preOK := pre.Code >= 200 && pre.Code < 300
	cpOK := cp.Code >= 200 && cp.Code < 300
	if preOK != cpOK {
		t.Fatalf("preflight and copy disagreed on a string override for a number field:\n"+
			" preflight: %d %s\n copy:      %d %s",
			pre.Code, pre.Body.String(), cp.Code, cp.Body.String())
	}

	// And with coercion in place, both accept and the copy stores a NUMBER.
	if !cpOK {
		t.Fatalf("copy refused a coercible override: %d %s", cp.Code, cp.Body.String())
	}
	// The copy answers with a wrapper, not a bare item.
	var result struct {
		Item *models.Item `json:"item"`
	}
	parseJSON(t, cp, &result)
	if result.Item == nil {
		t.Fatalf("copy response carried no item: %s", cp.Body.String())
	}
	stored := map[string]any{}
	if err := json.Unmarshal([]byte(result.Item.Fields), &stored); err != nil {
		t.Fatalf("copied fields are not JSON: %v (%s)", err, result.Item.Fields)
	}
	if got, ok := stored["cost"].(float64); !ok || got != 42 {
		t.Fatalf("copied cost: want a JSON number 42, got %[1]T(%[1]v) — blob: %s", stored["cost"], result.Item.Fields)
	}
}

// The parity pin the ruling asked for: an UNDECLARED number and an UNDECLARED
// object, asserting the stored native type (BUG-2850).
//
// Undeclared keys are accepted — a census found 168 live values under 14 such
// keys in this deployment, and refusing them would have broken read-modify-
// write on items nobody edited wrongly. What changed is that their JSON type
// survives where the encoding carries one. This is the HTTP door, which
// carries it; the key=value doors are string-by-construction and their row is
// the string, asserted in the MCP package.
//
// It also pins the WARNING: the write names the keys it did not recognize, so
// a typo leaves a trace. Both halves in one test because they describe one
// write.
func TestUndeclaredFieldsKeepTheirTypeAndAreNamed(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	sessionToken := bootstrapFirstUser(t, srv, "admin@example.com", "Admin")

	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces",
		map[string]string{"name": "Undeclared Pin"}, sessionToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create ws: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)

	// The schema declares NOTHING but status — every key below is undeclared.
	schema := `{"fields":[{"key":"status","type":"text"}]}`
	rr = doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/collections",
		map[string]interface{}{"name": "Jobs", "schema": schema}, sessionToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var coll models.Collection
	parseJSON(t, rr, &coll)

	rr = doRequestWithHeaders(srv, "POST",
		"/api/v1/workspaces/"+ws.Slug+"/collections/"+coll.Slug+"/items",
		map[string]interface{}{
			"title":  "undeclared pin",
			"fields": `{"materials_cost":42,"spec":{"a":[1,2]}}`,
		},
		map[string]string{"Authorization": "Bearer " + sessionToken})
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 — undeclared keys are ACCEPTED, got %d: %s", rr.Code, rr.Body.String())
	}
	var created models.Item
	parseJSON(t, rr, &created)

	stored := map[string]any{}
	if err := json.Unmarshal([]byte(created.Fields), &stored); err != nil {
		t.Fatalf("stored fields are not JSON: %v (%s)", err, created.Fields)
	}
	if got, ok := stored["materials_cost"].(float64); !ok || got != 42 {
		t.Fatalf("undeclared number: want a JSON number, got %[1]T(%[1]v) — blob: %s", stored["materials_cost"], created.Fields)
	}
	obj, ok := stored["spec"].(map[string]any)
	if !ok {
		t.Fatalf("undeclared object: want a JSON object, got %[1]T(%[1]v) — blob: %s", stored["spec"], created.Fields)
	}
	if _, ok := obj["a"].([]any); !ok {
		t.Fatalf("undeclared object: nested array lost, got %[1]T(%[1]v)", obj["a"])
	}

	// And the write says which keys it did not recognize.
	if created.Warnings == nil {
		t.Fatalf("expected warnings naming the undeclared keys, got none: %s", rr.Body.String())
	}
	got := created.Warnings.UndeclaredFields
	if len(got) != 2 || got[0] != "materials_cost" || got[1] != "spec" {
		t.Fatalf("undeclared_fields = %v, want [materials_cost spec] (sorted)", got)
	}
}

// A write with nothing to report carries NO warnings element, so the response
// stays byte-identical to before for every well-formed write (BUG-2850).
func TestNoWarningsWhenEveryFieldIsDeclared(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	sessionToken := bootstrapFirstUser(t, srv, "admin@example.com", "Admin")

	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces",
		map[string]string{"name": "No Warnings"}, sessionToken)
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
		map[string]interface{}{"title": "clean", "fields": `{"cost":"5"}`},
		map[string]string{"Authorization": "Bearer " + sessionToken})
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "warnings") {
		t.Fatalf("a clean write must carry no warnings element: %s", rr.Body.String())
	}
}
