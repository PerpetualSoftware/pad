package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2685 — a collection schema that DECLARES a reserved metadata key.
//
// `validateNoReservedFieldKeys` (BUG-2674) stops a schema NEWLY declaring one,
// but GRANDFATHERS an existing declaration, so the shape survives in any
// workspace that had it before that gate landed. These tests reproduce that
// shape the only way it can exist — written through the store, which is the
// door a pre-gate row came through — and pin what the declaration does to
// item-field paths that consult a schema.
//
// The declaration is wrong in BOTH directions at once, which is why the class
// remedy is to ignore it rather than to refuse the write: a `text` declaration
// REFUSES the system's own value (a notes ARRAY) and ACCEPTS a caller's junk
// string.

// grandfatherReservedKey rewrites a collection's STORED schema to declare def,
// bypassing the HTTP gate exactly as a legacy row does. Returns the collection.
func grandfatherReservedKey(t *testing.T, srv *Server, ws, collSlug string, def models.FieldDef) models.Collection {
	t.Helper()
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/collections/"+collSlug, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("load collection %q: %d %s", collSlug, rr.Code, rr.Body.String())
	}
	var coll models.Collection
	parseJSON(t, rr, &coll)

	var schema models.CollectionSchema
	if err := json.Unmarshal([]byte(coll.Schema), &schema); err != nil {
		t.Fatalf("parse stored schema: %v (%s)", err, coll.Schema)
	}
	schema.Fields = append(schema.Fields, def)
	raw, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	s := string(raw)
	updated, err := srv.store.UpdateCollection(coll.ID, models.CollectionUpdate{Schema: &s})
	if err != nil {
		t.Fatalf("store-level schema write: %v", err)
	}

	// PREMISE, asserted rather than assumed: the HTTP door would have REFUSED
	// this declaration, so the store write is genuinely reproducing a shape no
	// current client can create — not just taking a shortcut to one it could.
	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections",
		map[string]interface{}{
			"name":   "Gate Check " + def.Key,
			"schema": `{"fields":[{"key":"` + def.Key + `","type":"text"}]}`,
		})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("the reserved-key gate is not in force: creating a collection declaring %q returned %d %s",
			def.Key, rr.Code, rr.Body.String())
	}
	return *updated
}

func bug2685CreateItem(t *testing.T, srv *Server, ws, coll string, body map[string]interface{}) *httpResult {
	t.Helper()
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections/"+coll+"/items", body)
	return &httpResult{code: rr.Code, body: rr.Body.String()}
}

type httpResult struct {
	code int
	body string
}

// Leg A — the system's OWN value is refused.
//
// `pad item note` / `pad item decide` read the item's fields, append an entry
// with models.AppendImplementationNote, and PATCH the whole blob back. Against
// a legacy `implementation_notes: text` declaration that blob fails
// validateFieldType, so the collection's items cannot be annotated at all.
func TestBUG2685_LegacyDeclarationRefusesTheSystemsOwnWrite(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	grandfatherReservedKey(t, srv, ws, "tasks", models.FieldDef{
		Key:  models.ItemFieldImplementationNotes,
		Type: "text",
	})

	got := bug2685CreateItem(t, srv, ws, "tasks", map[string]interface{}{"title": "Annotate me"})
	if got.code != http.StatusCreated {
		t.Fatalf("create: %d %s", got.code, got.body)
	}
	var created models.Item
	if err := json.Unmarshal([]byte(got.body), &created); err != nil {
		t.Fatalf("decode created item: %v (%s)", err, got.body)
	}

	// Exactly what `pad item note` sends.
	fields, err := models.AppendImplementationNote(created.Fields, models.ItemImplementationNote{
		Summary:   "a note",
		Details:   "the details",
		CreatedAt: "2026-09-14T00:00:00Z",
		CreatedBy: "agent",
	})
	if err != nil {
		t.Fatalf("AppendImplementationNote: %v", err)
	}

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+ws+"/items/"+created.Slug,
		map[string]interface{}{"fields": fields})
	if rr.Code != http.StatusOK {
		t.Errorf("the system's own note write was refused by a legacy declaration: %d %s",
			rr.Code, rr.Body.String())
	}
}

