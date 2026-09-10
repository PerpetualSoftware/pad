package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Door-level legs for exact-title resolution and read hydration (PLAN-2857 U6).
//
// internal/store already tests the title ladder and the hydrator directly, so
// those vouch for the COMPONENTS. They say nothing about whether any door is
// BOUND to them — a create handler that never reaches the title rung, or a read
// handler that never hydrates, passes every store test. These are the binding
// claims, driven through the handlers a client actually reaches. Same
// separation, and same reason, as the U1 table above.

// TestRelationTitleDoors_ThreeSpellingsAgreeThroughTheCreateDoor is the design
// row's proving test at the door: uuid, ref and exact title, each through
// handleCreateItem, must land on an identical STORED id.
//
// Read back out of the database rather than off the response, for the reason
// storedRelation documents: a handler that echoed the right thing while writing
// the wrong thing would pass a response-only check.
func TestRelationTitleDoors_ThreeSpellingsAgreeThroughTheCreateDoor(t *testing.T) {
	f := newDoorFixture(t)
	path := "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + f.tasks.Slug + "/items"
	params := map[string]string{"collSlug": f.tasks.Slug}

	for _, supplied := range []string{f.target.ID, f.target.Ref, f.target.Title} {
		body := map[string]any{"title": "By " + supplied, "fields": map[string]any{"owner_ref": supplied}}
		rr := f.call(f.srv.handleCreateItem, "POST", path, params, body)
		if rr.Code != http.StatusCreated {
			t.Fatalf("supplied %q: expected 201, got %d: %s", supplied, rr.Code, rr.Body.String())
		}
		var created models.Item
		if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
			t.Fatalf("supplied %q: decode: %v", supplied, err)
		}
		stored, ok := f.storedRelation(created.ID)
		if !ok {
			t.Fatalf("supplied %q: no owner_ref stored", supplied)
		}
		if stored != f.target.ID {
			t.Errorf("supplied %q stored %v, want the canonical id %s — the door must agree with the other two spellings", supplied, stored, f.target.ID)
		}
	}
}

// TestRelationTitleDoors_TitleWrongCollectionDoesNotDiscloseExistence is
// ruling (3) on the TITLE path, and it is the leg the addendum was written for.
//
// The ref-path equivalent (TestRelationDoors_WrongCollectionDoesNotDisclose-
// Existence) collapses by RE-RESOLVING the value through ResolveRelationTarget.
// That ladder does not speak titles, so a title-derived wrong_collection would
// re-resolve to nil, take the not-found arm, and collapse for EVERY caller —
// visible and invisible alike. The invisible leg below would still pass. Only
// the VISIBLE leg catches it, which is why both are here and why the fix
// carries the matched id on the issue instead of looking it up twice.
func TestRelationTitleDoors_TitleWrongCollectionDoesNotDiscloseExistence(t *testing.T) {
	f := newDoorFixture(t)
	// A live item in a collection that is NOT the relation's declared target,
	// addressed by its TITLE.
	other := mustSchemaCollection(t, f.srv, f.ws.ID, "Vaults", `{"fields":[]}`)
	if _, err := f.srv.store.CreateItem(f.ws.ID, other.ID, models.ItemCreate{
		Title: "Ledger", CreatedBy: f.owner.ID,
	}); err != nil {
		t.Fatalf("CreateItem(ledger): %v", err)
	}

	body := map[string]any{"title": "New", "fields": map[string]any{"owner_ref": "Ledger"}}
	path := "/api/v1/workspaces/" + f.ws.Slug + "/collections/" + f.tasks.Slug + "/items"
	params := map[string]string{"collSlug": f.tasks.Slug}

	// VISIBLE: the owner can see Vaults, so they keep the specific reason. This
	// is the leg that dies if the collapse re-resolves the title.
	seeing := f.call(f.srv.handleCreateItem, "POST", path, params, body)
	if seeing.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", seeing.Code, seeing.Body.String())
	}
	if !strings.Contains(seeing.Body.String(), "is not an item in collection") {
		t.Fatalf("a caller who CAN see the target lost the wrong_collection reason — the collapse judged the wrong item, or could not find it at all: %s", seeing.Body.String())
	}

	// INVISIBLE: an editor with no access to Vaults must not learn that the
	// title names anything. A title is guessable in a way a ref is not, which
	// is why this leg exists at all.
	blind := mustUser(t, f.srv, "blind-title@example.com", "blindtitle", "")
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
		t.Fatalf("the refusal tells a caller who cannot see the target that the TITLE names something: %s", hidden.Body.String())
	}
	if !strings.Contains(hidden.Body.String(), "does not name an item") {
		t.Fatalf("expected the not_found phrasing, got: %s", hidden.Body.String())
	}
}

