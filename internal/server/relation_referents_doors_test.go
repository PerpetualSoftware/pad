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
			map[string]any{"owner_ref": badRef}, true, nil, f.owner.ID, "test", "batch")
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
			map[string]any{"owner_ref": f.target.Ref}, true, nil, f.owner.ID, "test", "batch"); opErr != nil {
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