// Leg B — the declaration must not DECIDE the answer.
//
// The class rule stated as something a test can refute: two collections, one
// carrying a legacy declaration and one not, must answer the same bytes the
// same way. Unfixed they do not — a legacy `implementation_notes: number`
// refuses the string that the undeclared collection stores, so whether a write
// succeeds turns on a FieldDef nobody may create any more.
//
// The control leg is what makes this evidence rather than an assertion about
// one path: it also pins the direction of the disagreement, so a future change
// that closes the create-mint hole (undeclared reserved keys are ACCEPTED at
// create — BUG-2850 chose accept-and-name, and `items/overrides.go` names the
// hole) fails this test loudly instead of silently inverting it.
//
// Note what is NOT asserted: that the junk is refused. Refusing is the other
// class and is out of this unit — see the BUG-2685 trail.
func TestBUG2685_ADeclarationDoesNotDecideWhetherAWriteSucceeds(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)

	body := map[string]interface{}{
		"title":  "Same bytes",
		"fields": `{"implementation_notes":"not a number and not a notes array"}`,
	}

	// CONTROL first, before the declaration exists anywhere.
	control := bug2685CreateItem(t, srv, ws, "ideas", body)

	grandfatherReservedKey(t, srv, ws, "tasks", models.FieldDef{
		Key:  models.ItemFieldImplementationNotes,
		Type: "number",
	})
	declared := bug2685CreateItem(t, srv, ws, "tasks", body)

	if control.code != declared.code {
		t.Errorf("a grandfathered declaration decided the outcome: undeclared collection answered %d, "+
			"declared collection answered %d\n  undeclared: %s\n  declared:   %s",
			control.code, declared.code, control.body, declared.body)
	}
	if control.code != http.StatusCreated {
		t.Fatalf("premise broken: the control write did not succeed (%d %s), so the comparison above "+
			"could pass by both paths failing", control.code, control.body)
	}
}

// Leg C — a legacy REQUIRED declaration makes the collection uncreatable.
//
// Nothing in a create request mentions implementation_notes, and no client
// surface offers it, so the 400 names a field the caller cannot supply.
func TestBUG2685_LegacyRequiredDeclarationBlocksCreate(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	grandfatherReservedKey(t, srv, ws, "tasks", models.FieldDef{
		Key:      models.ItemFieldDecisionLog,
		Type:     "text",
		Required: true,
	})

	got := bug2685CreateItem(t, srv, ws, "tasks", map[string]interface{}{"title": "Ordinary task"})
	if got.code != http.StatusCreated {
		t.Errorf("an ordinary create was refused over a reserved key nobody typed: %d %s", got.code, got.body)
	}
}

// Leg D — the two PRESENTATION doors must not publish the declaration either.
//
// These are the "bootstrap / share presentation" row of the filed table, and
// the bootstrap one is why a sweep owes its vantage point: that decode does NOT
// land in a models.CollectionSchema — `bootstrapSchema` is a parallel struct —
// so the type-resolved pass that found the other 23 sites could not see it. It
// surfaced only by chasing a review finding about the OTHER door here.
//
// Bootstrap is the sharper of the two: the blob goes to every agent at session
// start and the skill treats `collections[].schema` as the write contract, so a
// surviving declaration is one every agent is told to write into.
//
// Two tests rather than two subtests on one server, deliberately: the share leg
// has to CREATE A USER for the link's created_by FK, and doing so closes the
// fresh-install no-auth window the bootstrap leg's unauthenticated GET depends
// on. Sharing a server made the second leg 401 — a failure about the harness,
// inside a test whose whole job is to be about the payload.
func bug2685PresentationWorkspace(t *testing.T) (*Server, string) {
	t.Helper()
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	grandfatherReservedKey(t, srv, ws, "tasks", models.FieldDef{
		Key:   models.ItemFieldImplementationNotes,
		Label: "Implementation Notes",
		Type:  "text",
	})
	return srv, ws
}

