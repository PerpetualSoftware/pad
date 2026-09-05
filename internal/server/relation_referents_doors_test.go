package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The per-door × per-provenance table for referent validation (PLAN-2857 U1 /
// TASK-2878).
//
// WHY A TABLE AT ALL, given internal/store already tests the rule exhaustively.
// Those tests call `ResolveRelationReferents` / `MigrateRelationReferents`
// directly, so they vouch for the COMPONENT. They say nothing about whether
// any door is bound to it — a door that never calls the resolver passes every
// one of them. This table is the binding claim, one leg per (door, provenance)
// pair, driven through the HTTP handler the client actually reaches.
//
// PROVENANCE IS THE SECOND AXIS BECAUSE THE ANSWER DEPENDS ON IT, not for
// symmetry. A SUPPLIED value is asserted by the caller and an unresolvable one
// is their bug, so it is refused. A CARRIED value was asserted by nobody:
// `internal/items` has accepted any string in a relation field since the field
// type existed, so refusing carried values would make legacy items
// un-updatable, un-movable and un-copyable — the failure this unit would
// otherwise CAUSE while fixing another. Which of the two a door sees is a
// property of the door, and getting it wrong is invisible until a legacy item
// meets it.
//
// The eight doors:
//
//	 1. create                       supplied only
//	 2. update (fields)              supplied only — a full replacement map
//	 3. update (fields_patch)        supplied + carried (untouched stored keys)
//	 4. bulk update                  supplied + carried (stored blob is merged)
//	 5. move                         supplied (overrides) + carried
//	 6. bulk move                    carried only — no per-field overrides
//	 7. cross-workspace copy         supplied + carried
//	 8. copy preflight               supplied + carried
//
// Doors 7 and 8 are driven here too, but their AGREEMENT — one body, two
// endpoints, the same answer — is pinned separately in
// TestCopyEndpoint_PreflightAndCopyAgreeOnRelationReferents, because agreement
// between two doors is a claim a per-door table structurally cannot make.

// --- fixture -----------------------------------------------------------

type doorFixture struct {
	t     *testing.T
	srv   *Server
	owner *models.User
	ws    *models.Workspace
	// tasks holds the items under test; people is the relation target.
	tasks, people *models.Collection
	// target is a live item in `people`: the only value that resolves.
	target *models.Item
}

func newDoorFixture(t *testing.T) *doorFixture {
	t.Helper()
	srv := testServer(t)
	owner := mustUser(t, srv, "door-owner@example.com", "doorowner", "")
	ws := mustWorkspace(t, srv, "Door WS", owner.ID)

	people := mustSchemaCollection(t, srv, ws.ID, "People", `{"fields":[]}`)
	tasks := mustSchemaCollection(t, srv, ws.ID, "Doors", fmt.Sprintf(`{"fields":[
		{"key":"status","label":"Status","type":"select","options":["open","done"]},
		{"key":"priority","label":"Priority","type":"select","options":["low","high"]},
		{"key":"owner_ref","label":"Owner","type":"relation","collection":%q}
	]}`, people.Slug))

	target, err := srv.store.CreateItem(ws.ID, people.ID, models.ItemCreate{
		Title: "Ada", CreatedBy: owner.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem(target): %v", err)
	}

	return &doorFixture{t: t, srv: srv, owner: owner, ws: ws, tasks: tasks, people: people, target: target}
}

// seed creates an item in `tasks` with the given fields blob, writing it
// through the STORE so a value the doors would refuse can still be planted.
// That is the whole point: every carried-provenance leg needs an item whose
// stored relation value is already bad, which is precisely what no door will
// accept any more. Legacy rows exist; this is how one is reproduced.
func (f *doorFixture) seed(fields string) *models.Item {
	f.t.Helper()
	it, err := f.srv.store.CreateItem(f.ws.ID, f.tasks.ID, models.ItemCreate{
		Title: "Subject " + fields, Fields: fields, CreatedBy: f.owner.ID,
	})
	if err != nil {
		f.t.Fatalf("seed(%s): %v", fields, err)
	}
	return it
}

// call drives one handler with the route params it reads, exactly as the
// router would. Shared by every leg so a difference in invocation cannot be
// mistaken for a difference in behaviour.
func (f *doorFixture) call(h http.HandlerFunc, method, path string, params map[string]string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			f.t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	r := httptest.NewRequest(method, path, reader)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	ctx := WithCurrentUser(r.Context(), f.owner)
	ctx = contextWithWorkspaceRoleForTest(ctx, "owner")
	ctx = contextWithResolvedWorkspaceIDForTest(ctx, f.ws.ID)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("slug", f.ws.Slug)
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	rr := httptest.NewRecorder()
	h(rr, r.WithContext(ctx))
	return rr
}

// storedRelation reads owner_ref back out of the database. Deliberately not
// the response body: a handler that echoed the right thing while writing the
// wrong thing would pass a response-only check.
func (f *doorFixture) storedRelation(itemID string) (any, bool) {
	f.t.Helper()
	it, err := f.srv.store.GetItem(itemID)
	if err != nil || it == nil {
		f.t.Fatalf("GetItem(%s): %v", itemID, err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(it.Fields), &m); err != nil {
		f.t.Fatalf("parse fields %q: %v", it.Fields, err)
	}
	v, ok := m["owner_ref"]
	return v, ok
}

// storedRelationKey is storedRelation for a key other than owner_ref. Same
// reason it reads the database rather than the response: a handler that
// echoed the right thing while writing the wrong thing would pass a
// response-only check.
func (f *doorFixture) storedRelationKey(itemID, key string) (any, bool) {
	f.t.Helper()
	it, err := f.srv.store.GetItem(itemID)
	if err != nil || it == nil {
		f.t.Fatalf("GetItem(%s): %v", itemID, err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(it.Fields), &m); err != nil {
		f.t.Fatalf("parse fields %q: %v", it.Fields, err)
	}
	v, ok := m[key]
	return v, ok
}

// badRef is shaped like a real issue ref and names nothing. Not a slug and not
// free text: those are refused by a DIFFERENT rule (the resolver has no slug
// fallback), and a fixture rejectable two ways discriminates nothing.
const badRef = "PEOP-9999"

// assertRefused is the shared shape of every refusing leg.
func assertRefused(t *testing.T, door string, rr *httptest.ResponseRecorder) {
	t.Helper()
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("%s: expected 400 for an unresolvable SUPPLIED referent, got %d: %s",
			door, rr.Code, rr.Body.String())
	}
	if code := errCode(t, rr); code != "validation_error" {
		t.Fatalf("%s: error code = %q, want validation_error", door, code)
	}
	if !strings.Contains(rr.Body.String(), badRef) {
		t.Fatalf("%s: the refusal does not quote the offending value: %s", door, rr.Body.String())
	}
}

// --- doors 1-2: supplied-only writes -----------------------------------

func TestRelationDoors_Create(t *testing.T) {
	t.Run("supplied unresolvable is refused", func(t *testing.T) {
		f := newDoorFixture(t)
		rr := f.call(f.srv.handleCreateItem, "POST",
			"/api/v1/workspaces/"+f.ws.Slug+"/collections/"+f.tasks.Slug+"/items",
			map[string]string{"collSlug": f.tasks.Slug},
			map[string]any{"title": "New", "fields": map[string]any{"owner_ref": badRef}})
		assertRefused(t, "create", rr)
	})

	t.Run("supplied resolvable is accepted and canonicalised", func(t *testing.T) {
		// The positive control. Without it every leg above is equally
		// consistent with a door that refuses all relation values.
		f := newDoorFixture(t)
		rr := f.call(f.srv.handleCreateItem, "POST",
			"/api/v1/workspaces/"+f.ws.Slug+"/collections/"+f.tasks.Slug+"/items",
			map[string]string{"collSlug": f.tasks.Slug},
			map[string]any{"title": "New", "fields": map[string]any{"owner_ref": f.target.Ref}})
		if rr.Code != http.StatusCreated {
			t.Fatalf("create: expected 201, got %d: %s", rr.Code, rr.Body.String())
		}
		var out struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("parse create result: %v: %s", err, rr.Body.String())
		}
		// Supplied as a REF; what must land is the ID. A door that stored the
		// string unresolved would pass an equality check against itself.
		if v, ok := f.storedRelation(out.ID); !ok || v != f.target.ID {
			t.Fatalf("stored owner_ref = %#v (present=%v), want the resolved id %q", v, ok, f.target.ID)
		}
	})
}

func TestRelationDoors_UpdateFields(t *testing.T) {
	f := newDoorFixture(t)
	item := f.seed(`{"status":"open"}`)

	rr := f.call(f.srv.handleUpdateItem, "PATCH",
		"/api/v1/workspaces/"+f.ws.Slug+"/items/"+item.Slug,
		map[string]string{"itemSlug": item.Slug},
		map[string]any{"fields": map[string]any{"status": "open", "owner_ref": badRef}})
	assertRefused(t, "update(fields)", rr)

	if _, present := f.storedRelation(item.ID); present {
		t.Fatalf("a refused update wrote owner_ref anyway")
	}
}

// --- doors 3-4: the two doors that see BOTH provenances ----------------

func TestRelationDoors_UpdateFieldsPatch(t *testing.T) {
	t.Run("supplied unresolvable is refused", func(t *testing.T) {
		f := newDoorFixture(t)
		item := f.seed(`{"status":"open"}`)
		rr := f.call(f.srv.handleUpdateItem, "PATCH",
			"/api/v1/workspaces/"+f.ws.Slug+"/items/"+item.Slug,
			map[string]string{"itemSlug": item.Slug},
			map[string]any{"fields_patch": map[string]any{"owner_ref": badRef}})
		assertRefused(t, "update(fields_patch)", rr)
	})

	t.Run("a carried value the patch does not touch is left alone", func(t *testing.T) {
		// The regression this whole provenance axis exists to prevent: an item
		// whose stored relation is already unresolvable must stay editable.
		f := newDoorFixture(t)
		item := f.seed(fmt.Sprintf(`{"status":"open","owner_ref":%q}`, badRef))

		rr := f.call(f.srv.handleUpdateItem, "PATCH",
			"/api/v1/workspaces/"+f.ws.Slug+"/items/"+item.Slug,
			map[string]string{"itemSlug": item.Slug},
			map[string]any{"fields_patch": map[string]any{"status": "done"}})
		if rr.Code != http.StatusOK {
			t.Fatalf("a patch that never mentions owner_ref was refused %d because of a "+
				"value it did not touch: %s", rr.Code, rr.Body.String())
		}
		if v, ok := f.storedRelation(item.ID); !ok || v != badRef {
			t.Fatalf("the untouched carried value changed: %#v (present=%v)", v, ok)
		}
	})
}

func TestRelationDoors_BulkUpdate(t *testing.T) {
	t.Run("a carried value the operation does not touch is left alone", func(t *testing.T) {
		// Same claim as the fields_patch leg, and the door is easier to get
		// wrong: `bulkFieldUpdate` merges the item's STORED blob with the
		// caller's changes before validating, so a resolver pointed at the
		// merged map re-litigates every stored value and refuses a legacy
		// item's status change.
		f := newDoorFixture(t)
		item := f.seed(fmt.Sprintf(`{"status":"open","owner_ref":%q}`, badRef))

		rr := f.call(f.srv.handleBulkItems, "POST",
			"/api/v1/workspaces/"+f.ws.Slug+"/items/bulk", nil,
			map[string]any{"op": "move", "ids": []string{item.ID}, "status": "done"})
		if rr.Code != http.StatusOK {
			t.Fatalf("bulk status move: expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var out bulkItemsResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("parse bulk response: %v: %s", err, rr.Body.String())
		}
		if len(out.Failed) > 0 {
			t.Fatalf("a bulk status move failed on a value it never touched: %+v", out.Failed)
		}
		if v, ok := f.storedRelation(item.ID); !ok || v != badRef {
			t.Fatalf("the untouched carried value changed: %#v (present=%v)", v, ok)
		}
	})
}

// requestFor builds a request carrying the fixture owner's auth context, for
// the one leg that must call a handler helper directly.
func (f *doorFixture) requestFor() *http.Request {
	f.t.Helper()
	r := httptest.NewRequest("POST", "/api/v1/workspaces/"+f.ws.Slug+"/items/bulk", nil)
	ctx := WithCurrentUser(r.Context(), f.owner)
	ctx = contextWithWorkspaceRoleForTest(ctx, "owner")
	ctx = contextWithResolvedWorkspaceIDForTest(ctx, f.ws.ID)
	return r.WithContext(ctx)
}

// TestRelationDoors_BulkUpdateSuppliedBranch covers the one door whose
// SUPPLIED branch no black-box leg can reach.
//
// STATED PLAINLY BECAUSE IT IS A WEAKER INSTRUMENT: every other leg in this
// file drives an HTTP handler, so it proves the door is bound to the resolver.
// This one calls `bulkFieldUpdate` directly, because no bulk op puts a
// relation key into `changes` — `op` is validated against a closed list and
// the only field values any of them set are `status` and `priority`. So the
// branch is unreachable from outside today, and the dispatch mutant confirms
// it: unwiring this door alone leaves every other leg in this file passing.
//
// It is kept, rather than deleted as dead code, for the reason its sibling in
// `bulkMoveCollection` is kept — the day the bulk path grows per-field
// overrides, the branch must already refuse rather than be a silent no-op
// nobody notices is missing. A direct-call test is the only instrument that
// can hold that, and it vouches for the FUNCTION, not for a binding that does
// not exist yet.
func TestRelationDoors_BulkUpdateSuppliedBranch(t *testing.T) {
	t.Run("an unresolvable supplied change is refused", func(t *testing.T) {
		f := newDoorFixture(t)
		item := f.seed(`{"status":"open"}`)

		_, opErr := f.srv.bulkFieldUpdate(f.requestFor(), f.ws.ID, item,
			map[string]any{"owner_ref": badRef}, true, nil, f.owner.ID, "test", "batch", nil)
		if opErr == nil {
			t.Fatalf("bulkFieldUpdate accepted an unresolvable supplied referent")
		}
		if opErr.code != "validation_error" {
			t.Fatalf("code = %q, want validation_error (message: %s)", opErr.code, opErr.message)
		}
		if !strings.Contains(opErr.message, badRef) {
			t.Fatalf("the refusal does not quote the offending value: %s", opErr.message)
		}
	})

	t.Run("a resolvable supplied change is canonicalised", func(t *testing.T) {
		f := newDoorFixture(t)
		item := f.seed(`{"status":"open"}`)

		if _, opErr := f.srv.bulkFieldUpdate(f.requestFor(), f.ws.ID, item,
			map[string]any{"owner_ref": f.target.Ref}, true, nil, f.owner.ID, "test", "batch", nil); opErr != nil {
			t.Fatalf("bulkFieldUpdate refused a resolvable referent: %s", opErr.message)
		}
		// Supplied as a ref; the ID is what must be stored. Without the
		// write-back after resolution the ref would persist verbatim.
		if v, ok := f.storedRelation(item.ID); !ok || v != f.target.ID {
			t.Fatalf("stored owner_ref = %#v (present=%v), want the resolved id %q", v, ok, f.target.ID)
		}
	})
}

// --- doors 5-6: the same-workspace migrate doors -----------------------

// targetCollection is a second collection in `tasks`'s workspace with the same
// schema, so a move has somewhere to go and the relation field survives
// migration by key and type.
func (f *doorFixture) targetCollection() *models.Collection {
	f.t.Helper()
	return mustSchemaCollection(f.t, f.srv, f.ws.ID, "Doors Two", fmt.Sprintf(`{"fields":[
		{"key":"status","label":"Status","type":"select","options":["open","done"]},
		{"key":"priority","label":"Priority","type":"select","options":["low","high"]},
		{"key":"owner_ref","label":"Owner","type":"relation","collection":%q}
	]}`, f.people.Slug))
}

func TestRelationDoors_Move(t *testing.T) {
	t.Run("supplied unresolvable override is refused", func(t *testing.T) {
		f := newDoorFixture(t)
		dst := f.targetCollection()
		item := f.seed(`{"status":"open"}`)

		rr := f.call(f.srv.handleMoveItem, "POST",
			"/api/v1/workspaces/"+f.ws.Slug+"/items/"+item.Slug+"/move",
			map[string]string{"itemSlug": item.Slug},
			map[string]any{
				"target_collection": dst.Slug,
				"field_overrides":   map[string]any{"owner_ref": badRef},
			})
		assertRefused(t, "move", rr)
	})

	t.Run("a resolvable carried value SURVIVES the move", func(t *testing.T) {
		// Within a workspace the referent is still there, so dropping would
		// destroy a correct relation. This is the leg that separates "the door
		// is wired" from "the door drops everything".
		f := newDoorFixture(t)
		dst := f.targetCollection()
		item := f.seed(fmt.Sprintf(`{"status":"open","owner_ref":%q}`, f.target.ID))

		rr := f.call(f.srv.handleMoveItem, "POST",
			"/api/v1/workspaces/"+f.ws.Slug+"/items/"+item.Slug+"/move",
			map[string]string{"itemSlug": item.Slug},
			map[string]any{"target_collection": dst.Slug})
		if rr.Code != http.StatusOK {
			t.Fatalf("move: expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		if v, ok := f.storedRelation(item.ID); !ok || v != f.target.ID {
			t.Fatalf("a valid relation did not survive a same-workspace move: %#v (present=%v)", v, ok)
		}
	})

	t.Run("an unresolvable carried value is dropped and REPORTED", func(t *testing.T) {
		f := newDoorFixture(t)
		dst := f.targetCollection()
		item := f.seed(fmt.Sprintf(`{"status":"open","owner_ref":%q}`, badRef))

		rr := f.call(f.srv.handleMoveItem, "POST",
			"/api/v1/workspaces/"+f.ws.Slug+"/items/"+item.Slug+"/move",
			map[string]string{"itemSlug": item.Slug},
			map[string]any{"target_collection": dst.Slug})
		if rr.Code != http.StatusOK {
			t.Fatalf("move: expected 200 (a carried value is dropped, never refused), got %d: %s",
				rr.Code, rr.Body.String())
		}
		if v, ok := f.storedRelation(item.ID); ok {
			t.Fatalf("an unresolvable relation survived the move as %#v", v)
		}
		// Silently dropping is the defect BUG-2674 closed for moves. Dropping
		// and saying so is the contract.
		if !strings.Contains(rr.Body.String(), "owner_ref") {
			t.Fatalf("the move dropped owner_ref without reporting it: %s", rr.Body.String())
		}
	})
}

func TestRelationDoors_BulkMove(t *testing.T) {
	t.Run("a resolvable carried value SURVIVES", func(t *testing.T) {
		f := newDoorFixture(t)
		dst := f.targetCollection()
		item := f.seed(fmt.Sprintf(`{"status":"open","owner_ref":%q}`, f.target.ID))

		rr := f.call(f.srv.handleBulkItems, "POST",
			"/api/v1/workspaces/"+f.ws.Slug+"/items/bulk", nil,
			map[string]any{"op": "move", "ids": []string{item.ID}, "collection": dst.Slug})
		if rr.Code != http.StatusOK {
			t.Fatalf("bulk move: expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		if v, ok := f.storedRelation(item.ID); !ok || v != f.target.ID {
			t.Fatalf("a valid relation did not survive a bulk move: %#v (present=%v)", v, ok)
		}
	})

	t.Run("an unresolvable carried value is dropped, not refused", func(t *testing.T) {
		f := newDoorFixture(t)
		dst := f.targetCollection()
		item := f.seed(fmt.Sprintf(`{"status":"open","owner_ref":%q}`, badRef))

		rr := f.call(f.srv.handleBulkItems, "POST",
			"/api/v1/workspaces/"+f.ws.Slug+"/items/bulk", nil,
			map[string]any{"op": "move", "ids": []string{item.ID}, "collection": dst.Slug})
		if rr.Code != http.StatusOK {
			t.Fatalf("bulk move: expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var out bulkItemsResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("parse bulk response: %v: %s", err, rr.Body.String())
		}
		if len(out.Failed) > 0 {
			t.Fatalf("a bulk move REFUSED a carried value; carried values drop: %+v", out.Failed)
		}
		if v, ok := f.storedRelation(item.ID); ok {
			t.Fatalf("an unresolvable relation survived the bulk move as %#v", v)
		}
	})
}

// --- doors 7-8: the cross-workspace pair -------------------------------
//
// Driven through the fixture the pin uses, so the table's door list is
// complete. The AGREEMENT between them is the pin's claim, not this table's.

func TestRelationDoors_CrossWorkspaceCopyAndPreflight(t *testing.T) {
	t.Run("copy: carried drops, supplied unresolvable refuses", func(t *testing.T) {
		f := newCopyRelationFixture(t)

		res := f.copyOK(f.baseBody())
		if v, present := f.persistedFields(res.Item.ID)["owner_ref"]; present {
			t.Fatalf("the copy carried a source-workspace referent: %#v", v)
		}

		f2 := newCopyRelationFixture(t)
		body := f2.baseBody()
		body["field_overrides"] = map[string]any{"owner_ref": badRef}
		assertRefused(t, "copy", f2.callCopy(f2.owner, reqOpts{}, body))
	})

	t.Run("preflight: carried drops, supplied unresolvable refuses", func(t *testing.T) {
		f := newCopyRelationFixture(t)

		pre := f.ok(f.baseBody())
		if _, carried := carriedValue(pre, "owner_ref"); carried {
			t.Fatalf("the preflight reports a source-workspace referent as carrying: %+v", pre.Fields)
		}

		body := f.baseBody()
		body["field_overrides"] = map[string]any{"owner_ref": badRef}
		assertRefused(t, "preflight", f.call(f.owner, reqOpts{}, body))
	})
}

// A supplied relation override at the MOVE door must name an item the
// requester can see (codex round 1, P1).
//
// The write doors have always checked this; the migrate doors call the store
// resolver directly, and the store cannot answer a request-scoped question.
// Same workspace here, so the role stashed for the request is the right one —
// the cross-workspace half of this rule, where it is NOT, is pinned in
// TestCopyEndpoint_InvisibleRelationOverrideIsRefused.
func TestRelationDoors_MoveRefusesInvisibleOverride(t *testing.T) {
	f := newDoorFixture(t)
	dst := f.targetCollection()
	item := f.seed(`{"status":"open"}`)

	// An editor who can see the two task collections but NOT People.
	blind := mustUser(t, f.srv, "blind-move@example.com", "blindmove", "")
	if err := f.srv.store.AddWorkspaceMember(f.ws.ID, blind.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	if err := f.srv.store.SetMemberCollectionAccess(f.ws.ID, blind.ID, "specific",
		[]string{f.tasks.ID, dst.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}

	body := map[string]any{
		"target_collection": dst.Slug,
		"field_overrides":   map[string]any{"owner_ref": f.target.Ref},
	}

	rr := f.callAs(blind, "editor", f.srv.handleMoveItem, "POST",
		"/api/v1/workspaces/"+f.ws.Slug+"/items/"+item.Slug+"/move",
		map[string]string{"itemSlug": item.Slug}, body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an override naming an item the caller cannot see, got %d: %s",
			rr.Code, rr.Body.String())
	}
	if code := errCode(t, rr); code != "validation_error" {
		t.Fatalf("error code = %q, want validation_error", code)
	}

	// Control: the owner, who can see People, gets the same override through.
	// Without this the test passes against a build that refuses every override.
	item2 := f.seed(`{"status":"open"}`)
	body2 := map[string]any{
		"target_collection": dst.Slug,
		"field_overrides":   map[string]any{"owner_ref": f.target.Ref},
	}
	if rr := f.call(f.srv.handleMoveItem, "POST",
		"/api/v1/workspaces/"+f.ws.Slug+"/items/"+item2.Slug+"/move",
		map[string]string{"itemSlug": item2.Slug}, body2); rr.Code != http.StatusOK {
		t.Fatalf("the owner's identical override was refused %d — the check is not "+
			"visibility-dependent: %s", rr.Code, rr.Body.String())
	}
}

// A BULK collection move that discards field values must say so (codex round
// 1, P1).
//
// `bulkMoveCollection` has populated `result.Dropped` since MigrateFields
// existed and NOTHING read it — BUG-2674 fixed the single-item door only, so
// the bulk door discarded values with no record anywhere. Wiring relation
// drops into that same dead list is what made it worth fixing rather than
// noting.
//
// Asserted on the ACTIVITY ROW, not the response: the bulk response is a
// per-item outcome list with no room for this, and the timeline is where
// someone asking "what happened to my item" actually looks.
func TestRelationDoors_BulkMoveReportsDroppedFields(t *testing.T) {
	f := newDoorFixture(t)
	dst := f.targetCollection()
	item := f.seed(fmt.Sprintf(`{"status":"open","owner_ref":%q}`, badRef))

	rr := f.call(f.srv.handleBulkItems, "POST",
		"/api/v1/workspaces/"+f.ws.Slug+"/items/bulk", nil,
		map[string]any{"op": "move", "ids": []string{item.ID}, "collection": dst.Slug})
	if rr.Code != http.StatusOK {
		t.Fatalf("bulk move: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if v, ok := f.storedRelation(item.ID); ok {
		t.Fatalf("an unresolvable relation survived the bulk move as %#v", v)
	}

	acts, err := f.srv.store.ListDocumentActivity(item.ID, models.ActivityListParams{Limit: 20})
	if err != nil {
		t.Fatalf("ListDocumentActivity: %v", err)
	}
	var moved *models.Activity
	for i := range acts {
		if acts[i].Action == "moved" {
			moved = &acts[i]
			break
		}
	}
	if moved == nil {
		t.Fatalf("no `moved` activity row for a bulk collection move: %+v", acts)
	}
	if !strings.Contains(moved.Metadata, "dropped_fields") || !strings.Contains(moved.Metadata, "owner_ref") {
		t.Fatalf("the bulk move discarded owner_ref without naming it in the activity row: %s",
			moved.Metadata)
	}
}

// callAs is `call` with an explicit user and workspace role, for the legs that
// need someone other than the fixture owner.
func (f *doorFixture) callAs(user *models.User, role string, h http.HandlerFunc, method, path string, params map[string]string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			f.t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	r := httptest.NewRequest(method, path, reader)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	ctx := WithCurrentUser(r.Context(), user)
	ctx = contextWithWorkspaceRoleForTest(ctx, role)
	ctx = contextWithResolvedWorkspaceIDForTest(ctx, f.ws.ID)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("slug", f.ws.Slug)
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	rr := httptest.NewRecorder()
	h(rr, r.WithContext(ctx))
	return rr
}

// `wrong_collection` names a LIVE item, so its message tells the caller the
// value exists — distinguishable from the `not_found` a nonexistent value
// gets, and therefore an existence oracle for anyone who cannot see that item
// (codex round 3).
//
// Both legs are required. Collapsing every wrong_collection to not_found would
// pass the first and destroy the second, and "you linked a task where a person
// belongs" is the useful half of this reason.
func TestRelationDoors_WrongCollectionDoesNotDiscloseExistence(t *testing.T) {
	f := newDoorFixture(t)
	// A live item in a collection that is NOT the relation's declared target.
	other := mustSchemaCollection(t, f.srv, f.ws.ID, "Vaults", `{"fields":[]}`)
	secret, err := f.srv.store.CreateItem(f.ws.ID, other.ID, models.ItemCreate{
		Title: "Secret", CreatedBy: f.owner.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem(secret): %v", err)
	}

	body := map[string]any{"title": "New", "fields": map[string]any{"owner_ref": secret.Ref}}
	path := "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + f.tasks.Slug + "/items"
	params := map[string]string{"collSlug": f.tasks.Slug}

	// The owner can see Vaults, so they get the specific, useful reason.
	seeing := f.call(f.srv.handleCreateItem, "POST", path, params, body)
	if seeing.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", seeing.Code, seeing.Body.String())
	}
	if !strings.Contains(seeing.Body.String(), "is not an item in collection") {
		t.Fatalf("a caller who CAN see the target lost the wrong_collection reason: %s",
			seeing.Body.String())
	}

	// An editor with no access to Vaults must not be able to tell that
	// `secret.Ref` names anything at all.
	blind := mustUser(t, f.srv, "blind-oracle@example.com", "blindoracle", "")
	if err := f.srv.store.AddWorkspaceMember(f.ws.ID, blind.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	if err := f.srv.store.SetMemberCollectionAccess(f.ws.ID, blind.ID, "specific",
		[]string{f.tasks.ID, f.people.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}

	hidden := f.callAs(blind, "editor", f.srv.handleCreateItem, "POST", path, params, body)
	if hidden.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", hidden.Code, hidden.Body.String())
	}
	if strings.Contains(hidden.Body.String(), "is not an item in collection") {
		t.Fatalf("the refusal tells a caller who cannot see the target that it EXISTS: %s",
			hidden.Body.String())
	}
	if !strings.Contains(hidden.Body.String(), "does not name an item") {
		t.Fatalf("expected the not_found phrasing, got: %s", hidden.Body.String())
	}
}

// A bulk status or priority change must resolve a relation default validation
// injects (codex round 3).
//
// `bulkFieldUpdate` looks only at the keys `changes` names — correctly, since
// re-litigating stored values would freeze legacy items — but `ValidateFields`
// runs first and INJECTS schema defaults, so a defaulted relation was
// persisted raw: never canonicalised, never checked against its collection.
func TestRelationDoors_BulkUpdateResolvesInjectedRelationDefault(t *testing.T) {
	f := newDoorFixture(t)
	defaulted := mustSchemaCollection(t, f.srv, f.ws.ID, "Defaulted", fmt.Sprintf(`{"fields":[
		{"key":"status","label":"Status","type":"select","options":["open","done"]},
		{"key":"priority","label":"Priority","type":"select","options":["low","high"]},
		{"key":"owner_ref","label":"Owner","type":"relation","collection":%q,"default":%q}
	]}`, f.people.Slug, f.target.Ref))

	item, err := f.srv.store.CreateItem(f.ws.ID, defaulted.ID, models.ItemCreate{
		Title: "Needs a default", Fields: `{"status":"open"}`, CreatedBy: f.owner.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	rr := f.call(f.srv.handleBulkItems, "POST",
		"/api/v1/workspaces/"+f.ws.Slug+"/items/bulk", nil,
		map[string]any{"op": "set-priority", "ids": []string{item.ID}, "priority": "high"})
	if rr.Code != http.StatusOK {
		t.Fatalf("bulk set-priority: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// The default is declared as a REF; only a resolved one lands as the id.
	if v, ok := f.storedRelation(item.ID); !ok || v != f.target.ID {
		t.Fatalf("stored owner_ref = %#v (present=%v), want the default RESOLVED to %q; the raw "+
			"ref %q means validation injected it after the relation pass had finished",
			v, ok, f.target.ID, f.target.Ref)
	}
}

// A whitespace-only relation value is "no reference", not a bad one (codex
// round 6).
//
// The store resolver trims and ignores it. The server wrapper checked the
// UNTRIMMED string, so it fell through to the visibility loop — and since the
// vanished-target arm turns a missing lookup into a refusal, `"   "` came back
// as not_found instead of an empty field. A defect my own round-1 fix
// introduced: before it, the same path did `continue`.
func TestRelationDoors_WhitespaceOnlyRelationIsNotRefused(t *testing.T) {
	f := newDoorFixture(t)

	rr := f.call(f.srv.handleCreateItem, "POST",
		"/api/v1/workspaces/"+f.ws.Slug+"/collections/"+f.tasks.Slug+"/items",
		map[string]string{"collSlug": f.tasks.Slug},
		map[string]any{"title": "Blank relation", "fields": map[string]any{"owner_ref": "   "}})
	if rr.Code != http.StatusCreated {
		t.Fatalf("a whitespace-only relation was refused %d; the store treats it as no value "+
			"at all: %s", rr.Code, rr.Body.String())
	}
}

// The write doors have the same injected-default hole the migrate doors had
// (codex round 7).
//
// `ValidateFields` assigns a schema default and `continue`s past its own type
// check, and the resolver skips a non-string — so `default: 42` on a relation
// field reached the blob unchallenged at create and at a full `fields` update.
// A value the CALLER supplies is type-checked and refused like any other; only
// the default escapes.
//
// Dropped rather than refused, because nobody in the request typed it: refusing
// would make every write into that collection fail on a schema defect its
// author has to fix elsewhere. Reported in the write's warnings, so the drop is
// not silent.
func TestRelationDoors_NonStringDefaultIsDroppedAndReportedOnWrite(t *testing.T) {
	f := newDoorFixture(t)
	bad := mustSchemaCollection(t, f.srv, f.ws.ID, "Bad Default", fmt.Sprintf(`{"fields":[
		{"key":"status","label":"Status","type":"select","options":["open","done"]},
		{"key":"owner_ref","label":"Owner","type":"relation","collection":%q,"default":42}
	]}`, f.people.Slug))

	rr := f.call(f.srv.handleCreateItem, "POST",
		"/api/v1/workspaces/"+f.ws.Slug+"/collections/"+bad.Slug+"/items",
		map[string]string{"collSlug": bad.Slug},
		map[string]any{"title": "Defaulted", "fields": map[string]any{"status": "open"}})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var out struct {
		ID       string `json:"id"`
		Warnings *struct {
			DroppedFields []string `json:"dropped_fields"`
		} `json:"warnings"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("parse create result: %v: %s", err, rr.Body.String())
	}

	item, err := f.srv.store.GetItem(out.ID)
	if err != nil || item == nil {
		t.Fatalf("GetItem: %v", err)
	}
	var stored map[string]any
	if err := json.Unmarshal([]byte(item.Fields), &stored); err != nil {
		t.Fatalf("parse fields: %v", err)
	}
	if v, present := stored["owner_ref"]; present {
		t.Fatalf("a non-reference default was stored in a relation field: %#v", v)
	}

	// Dropped is not the same as silently dropped.
	if out.Warnings == nil || len(out.Warnings.DroppedFields) == 0 {
		t.Fatalf("the write discarded owner_ref without reporting it: %s", rr.Body.String())
	}
	var named bool
	for _, k := range out.Warnings.DroppedFields {
		if k == "owner_ref" {
			named = true
		}
	}
	if !named {
		t.Fatalf("warnings.dropped_fields does not name owner_ref: %v", out.Warnings.DroppedFields)
	}
}

// An unresolvable STRING default must not refuse the write (codex round 10).
//
// Validation injects the default before the resolver runs, and the resolver
// treats the whole map as caller input — so an optional relation whose schema
// default names nothing turned every create and full-fields update into a 400,
// on a defect the caller cannot fix and did not cause. The migrate doors always
// dropped it and said so; the write doors refused.
//
// Both legs matter. The caller's OWN bad value must still be refused, or this
// fix would have quietly disabled the door.
func TestRelationDoors_UnresolvableStringDefaultDropsInsteadOfRefusing(t *testing.T) {
	f := newDoorFixture(t)
	coll := mustSchemaCollection(t, f.srv, f.ws.ID, "Dangling Default", fmt.Sprintf(`{"fields":[
		{"key":"status","label":"Status","type":"select","options":["open","done"]},
		{"key":"owner_ref","label":"Owner","type":"relation","collection":%q,"default":%q}
	]}`, f.people.Slug, badRef))
	path := "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + coll.Slug + "/items"
	params := map[string]string{"collSlug": coll.Slug}

	rr := f.call(f.srv.handleCreateItem, "POST", path, params,
		map[string]any{"title": "Defaulted", "fields": map[string]any{"status": "open"}})
	if rr.Code != http.StatusCreated {
		t.Fatalf("a dangling schema DEFAULT refused the create %d; the caller neither typed it "+
			"nor can fix it from here: %s", rr.Code, rr.Body.String())
	}
	var out struct {
		ID       string `json:"id"`
		Warnings *struct {
			DroppedFields []string `json:"dropped_fields"`
		} `json:"warnings"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("parse create result: %v: %s", err, rr.Body.String())
	}
	if v, ok := f.storedRelation(out.ID); ok {
		t.Fatalf("the dangling default was stored: %#v", v)
	}
	if out.Warnings == nil || len(out.Warnings.DroppedFields) == 0 {
		t.Fatalf("dropped without saying so: %s", rr.Body.String())
	}

	// The caller's OWN unresolvable value is still refused — without this leg
	// the test passes against a door that stopped checking anything.
	bad := f.call(f.srv.handleCreateItem, "POST", path, params,
		map[string]any{"title": "Typed by hand", "fields": map[string]any{"status": "open", "owner_ref": badRef}})
	assertRefused(t, "create with a caller-supplied bad referent", bad)
}

// A schema default naming an item the caller cannot see must not hand them its
// id (codex round 11).
//
// Round 10 filtered the caller's-input-only refusals, which correctly stopped
// a default refusing the write — and also removed the VISIBILITY issues raised
// against those keys. `ResolveLateRelationDefaults` then re-resolved them in
// the store, which has no visibility layer, and the write's response carries
// the item's fields: the caller received the canonical id of an item they
// cannot see.
//
// Dropped, not refused: the schema author chose the value and the caller can
// neither fix it nor be blamed for it. What they must not get is the id.
func TestRelationDoors_InvisibleRelationDefaultIsNotDisclosed(t *testing.T) {
	f := newDoorFixture(t)
	coll := mustSchemaCollection(t, f.srv, f.ws.ID, "Hidden Default", fmt.Sprintf(`{"fields":[
		{"key":"status","label":"Status","type":"select","options":["open","done"]},
		{"key":"owner_ref","label":"Owner","type":"relation","collection":%q,"default":%q}
	]}`, f.people.Slug, f.target.Ref))

	blind := mustUser(t, f.srv, "blind-default@example.com", "blinddefault", "")
	if err := f.srv.store.AddWorkspaceMember(f.ws.ID, blind.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	if err := f.srv.store.SetMemberCollectionAccess(f.ws.ID, blind.ID, "specific",
		[]string{coll.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}

	path := "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + coll.Slug + "/items"
	params := map[string]string{"collSlug": coll.Slug}
	body := map[string]any{"title": "Hidden", "fields": map[string]any{"status": "open"}}

	rr := f.callAs(blind, "editor", f.srv.handleCreateItem, "POST", path, params, body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: expected 201 (a default is dropped, never refused), got %d: %s",
			rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), f.target.ID) {
		t.Fatalf("the response hands a caller who cannot see People the target's id: %s",
			rr.Body.String())
	}

	// Control: the owner CAN see People, so the same default resolves and
	// lands. Without this leg the test passes against a build that dropped
	// every default.
	rr2 := f.call(f.srv.handleCreateItem, "POST", path, params, body)
	if rr2.Code != http.StatusCreated {
		t.Fatalf("create as owner: expected 201, got %d: %s", rr2.Code, rr2.Body.String())
	}
	if !strings.Contains(rr2.Body.String(), f.target.ID) {
		t.Fatalf("the owner's identical create lost the default; the drop is not "+
			"visibility-dependent: %s", rr2.Body.String())
	}
}

// `req.Status` on a bulk collection move is CALLER INPUT (codex round 11).
//
// The path merges exactly one field and passed `supplied=nil` on the grounds
// that it carries no per-field overrides — true of every field except that
// one. A destination schema is free to declare `status` as a relation, and
// then a value the caller typed was classified as CARRIED: silently dropped
// instead of refused, and never checked for visibility.
//
// The refusal branch on that door had been written as "unreachable today, kept
// so it stops being a silent no-op" — which is exactly why this survived. A
// branch nobody can reach is a branch nobody checks.
func TestRelationDoors_BulkMoveStatusIsCallerInput(t *testing.T) {
	f := newDoorFixture(t)
	// A destination whose `status` is a relation rather than a select.
	dst := mustSchemaCollection(t, f.srv, f.ws.ID, "Status As Relation", fmt.Sprintf(`{"fields":[
		{"key":"status","label":"Status","type":"relation","collection":%q}
	]}`, f.people.Slug))
	item := f.seed(`{"status":"open"}`)

	rr := f.call(f.srv.handleBulkItems, "POST",
		"/api/v1/workspaces/"+f.ws.Slug+"/items/bulk", nil,
		map[string]any{"op": "move", "ids": []string{item.ID}, "collection": dst.Slug, "status": badRef})
	if rr.Code != http.StatusOK {
		t.Fatalf("bulk move: expected 200 with a per-item failure, got %d: %s", rr.Code, rr.Body.String())
	}
	var out bulkItemsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("parse bulk response: %v: %s", err, rr.Body.String())
	}
	if len(out.Failed) == 0 {
		t.Fatalf("the caller's own unresolvable status was accepted; a supplied value is "+
			"REFUSED, not dropped: %+v", out)
	}
	if got := out.Failed[0].Code; got != "validation_error" {
		t.Fatalf("failure code = %q, want validation_error (message: %s)", got, out.Failed[0].Error)
	}
}

// A MIGRATE door's destination default gets the visibility check too (codex
// round 13).
//
// The first version of `dropInvisibleRelationDefaults` skipped keys "present
// before the late pass". At a migrate door that set includes the defaults
// `MigrateFields` injected — so exactly the values the helper exists to check
// were the ones it skipped, and only the write doors were ever covered. The
// predicate is now "not a destination default": the caller's own values and
// the values carried from the source, both excluded for their own reasons.
func TestRelationDoors_MoveDropsInvisibleDestinationDefault(t *testing.T) {
	f := newDoorFixture(t)
	dst := mustSchemaCollection(t, f.srv, f.ws.ID, "Move Hidden Default", fmt.Sprintf(`{"fields":[
		{"key":"status","label":"Status","type":"select","options":["open","done"]},
		{"key":"owner_ref","label":"Owner","type":"relation","collection":%q,"default":%q}
	]}`, f.people.Slug, f.target.Ref))
	item := f.seed(`{"status":"open"}`)

	blind := mustUser(t, f.srv, "blind-move-default@example.com", "blindmovedef", "")
	if err := f.srv.store.AddWorkspaceMember(f.ws.ID, blind.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	if err := f.srv.store.SetMemberCollectionAccess(f.ws.ID, blind.ID, "specific",
		[]string{f.tasks.ID, dst.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}

	rr := f.callAs(blind, "editor", f.srv.handleMoveItem, "POST",
		"/api/v1/workspaces/"+f.ws.Slug+"/items/"+item.Slug+"/move",
		map[string]string{"itemSlug": item.Slug},
		map[string]any{"target_collection": dst.Slug})
	if rr.Code != http.StatusOK {
		t.Fatalf("move: expected 200 (a default is dropped, never refused), got %d: %s",
			rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), f.target.ID) {
		t.Fatalf("the move handed a caller who cannot see People the target's id: %s", rr.Body.String())
	}
	if v, ok := f.storedRelation(item.ID); ok {
		t.Fatalf("the hidden default was stored: %#v", v)
	}

	// Control: the owner sees People, so the same move keeps the default.
	item2 := f.seed(`{"status":"open"}`)
	rr2 := f.call(f.srv.handleMoveItem, "POST",
		"/api/v1/workspaces/"+f.ws.Slug+"/items/"+item2.Slug+"/move",
		map[string]string{"itemSlug": item2.Slug},
		map[string]any{"target_collection": dst.Slug})
	if rr2.Code != http.StatusOK {
		t.Fatalf("move as owner: expected 200, got %d: %s", rr2.Code, rr2.Body.String())
	}
	if v, ok := f.storedRelation(item2.ID); !ok || v != f.target.ID {
		t.Fatalf("the owner's move lost the default (%#v, present=%v); the drop is not "+
			"visibility-dependent", v, ok)
	}
}

// An invisible default in a REQUIRED relation refuses (codex round 13).
//
// The required check was written for the late-dropped list and a sibling list
// — the visibility drops — was added beside it a round later. Deleting a key
// after validation has passed leaves a required field absent regardless of
// which list recorded it, so the check has to cover both.
func TestRelationDoors_RequiredInvisibleDefaultRefuses(t *testing.T) {
	f := newDoorFixture(t)
	coll := mustSchemaCollection(t, f.srv, f.ws.ID, "Required Hidden", fmt.Sprintf(`{"fields":[
		{"key":"status","label":"Status","type":"select","options":["open","done"]},
		{"key":"owner_ref","label":"Owner","type":"relation","collection":%q,"default":%q,"required":true}
	]}`, f.people.Slug, f.target.Ref))

	blind := mustUser(t, f.srv, "blind-required@example.com", "blindrequired", "")
	if err := f.srv.store.AddWorkspaceMember(f.ws.ID, blind.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	if err := f.srv.store.SetMemberCollectionAccess(f.ws.ID, blind.ID, "specific",
		[]string{coll.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}

	path := "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + coll.Slug + "/items"
	params := map[string]string{"collSlug": coll.Slug}
	body := map[string]any{"title": "Required hidden", "fields": map[string]any{"status": "open"}}

	rr := f.callAs(blind, "editor", f.srv.handleCreateItem, "POST", path, params, body)
	if rr.Code == http.StatusCreated {
		t.Fatalf("the item was created with a REQUIRED relation dropped for visibility; "+
			"dropping there stores the item with the field absent: %s", rr.Body.String())
	}
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}

	// Control: the owner can see the target, so the same create succeeds.
	if rr2 := f.call(f.srv.handleCreateItem, "POST", path, params, body); rr2.Code != http.StatusCreated {
		t.Fatalf("the owner's identical create was refused %d: %s", rr2.Code, rr2.Body.String())
	}
}

// A NIL source value does not make the key "carried", so the destination
// default that replaces it still gets the visibility check (codex round 14).
//
// `notDefaultKeys` counted every key present in the supplied or carried map,
// nil included. `ValidateFields` treats a present-but-nil key as ABSENT and
// injects the destination default in its place — so a nil counted the key out
// of exactly the check the injected default needs. Third time in this unit
// that a nil has been mistaken for a value; rounds 4 and 5 were the same
// distinction in the origin label.
func TestRelationDoors_NullSourceDoesNotExemptDefaultFromVisibility(t *testing.T) {
	f := newDoorFixture(t)
	dst := mustSchemaCollection(t, f.srv, f.ws.ID, "Null Then Hidden Default", fmt.Sprintf(`{"fields":[
		{"key":"status","label":"Status","type":"select","options":["open","done"]},
		{"key":"owner_ref","label":"Owner","type":"relation","collection":%q,"default":%q}
	]}`, f.people.Slug, f.target.Ref))
	// The source HOLDS the key, as an explicit null.
	item := f.seed(`{"status":"open","owner_ref":null}`)

	blind := mustUser(t, f.srv, "blind-null@example.com", "blindnull", "")
	if err := f.srv.store.AddWorkspaceMember(f.ws.ID, blind.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	if err := f.srv.store.SetMemberCollectionAccess(f.ws.ID, blind.ID, "specific",
		[]string{f.tasks.ID, dst.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}

	rr := f.callAs(blind, "editor", f.srv.handleMoveItem, "POST",
		"/api/v1/workspaces/"+f.ws.Slug+"/items/"+item.Slug+"/move",
		map[string]string{"itemSlug": item.Slug},
		map[string]any{"target_collection": dst.Slug})
	if rr.Code != http.StatusOK {
		t.Fatalf("move: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), f.target.ID) {
		t.Fatalf("a null source value exempted the destination default from the visibility "+
			"check, and the caller got the hidden target's id: %s", rr.Body.String())
	}
	if v, ok := f.storedRelation(item.ID); ok {
		t.Fatalf("the hidden default was stored: %#v", v)
	}
}

// A relation issue raised by the LATE default pass must pass through the same
// visibility collapse the main pass applies (codex round 15).
//
// `store.ResolveLateRelationDefaults` cannot know who is asking, so it returns
// the raw reason. `wrong_collection` is the one reason that names a LIVE item,
// and every door renders these issues into a caller-visible message — so a
// schema default pointing at an item the caller cannot see announced that the
// item EXISTS. Same oracle round 3 closed on the main pass, reopened through
// the late-default door round 10 added.
func TestRelationDoors_LateDefaultWrongCollectionDoesNotDiscloseExistence(t *testing.T) {
	f := newDoorFixture(t)
	other := mustSchemaCollection(t, f.srv, f.ws.ID, "Late Vaults", `{"fields":[]}`)
	secret, err := f.srv.store.CreateItem(f.ws.ID, other.ID, models.ItemCreate{
		Title: "Late Secret", CreatedBy: f.owner.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem(secret): %v", err)
	}

	// REQUIRED, so the unresolved default becomes a refusal the caller reads
	// rather than a silent drop.
	dst := mustSchemaCollection(t, f.srv, f.ws.ID, "Late Default Door", fmt.Sprintf(`{"fields":[
		{"key":"status","label":"Status","type":"select","options":["open","done"]},
		{"key":"owner_ref","label":"Owner","type":"relation","collection":%q,"required":true,"default":%q}
	]}`, f.people.Slug, secret.Ref))

	move := func(u *models.User, role string) string {
		t.Helper()
		item := f.seed(`{"status":"open"}`)
		rr := f.callAs(u, role, f.srv.handleMoveItem, "POST",
			"/api/v1/workspaces/"+f.ws.Slug+"/items/"+item.Slug+"/move",
			map[string]string{"itemSlug": item.Slug},
			map[string]any{"target_collection": dst.Slug})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for an unresolved REQUIRED relation default, got %d: %s",
				rr.Code, rr.Body.String())
		}
		return rr.Body.String()
	}

	// CONTROL: a caller who CAN see the target keeps the specific, useful
	// reason. Without this leg, collapsing every reason to not_found would
	// pass the assertion below while destroying the message's value.
	seeing := move(f.owner, "owner")
	if !strings.Contains(seeing, "is not an item in collection") {
		t.Fatalf("a caller who CAN see the target lost the wrong_collection reason: %s", seeing)
	}

	blind := mustUser(t, f.srv, "blind-late@example.com", "blindlate", "")
	if err := f.srv.store.AddWorkspaceMember(f.ws.ID, blind.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	if err := f.srv.store.SetMemberCollectionAccess(f.ws.ID, blind.ID, "specific",
		[]string{f.tasks.ID, dst.ID, f.people.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}

	hidden := move(blind, "editor")
	if strings.Contains(hidden, "is not an item in collection") {
		t.Fatalf("the late-default refusal tells a caller who cannot see the target that it "+
			"EXISTS: %s", hidden)
	}
	if strings.Contains(hidden, secret.ID) {
		t.Fatalf("the late-default refusal handed back the hidden item's canonical id: %s", hidden)
	}
}

// A BULK move owes its supplied half the same visibility check the single
// move door runs (codex round 16).
//
// Round 11 established that `req.Status` on a bulk collection move is CALLER
// INPUT and wired `suppliedByCaller` so the store classifier would refuse an
// unresolvable value instead of dropping it. What that round did not carry
// across is the OTHER half of the supplied contract: the single move door,
// the copy, and the preflight all call refuseInvisibleRelationOverrides
// before handing the values to the store, because the store resolver cannot
// answer a request-scoped question. Bulk move is the fourth migrate door and
// the only one that never called it — the N+1th site of a rule three doors
// already applied.
//
// So a caller who cannot see the relation's target collection could name a
// live item in it and have the value stored, receiving its canonical id back.
func TestRelationDoors_BulkMoveRefusesInvisibleSuppliedStatus(t *testing.T) {
	f := newDoorFixture(t)
	dst := mustSchemaCollection(t, f.srv, f.ws.ID, "Bulk Status Relation", fmt.Sprintf(`{"fields":[
		{"key":"status","label":"Status","type":"relation","collection":%q}
	]}`, f.people.Slug))

	blind := mustUser(t, f.srv, "blind-bulk-status@example.com", "blindbulkstatus", "")
	if err := f.srv.store.AddWorkspaceMember(f.ws.ID, blind.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	// Sees the source and the destination; NOT People.
	if err := f.srv.store.SetMemberCollectionAccess(f.ws.ID, blind.ID, "specific",
		[]string{f.tasks.ID, dst.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}

	item := f.seed(`{"status":"open"}`)
	rr := f.callAs(blind, "editor", f.srv.handleBulkItems, "POST",
		"/api/v1/workspaces/"+f.ws.Slug+"/items/bulk", nil,
		map[string]any{"op": "move", "ids": []string{item.ID}, "collection": dst.Slug,
			"status": f.target.Ref})
	if rr.Code != http.StatusOK {
		t.Fatalf("bulk move: expected 200 with a per-item failure, got %d: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), f.target.ID) {
		t.Fatalf("the bulk move handed a caller who cannot see People the target's id: %s",
			rr.Body.String())
	}
	if v, ok := f.storedRelationKey(item.ID, "status"); ok && v == f.target.ID {
		t.Fatalf("a caller who cannot see People pointed a relation at one of its items: %#v", v)
	}
	var out bulkItemsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("parse bulk response: %v: %s", err, rr.Body.String())
	}
	if len(out.Failed) == 0 {
		t.Fatalf("a supplied value naming an item the caller cannot see was accepted: %+v", out)
	}

	// Control: the OWNER can see People, so the identical bulk move lands and
	// stores the canonical id. Without this leg the test passes against a
	// build that refused every supplied status.
	item2 := f.seed(`{"status":"open"}`)
	rr2 := f.call(f.srv.handleBulkItems, "POST",
		"/api/v1/workspaces/"+f.ws.Slug+"/items/bulk", nil,
		map[string]any{"op": "move", "ids": []string{item2.ID}, "collection": dst.Slug,
			"status": f.target.Ref})
	if rr2.Code != http.StatusOK {
		t.Fatalf("bulk move as owner: expected 200, got %d: %s", rr2.Code, rr2.Body.String())
	}
	if v, ok := f.storedRelationKey(item2.ID, "status"); !ok || v != f.target.ID {
		t.Fatalf("the owner's identical bulk move did not store the relation (%#v, present=%v); "+
			"the refusal above is not visibility-dependent", v, ok)
	}
}

// The preflight's CARRIED relation drops owe the same visibility collapse the
// late-default drops get (codex round 16).
//
// Round 15 hoisted the collapse so `ResolveLateRelationDefaults`' issues stop
// naming live items to callers who cannot see them. `MigrateRelationReferents`
// returns its issues down a second path — `relationDropReason`, rendered
// straight into `fields.dropped[].reason` — and that path never passed through
// the collapse. `wrong_collection` is the reason that names a LIVE item, so a
// carried value pointing at one the caller cannot see was reported
// differently from a value naming nothing: the existence oracle, on the
// carried path rather than the default path.
//
// The two legs are the whole test: a hidden-but-live target and a dangling
// value must produce the SAME reason for a caller who can see neither.
func TestRelationDoors_PreflightCarriedDropDoesNotDiscloseExistence(t *testing.T) {
	f := newDoorFixture(t)
	// A collection the restricted caller cannot see, holding a live item.
	secret := mustSchemaCollection(t, f.srv, f.ws.ID, "Secret People", `{"fields":[]}`)
	hidden, err := f.srv.store.CreateItem(f.ws.ID, secret.ID, models.ItemCreate{
		Title: "Hidden Person", CreatedBy: f.owner.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem(hidden): %v", err)
	}
	// The destination declares owner_ref against People, which the caller CAN
	// see — so the only thing hidden is the collection the source value
	// actually points into.
	dst := mustSchemaCollection(t, f.srv, f.ws.ID, "Preflight Dest", fmt.Sprintf(`{"fields":[
		{"key":"status","label":"Status","type":"select","options":["open","done"]},
		{"key":"owner_ref","label":"Owner","type":"relation","collection":%q}
	]}`, f.people.Slug))

	blind := mustUser(t, f.srv, "blind-carried-drop@example.com", "blindcarrieddrop", "")
	if err := f.srv.store.AddWorkspaceMember(f.ws.ID, blind.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	if err := f.srv.store.SetMemberCollectionAccess(f.ws.ID, blind.ID, "specific",
		[]string{f.tasks.ID, dst.ID, f.people.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}

	reasonFor := func(user *models.User, role, storedRef string) string {
		t.Helper()
		item := f.seed(fmt.Sprintf(`{"status":"open","owner_ref":%q}`, storedRef))
		rr := f.callAs(user, role, f.srv.handleCopyItemPreflight, "POST",
			"/api/v1/workspaces/"+f.ws.Slug+"/items/"+item.Slug+"/copy/preflight",
			map[string]string{"itemSlug": item.Slug},
			map[string]any{"target_workspace": f.ws.Slug, "target_collection": dst.Slug})
		if rr.Code != http.StatusOK {
			t.Fatalf("preflight (%s): got %d, want 200: %s", storedRef, rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), hidden.ID) && user == blind {
			t.Fatalf("the preflight handed a caller who cannot see Secret People the hidden "+
				"item's id: %s", rr.Body.String())
		}
		var pre ItemCopyPreflight
		if err := json.Unmarshal(rr.Body.Bytes(), &pre); err != nil {
			t.Fatalf("parse preflight: %v: %s", err, rr.Body.String())
		}
		for _, d := range pre.Fields.Dropped {
			if d.Key == "owner_ref" {
				return d.Reason
			}
		}
		t.Fatalf("preflight (%s) reported no drop for owner_ref: %+v", storedRef, pre.Fields)
		return ""
	}

	hiddenReason := reasonFor(blind, "editor", hidden.Ref)
	danglingReason := reasonFor(blind, "editor", badRef)
	if hiddenReason != danglingReason {
		t.Fatalf("a caller who can see neither target distinguishes them: a LIVE hidden item "+
			"reports %q and a nonexistent value reports %q — that difference is the existence "+
			"oracle", hiddenReason, danglingReason)
	}

	// Control: the OWNER can see Secret People, so they still get the
	// specific reason. Without this leg the test passes against a build that
	// collapsed every reason to not_found and told nobody anything.
	if got := reasonFor(f.owner, "owner", hidden.Ref); got == danglingReason {
		t.Fatalf("the owner's identical preflight also reports %q for a live item they CAN "+
			"see; the collapse is not visibility-dependent", got)
	}
}

// A NULL override does not make a stale non-nil source value stop counting as
// carried (codex round 16).
//
// `notDefaultKeys` already knows that a nil value is not a value — round 14
// taught it that, because ValidateFields treats a present-but-nil key as
// absent and injects the destination default in its place. What it checks is
// the SUPPLIED and CARRIED maps; the carried map here is built from the
// item's STORED blob, which holds a non-nil legacy value. So a caller who
// nulls the key gets the default injected AND the key exempted from the
// visibility check, on the strength of a stored value the request just
// discarded.
//
// The relation pass does not rescue this: an override of nil leaves nothing
// for it to resolve, so the key is never dropped and never leaves the carried
// set. The existing null-source test does not reach it — that fixture has no
// stored value to go stale.
func TestRelationDoors_NullOverrideDoesNotExemptDefaultFromVisibility(t *testing.T) {
	f := newDoorFixture(t)
	dst := mustSchemaCollection(t, f.srv, f.ws.ID, "Null Override Dest", fmt.Sprintf(`{"fields":[
		{"key":"status","label":"Status","type":"select","options":["open","done"]},
		{"key":"owner_ref","label":"Owner","type":"relation","collection":%q,"default":%q,"required":true}
	]}`, f.people.Slug, f.target.Ref))

	blind := mustUser(t, f.srv, "blind-null-override@example.com", "blindnulloverride", "")
	if err := f.srv.store.AddWorkspaceMember(f.ws.ID, blind.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	if err := f.srv.store.SetMemberCollectionAccess(f.ws.ID, blind.ID, "specific",
		[]string{f.tasks.ID, dst.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}

	// A non-nil STORED value, which is what goes stale. badRef so the value
	// itself is unresolvable and cannot be confused for the thing under test.
	item := f.seed(fmt.Sprintf(`{"status":"open","owner_ref":%q}`, badRef))
	rr := f.callAs(blind, "editor", f.srv.handleMoveItem, "POST",
		"/api/v1/workspaces/"+f.ws.Slug+"/items/"+item.Slug+"/move",
		map[string]string{"itemSlug": item.Slug},
		map[string]any{"target_collection": dst.Slug,
			"field_overrides": map[string]any{"owner_ref": nil}})
	if strings.Contains(rr.Body.String(), f.target.ID) {
		t.Fatalf("the move handed a caller who cannot see People the id of the item the "+
			"destination default names: %s", rr.Body.String())
	}
	if v, ok := f.storedRelation(item.ID); ok && v == f.target.ID {
		t.Fatalf("the hidden default was stored for a caller who cannot see People: %#v", v)
	}

	// Control: the OWNER can see People, so the same move keeps the default.
	// Without this leg the test passes against a build that dropped every
	// default, or refused every move.
	item2 := f.seed(fmt.Sprintf(`{"status":"open","owner_ref":%q}`, badRef))
	rr2 := f.call(f.srv.handleMoveItem, "POST",
		"/api/v1/workspaces/"+f.ws.Slug+"/items/"+item2.Slug+"/move",
		map[string]string{"itemSlug": item2.Slug},
		map[string]any{"target_collection": dst.Slug,
			"field_overrides": map[string]any{"owner_ref": nil}})
	if rr2.Code != http.StatusOK {
		t.Fatalf("move as owner: expected 200, got %d: %s", rr2.Code, rr2.Body.String())
	}
	if v, ok := f.storedRelation(item2.ID); !ok || v != f.target.ID {
		t.Fatalf("the owner's identical move did not store the default (%#v, present=%v); "+
			"the drop above is not visibility-dependent", v, ok)
	}
}

// A supplied value that SATISFIES a required destination field must not be
// refused by a required-field error computed before the override existed
// (codex round 19).
//
// PLAN-2357 DR-12 fixed exactly this at the SINGLE move door — the comment
// there records that `result.Errors` is computed by MigrateFields BEFORE any
// override, so an override that satisfied a required field still 400'd — and
// nobody swept the fix to the bulk door. It became reachable when round 11
// established that `req.Status` on a bulk move is caller input.
//
// The shape: a source `status` holding a select value cannot migrate into a
// destination `status` declared as a required relation, so MigrateFields drops
// it and records the required-field error. The caller supplies a perfectly
// good referent in the same request, and the move was refused for a field the
// request had just filled.
func TestRelationDoors_BulkMoveAcceptsASuppliedValueForARequiredRelation(t *testing.T) {
	f := newDoorFixture(t)
	dst := mustSchemaCollection(t, f.srv, f.ws.ID, "Bulk Required Relation", fmt.Sprintf(`{"fields":[
		{"key":"status","label":"Status","type":"relation","collection":%q,"required":true}
	]}`, f.people.Slug))

	item := f.seed(`{"status":"open"}`)
	rr := f.call(f.srv.handleBulkItems, "POST",
		"/api/v1/workspaces/"+f.ws.Slug+"/items/bulk", nil,
		map[string]any{"op": "move", "ids": []string{item.ID}, "collection": dst.Slug,
			"status": f.target.Ref})
	if rr.Code != http.StatusOK {
		t.Fatalf("bulk move: got %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var out bulkItemsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("parse bulk response: %v: %s", err, rr.Body.String())
	}
	if len(out.Failed) > 0 {
		t.Fatalf("the move was refused for a required field the SAME request supplied: %+v",
			out.Failed)
	}
	if v, ok := f.storedRelationKey(item.ID, "status"); !ok || v != f.target.ID {
		t.Fatalf("the supplied referent was not stored (%#v, present=%v), want %q",
			v, ok, f.target.ID)
	}

	// Control: with NOTHING supplied for the required relation, the move must
	// STILL be refused, and with the same code. Without this leg the test
	// passes against a build that stopped checking required fields at all.
	item2 := f.seed(`{"status":"open"}`)
	rr2 := f.call(f.srv.handleBulkItems, "POST",
		"/api/v1/workspaces/"+f.ws.Slug+"/items/bulk", nil,
		map[string]any{"op": "move", "ids": []string{item2.ID}, "collection": dst.Slug})
	var out2 bulkItemsResponse
	if err := json.Unmarshal(rr2.Body.Bytes(), &out2); err != nil {
		t.Fatalf("parse bulk response: %v: %s", err, rr2.Body.String())
	}
	if len(out2.Failed) == 0 {
		t.Fatalf("a move leaving a REQUIRED relation empty was accepted: %+v", out2)
	}
	if got := out2.Failed[0].Code; got != "missing_required_fields" {
		t.Fatalf("failure code = %q, want missing_required_fields — the filter must narrow "+
			"the error list, not replace the check (message: %s)", got, out2.Failed[0].Error)
	}
}