// TestRelationTitleDoors_ReadHydratesTheTarget is the hydration half's binding
// claim: the read door returns relation_targets, and a dangling value comes
// back ID-ONLY and PRESENT rather than omitted.
func TestRelationTitleDoors_ReadHydratesTheTarget(t *testing.T) {
	f := newDoorFixture(t)

	live := f.seed(`{"owner_ref":"` + f.target.ID + `"}`)
	dangling := f.seed(`{"owner_ref":"00000000-0000-4000-8000-00000000dead"}`)

	get := func(it *models.Item) models.Item {
		t.Helper()
		rr := f.call(f.srv.handleGetItem, "GET",
			"/api/v1/workspaces/"+f.ws.Slug+"/items/"+it.Slug,
			map[string]string{"itemSlug": it.Slug}, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("get %s: expected 200, got %d: %s", it.Slug, rr.Code, rr.Body.String())
		}
		var out models.Item
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v", it.Slug, err)
		}
		return out
	}

	got := get(live)
	target, ok := got.RelationTargets["owner_ref"]
	if !ok {
		t.Fatal("the read door returned no relation_targets — the hydrator is not bound to it, which every store-level test would still pass")
	}
	if target.ID != f.target.ID || target.Ref != f.target.Ref || target.Title != f.target.Title {
		t.Errorf("hydrated as %+v, want {id:%s ref:%s title:%s}", target, f.target.ID, f.target.Ref, f.target.Title)
	}

	got = get(dangling)
	target, ok = got.RelationTargets["owner_ref"]
	if !ok {
		t.Fatal("a dangling value was OMITTED; omission reads as \"this item has no relation\", which is a different and false statement")
	}
	if target.Ref != "" || target.Title != "" {
		t.Errorf("a dangling value hydrated as %+v, want id-only", target)
	}
	if target.ID == "" {
		t.Error("a dangling entry lost the stored id, which is the only thing the caller had")
	}
}

// TestRelationTitleDoors_ReadDoesNotHydrateAnInvisibleTarget is the read-side
// mirror of the disclosure rule above.
//
// The write side collapses `wrong_collection` to `not_found` for a target the
// caller cannot see. Hydrating that same item's ref and title on the READ would
// hand back exactly what the collapse withholds — a second door onto the fact
// the first one closes.
func TestRelationTitleDoors_ReadDoesNotHydrateAnInvisibleTarget(t *testing.T) {
	f := newDoorFixture(t)

	it := f.seed(`{"owner_ref":"` + f.target.ID + `"}`)

	blind := mustUser(t, f.srv, "blind-read@example.com", "blindread", "")
	if err := f.srv.store.AddWorkspaceMember(f.ws.ID, blind.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	// Can see the item itself, but NOT the collection its relation points into.
	if err := f.srv.store.SetMemberCollectionAccess(f.ws.ID, blind.ID, "specific",
		[]string{f.tasks.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}

	rr := f.callAs(blind, "editor", f.srv.handleGetItem, "GET",
		"/api/v1/workspaces/"+f.ws.Slug+"/items/"+it.Slug,
		map[string]string{"itemSlug": it.Slug}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var out models.Item
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	target, ok := out.RelationTargets["owner_ref"]
	if !ok {
		t.Fatal("the key was omitted entirely; id-only is the honest answer, omission says the item has no relation")
	}
	if target.Ref != "" || target.Title != "" {
		t.Errorf("hydrated an invisible target as %+v — that is the ref and title the write-side collapse exists to withhold", target)
	}

	// CONTROL, same fixture: the owner CAN see People and gets the full triple.
	// Without it, a hydrator that returned id-only for everything would pass.
	ownerRR := f.call(f.srv.handleGetItem, "GET",
		"/api/v1/workspaces/"+f.ws.Slug+"/items/"+it.Slug,
		map[string]string{"itemSlug": it.Slug}, nil)
	var ownerOut models.Item
	if err := json.Unmarshal(ownerRR.Body.Bytes(), &ownerOut); err != nil {
		t.Fatalf("decode owner: %v", err)
	}
	if ownerOut.RelationTargets["owner_ref"].Ref != f.target.Ref {
		t.Errorf("control: the owner should see the full triple, got %+v", ownerOut.RelationTargets["owner_ref"])
	}
}