// Codex round 2 raised the public payload as a response-shape change and
// proposed preserving the raw schema. The change IS the fix, and the reason is
// in the handler's own comment two lines above the decode: the schema is
// emitted "so the public viewer can render" the items. That is an item-field
// purpose, so an anonymous viewer would otherwise be handed a declaration
// telling it to render system metadata as an ordinary field. Pinned here so the
// contract change stays deliberate.
func TestBUG2685_PublicSharePayloadDoesNotPublishAGrandfatheredDeclaration(t *testing.T) {
	srv, ws := bug2685PresentationWorkspace(t)

	// Minted through the store rather than the HTTP door: that door records
	// created_by and the FK needs a real user, which this workspace (made by
	// the no-auth fresh-install path) has none of. Nothing about the READ under
	// test depends on how the link was minted.
	wsRow, err := srv.store.GetWorkspaceBySlug(ws)
	if err != nil || wsRow == nil {
		t.Fatalf("load workspace: %v", err)
	}
	coll, err := srv.store.GetCollectionBySlug(wsRow.ID, "tasks")
	if err != nil || coll == nil {
		t.Fatalf("load tasks collection: %v", err)
	}
	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "bug2685-share@example.com", Name: "Share", Username: "bug2685-share",
		Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	link, err := srv.store.CreateShareLink(wsRow.ID, "collection", coll.ID, "view", owner.ID, nil)
	if err != nil {
		t.Fatalf("mint share link: %v", err)
	}

	rr := doRequest(srv, "GET", "/api/v1/s/"+link.Token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("resolve share link: %d %s", rr.Code, rr.Body.String())
	}
	var resolved struct {
		Collection struct {
			Schema struct {
				Fields []struct {
					Key string `json:"key"`
				} `json:"fields"`
			} `json:"schema"`
		} `json:"collection"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resolved); err != nil {
		t.Fatalf("decode resolved share: %v (%s)", err, rr.Body.String())
	}
	if len(resolved.Collection.Schema.Fields) == 0 {
		t.Fatal("premise broken: the public payload carried no schema fields at all, " +
			"so the assertion below cannot distinguish a strip from an absent schema")
	}
	for _, f := range resolved.Collection.Schema.Fields {
		if models.IsReservedItemField(f.Key) {
			t.Errorf("the public share payload published a grandfathered reserved-key declaration: %q", f.Key)
		}
	}
}

func TestBUG2685_BootstrapDoesNotPublishAGrandfatheredDeclarationToEveryAgent(t *testing.T) {
	srv, ws := bug2685PresentationWorkspace(t)

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/agent/bootstrap", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("bootstrap: %d %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Collections []struct {
			Slug   string `json:"slug"`
			Schema struct {
				Fields []struct {
					Key string `json:"key"`
				} `json:"fields"`
			} `json:"schema"`
		} `json:"collections"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode bootstrap: %v", err)
	}
	var sawTasks bool
	for _, c := range resp.Collections {
		if c.Slug != "tasks" {
			continue
		}
		sawTasks = true
		for _, f := range c.Schema.Fields {
			if models.IsReservedItemField(f.Key) {
				t.Errorf("bootstrap published a grandfathered reserved-key declaration to every agent: %q", f.Key)
			}
		}
		if len(c.Schema.Fields) == 0 {
			t.Error("premise broken: the tasks schema came back with no fields at all, " +
				"so the assertion above cannot distinguish a strip from an empty projection")
		}
	}
	if !sawTasks {
		t.Fatal("premise broken: bootstrap returned no tasks collection")
	}
}
