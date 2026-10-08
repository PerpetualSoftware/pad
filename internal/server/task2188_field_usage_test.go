package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-2188: removing a field or a select option was silent. The collection's
// field-usage read says how many items hold a value before the edit, and the
// update response names what the edit left behind (warnings.orphaned).

const task2188Schema = `{"fields":[
	{"key":"status","type":"select","options":["open","blocked","done"],"default":"open"},
	{"key":"tags","type":"multi_select","options":["a","b"]},
	{"key":"note","type":"text"}
]}`

type task2188Fixture struct {
	srv   *Server
	ws    string
	coll  models.Collection
	items []models.Item
}

func newTask2188Fixture(t *testing.T) *task2188Fixture {
	t.Helper()
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections", map[string]any{
		"name": "Field Usage", "schema": task2188Schema,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection: %d %s", rr.Code, rr.Body.String())
	}
	var coll models.Collection
	parseJSON(t, rr, &coll)
	f := &task2188Fixture{srv: srv, ws: ws, coll: coll}
	for _, fields := range []string{
		`{"status":"blocked","tags":["a","b"],"note":"kept text"}`,
		`{"status":"blocked","tags":["a"]}`,
		`{"status":"open","note":""}`,
		`{}`,
	} {
		irr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections/"+coll.Slug+"/items", map[string]any{
			"title": "usage item", "fields": fields,
		})
		if irr.Code != http.StatusCreated {
			t.Fatalf("create item: %d %s", irr.Code, irr.Body.String())
		}
		var it models.Item
		parseJSON(t, irr, &it)
		f.items = append(f.items, it)
	}
	return f
}

func (f *task2188Fixture) usage(t *testing.T) store.FieldUsage {
	t.Helper()
	rr := doRequest(f.srv, "GET", "/api/v1/workspaces/"+f.ws+"/collections/"+f.coll.Slug+"/field-usage", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("field-usage: %d %s", rr.Code, rr.Body.String())
	}
	var u store.FieldUsage
	parseJSON(t, rr, &u)
	return u
}

func (f *task2188Fixture) patchSchema(t *testing.T, schema string, migrations []models.FieldMigration) models.Collection {
	t.Helper()
	body := map[string]any{"schema": schema}
	if migrations != nil {
		body["migrations"] = migrations
	}
	rr := doRequest(f.srv, "PATCH", "/api/v1/workspaces/"+f.ws+"/collections/"+f.coll.Slug, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("update schema: %d %s", rr.Code, rr.Body.String())
	}
	var out models.Collection
	parseJSON(t, rr, &out)
	return out
}

func TestFieldUsage_TASK2188_Counts(t *testing.T) {
	f := newTask2188Fixture(t)
	u := f.usage(t)

	// The default fills status on the two items that did not name it.
	if got := u.Fields["status"]; got.Items != 4 || got.Values["blocked"] != 2 || got.Values["open"] != 2 {
		t.Errorf("status usage = %+v, want 4 items, blocked 2, open 2", got)
	}
	if got := u.Fields["tags"]; got.Items != 2 || got.Values["a"] != 2 || got.Values["b"] != 1 {
		t.Errorf("tags usage = %+v, want 2 items, a 2, b 1", got)
	}
	// "" is no value: one item holds note, not two.
	if got := u.Fields["note"]; got.Items != 1 {
		t.Errorf("note usage = %+v, want 1 item", got)
	}
}

func TestFieldUsage_TASK2188_UpdateNamesWhatItOrphaned(t *testing.T) {
	f := newTask2188Fixture(t)

	// Remove note, remove status's "blocked", rename tags "a" → "alpha" (a
	// migration, so nothing orphaned) and drop tags "b".
	next := `{"fields":[
		{"key":"status","type":"select","options":["open","done"],"default":"open"},
		{"key":"tags","type":"multi_select","options":["alpha"]}
	]}`
	updated := f.patchSchema(t, next, []models.FieldMigration{{Field: "tags", RenameOptions: map[string]string{"a": "alpha"}}})
	if updated.Warnings == nil {
		t.Fatal("update that orphaned values carried no warnings")
	}
	want := []models.OrphanedValue{
		{Field: "note", Items: 1},
		{Field: "status", Option: "blocked", Items: 2},
		{Field: "tags", Option: "b", Items: 1},
	}
	if !reflect.DeepEqual(updated.Warnings.Orphaned, want) {
		t.Fatalf("orphaned = %+v, want %+v", updated.Warnings.Orphaned, want)
	}

	// The removed field's value is retained: declaring the key again shows it.
	f.patchSchema(t, `{"fields":[
		{"key":"status","type":"select","options":["open","done"],"default":"open"},
		{"key":"tags","type":"multi_select","options":["alpha"]},
		{"key":"note","type":"text"}
	]}`, nil)
	rr := doRequest(f.srv, "GET", "/api/v1/workspaces/"+f.ws+"/items/"+f.items[0].Slug, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get item: %d %s", rr.Code, rr.Body.String())
	}
	var got models.Item
	parseJSON(t, rr, &got)
	var fields map[string]any
	if err := json.Unmarshal([]byte(got.Fields), &fields); err != nil {
		t.Fatalf("decode fields %q: %v", got.Fields, err)
	}
	if fields["note"] != "kept text" {
		t.Errorf("re-declared note = %v, want the retained %q", fields["note"], "kept text")
	}
}

// An edit that removes nothing carries no orphan report, and neither does a
// removal no item holds a value for: the response is byte-identical to before.
func TestFieldUsage_TASK2188_NothingOrphanedNoWarning(t *testing.T) {
	f := newTask2188Fixture(t)
	added := f.patchSchema(t, `{"fields":[
		{"key":"status","type":"select","options":["open","blocked","done","later"],"default":"open"},
		{"key":"tags","type":"multi_select","options":["a","b"]},
		{"key":"note","type":"text"}
	]}`, nil)
	if added.Warnings != nil {
		t.Errorf("an additive edit carried warnings: %+v", added.Warnings)
	}
	// "done" and "later" are held by no item.
	removed := f.patchSchema(t, `{"fields":[
		{"key":"status","type":"select","options":["open","blocked"],"default":"open"},
		{"key":"tags","type":"multi_select","options":["a","b"]},
		{"key":"note","type":"text"}
	]}`, nil)
	if removed.Warnings != nil {
		t.Errorf("removing options no item holds carried warnings: %+v", removed.Warnings)
	}
}

// What an orphaned option value does to later writes, which is what the
// editor's notice says (measured, not assumed): an edit to ANOTHER field
// keeps it, setting the field to it again is refused, and a full-fields write
// that carries it unchanged is refused too.
func TestFieldUsage_TASK2188_OrphanedOptionOnLaterWrites(t *testing.T) {
	f := newTask2188Fixture(t)
	f.patchSchema(t, `{"fields":[
		{"key":"status","type":"select","options":["open","done"],"default":"open"},
		{"key":"tags","type":"multi_select","options":["a","b"]},
		{"key":"note","type":"text"}
	]}`, nil)
	path := "/api/v1/workspaces/" + f.ws + "/items/" + f.items[0].Slug

	other := doRequest(f.srv, "PATCH", path, map[string]any{"fields_patch": map[string]any{"note": "edited"}})
	if other.Code != http.StatusOK {
		t.Fatalf("patching another field answered %d: %s", other.Code, other.Body.String())
	}
	var afterOther models.Item
	parseJSON(t, other, &afterOther)
	var fields map[string]any
	_ = json.Unmarshal([]byte(afterOther.Fields), &fields)
	if fields["status"] != "blocked" {
		t.Errorf("status after patching another field = %v, want the orphaned %q kept", fields["status"], "blocked")
	}

	same := doRequest(f.srv, "PATCH", path, map[string]any{"fields_patch": map[string]any{"status": "blocked"}})
	if same.Code != http.StatusBadRequest {
		t.Errorf("setting the removed option again answered %d, want 400: %s", same.Code, same.Body.String())
	}

	full := doRequest(f.srv, "PATCH", path, map[string]any{"fields": afterOther.Fields})
	if full.Code != http.StatusBadRequest {
		t.Errorf("a full-fields write carrying the orphaned value answered %d, want 400: %s", full.Code, full.Body.String())
	}
}

// The read has the PATCH's gates: an editor cannot read it, and an owner
// restricted away from a collection gets the 404 the PATCH gives.
func TestFieldUsage_TASK2188_Gates(t *testing.T) {
	f := newRestrictedOwnerVisibilityFixture(t)
	hidden := doRequestWithHeaders(f.srv, "GET", "/api/v1/workspaces/"+f.ws.Slug+"/collections/"+f.hiddenColl.Slug+"/field-usage", nil, f.bearerHeaders())
	hiddenPatch := doRequestWithHeaders(f.srv, "PATCH", "/api/v1/workspaces/"+f.ws.Slug+"/collections/"+f.hiddenColl.Slug, map[string]any{"name": "x"}, f.bearerHeaders())
	if hidden.Code == http.StatusOK || hidden.Code != hiddenPatch.Code {
		t.Errorf("hidden collection: field-usage %d, PATCH %d; want the same refusal", hidden.Code, hiddenPatch.Code)
	}
	visible := doRequestWithHeaders(f.srv, "GET", "/api/v1/workspaces/"+f.ws.Slug+"/collections/"+f.visibleColl.Slug+"/field-usage", nil, f.bearerHeaders())
	if visible.Code != http.StatusOK {
		t.Errorf("visible collection: field-usage %d, want 200: %s", visible.Code, visible.Body.String())
	}

	editor, err := f.srv.store.CreateUser(models.UserCreate{
		Email: "usage-editor@example.com", Name: "Usage Editor", Username: "usage-editor", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.srv.store.AddWorkspaceMember(f.ws.ID, editor.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	tok, err := f.srv.store.CreateAPIToken(editor.ID, models.APITokenCreate{Name: "editor-pat", WorkspaceID: f.ws.ID}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	asEditor := doRequestWithHeaders(f.srv, "GET", "/api/v1/workspaces/"+f.ws.Slug+"/collections/"+f.visibleColl.Slug+"/field-usage", nil,
		map[string]string{"Authorization": "Bearer " + tok.Token})
	if asEditor.Code != http.StatusForbidden {
		t.Errorf("editor: field-usage %d, want 403: %s", asEditor.Code, asEditor.Body.String())
	}
}
