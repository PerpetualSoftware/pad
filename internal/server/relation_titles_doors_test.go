package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// --- title resolution is scoped to what the caller can SEE ----------------
//
// Lead ruling on PLAN-2857 U6, one step past "collapse ambiguous when nothing
// is visible": among live exact-title matches in the declared collection, only
// VISIBLE ones count — zero is not_found, one RESOLVES, two or more is
// ambiguous. A hidden match never changes the answer a caller gets.
//
// The rule lives in the RESOLVER, which takes the requester's visibility
// predicate (store.RelationVisibilityFunc). It was a server-side second pass
// first, and that left the cross-workspace copy — which runs the resolver
// inside its own transaction — deciding without it.

// restrictedTitleFixture builds two same-titled colours and a member who can
// see the Doors collection plus, through an ITEM-LEVEL grant, exactly the
// colours named. Item grants are the point: they are what makes
// VisibleCollectionIDs nav-lenient, and therefore what a collection-level
// filter gets wrong.
func restrictedTitleFixture(t *testing.T, f *doorFixture, email string, grantItems ...*models.Item) *models.User {
	t.Helper()
	u := mustUser(t, f.srv, email, strings.ReplaceAll(email, "@example.com", ""), "")
	if err := f.srv.store.AddWorkspaceMember(f.ws.ID, u.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	// Explicit access to the collection under test only. People is reachable
	// ONLY through the item grants below — the nav-lenient case.
	if err := f.srv.store.SetMemberCollectionAccess(f.ws.ID, u.ID, "specific", []string{f.tasks.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}
	for _, it := range grantItems {
		if _, err := f.srv.store.CreateItemGrant(f.ws.ID, it.ID, u.ID, "read", f.owner.ID); err != nil {
			t.Fatalf("CreateItemGrant(%s): %v", it.Title, err)
		}
	}
	return u
}

// createByTitle drives the create door as `user` with a relation supplied BY
// TITLE, returning the recorder so each leg can assert on the outcome.
func (f *doorFixture) createByTitle(user *models.User, title, itemTitle string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.callAs(user, "editor", f.srv.handleCreateItem, "POST",
		"/api/v1/workspaces/"+f.ws.Slug+"/collections/"+f.tasks.Slug+"/items",
		map[string]string{"collSlug": f.tasks.Slug},
		map[string]any{"title": itemTitle, "fields": map[string]any{"owner_ref": title}})
}

func TestRelationTitleDoors_AmbiguityIsCountedOverVISIBLEMatchesOnly(t *testing.T) {
	f := newDoorFixture(t)
	// Two live people share a title. The OWNER sees both.
	twinA, err := f.srv.store.CreateItem(f.ws.ID, f.people.ID, models.ItemCreate{Title: "Robin", CreatedBy: f.owner.ID})
	if err != nil {
		t.Fatalf("CreateItem(twinA): %v", err)
	}
	twinB, err := f.srv.store.CreateItem(f.ws.ID, f.people.ID, models.ItemCreate{Title: "Robin", CreatedBy: f.owner.ID})
	if err != nil {
		t.Fatalf("CreateItem(twinB): %v", err)
	}

	t.Run("two visible matches stay ambiguous", func(t *testing.T) {
		both := restrictedTitleFixture(t, f, "sees-both@example.com", twinA, twinB)
		rr := f.createByTitle(both, "Robin", "Both")
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "more than one") && !strings.Contains(rr.Body.String(), "ambiguous") {
			t.Errorf("a caller who can see BOTH matches must be told the title is ambiguous, got: %s", rr.Body.String())
		}
	})

	t.Run("one visible match resolves, and the hidden twin changes nothing", func(t *testing.T) {
		one := restrictedTitleFixture(t, f, "sees-one@example.com", twinA)
		rr := f.createByTitle(one, "Robin", "Only one")
		if rr.Code != http.StatusCreated {
			t.Fatalf("a caller who can see exactly ONE match must have it resolve — the other is not ambiguity for them: %d %s", rr.Code, rr.Body.String())
		}
		var created models.Item
		if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode: %v", err)
		}
		stored, ok := f.storedRelation(created.ID)
		if !ok {
			t.Fatal("no owner_ref stored")
		}
		if stored != twinA.ID {
			t.Errorf("stored %v, want the VISIBLE twin %s", stored, twinA.ID)
		}
	})

	t.Run("no visible match is not_found, never ambiguous", func(t *testing.T) {
		none := restrictedTitleFixture(t, f, "sees-none@example.com")
		rr := f.createByTitle(none, "Robin", "Neither")
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
		}
		body := rr.Body.String()
		if strings.Contains(body, "more than one") || strings.Contains(body, "ambiguous") {
			t.Errorf("telling a caller the title matches SEVERAL items they cannot see is the oracle this rule closes: %s", body)
		}
		if !strings.Contains(body, "does not name an item") {
			t.Errorf("expected the not_found phrasing, got: %s", body)
		}
	})
}

// TestRelationTitleDoors_HydrationUsesITEMVisibilityNotCollectionVisibility is
// the read-side half of the same trap, and it is a leak a collection-level
// filter cannot catch.
//
// store.VisibleCollectionIDs is deliberately NAV-LENIENT: it folds in a
// collection reachable only through an ITEM grant "so the collection appears in
// navigation", leaving item-level filtering to handlers.
// requireCollectionFullyVisible's comment says so, and exists because BUG-1920
// walked into it. So a member holding a grant on ONE person gets People in that
// set — and a hydration filtered on it hands them the ref and title of EVERY
// person any item points at.
func TestRelationTitleDoors_HydrationUsesITEMVisibilityNotCollectionVisibility(t *testing.T) {
	f := newDoorFixture(t)

	granted, err := f.srv.store.CreateItem(f.ws.ID, f.people.ID, models.ItemCreate{Title: "Granted Person", CreatedBy: f.owner.ID})
	if err != nil {
		t.Fatalf("CreateItem(granted): %v", err)
	}

	pointsAtGranted := f.seed(`{"owner_ref":"` + granted.ID + `"}`)
	pointsAtSecret := f.seed(`{"owner_ref":"` + f.target.ID + `"}`)

	// The grant is on `granted` ONLY. f.target (Ada) is a sibling in the same
	// collection, with no grant.
	member := restrictedTitleFixture(t, f, "item-grant@example.com", granted, pointsAtGranted, pointsAtSecret)

	read := func(it *models.Item) models.RelationTarget {
		t.Helper()
		rr := f.callAs(member, "editor", f.srv.handleGetItem, "GET",
			"/api/v1/workspaces/"+f.ws.Slug+"/items/"+it.Slug,
			map[string]string{"itemSlug": it.Slug}, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("get %s: expected 200, got %d: %s", it.Slug, rr.Code, rr.Body.String())
		}
		var out models.Item
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out.RelationTargets["owner_ref"]
	}

	// THE LEAK: a sibling in a nav-lenient collection, with no grant of its own.
	secret := read(pointsAtSecret)
	if secret.Ref != "" || secret.Title != "" {
		t.Errorf("hydrated an UNGRANTED sibling as %+v — the collection is only reachable through an item grant, and filtering on collection visibility hands over every item in it", secret)
	}
	if secret.ID == "" {
		t.Error("the stored id was dropped; id-only is the honest answer, omission is not")
	}

	// CONTROL, same member and same request shape: the item they DO hold a
	// grant on still hydrates fully. Without this a hydrator that returned
	// id-only for everything would pass the assertion above.
	ok := read(pointsAtGranted)
	if ok.Ref != granted.Ref || ok.Title != "Granted Person" {
		t.Errorf("control: the GRANTED target should hydrate fully, got %+v", ok)
	}
}

// TestRelationTitleDoors_VisibilityNarrowingHasNoCountBoundary is the
// regression test for codex round 2's P1: the first fix bounded the candidate
// list at a constant and answered `ambiguous` past it, which made the BOUND an
// oracle — a caller could tell "one visible plus cap hidden" (resolves) from
// "one visible plus cap+1 hidden" (ambiguous), and the difference is entirely
// about items they cannot see.
//
// The fix pages through candidates and stops at the SECOND VISIBLE one, so no
// match count changes the answer. This drives well past the page size with a
// single visible match: it must still resolve.
func TestRelationTitleDoors_VisibilityNarrowingHasNoCountBoundary(t *testing.T) {
	f := newDoorFixture(t)

	// One visible match. Creation ORDER does not place it in the walk — the
	// walk goes by `id` and ids are random UUIDs — so this is not "created
	// first, therefore visited last". What forces the walk past the page
	// boundary is the COUNT: 251 matches against a page size of 100, so the
	// single visible one cannot be decided without paging wherever it lands.
	visible, err := f.srv.store.CreateItem(f.ws.ID, f.people.ID, models.ItemCreate{Title: "Crowd", CreatedBy: f.owner.ID})
	if err != nil {
		t.Fatalf("CreateItem(visible): %v", err)
	}
	// Comfortably past relationTitleCandidatePage (100), so the walk pages.
	const hidden = 250
	for i := 0; i < hidden; i++ {
		if _, err := f.srv.store.CreateItem(f.ws.ID, f.people.ID, models.ItemCreate{
			Title: "Crowd", CreatedBy: f.owner.ID,
		}); err != nil {
			t.Fatalf("CreateItem(hidden %d): %v", i, err)
		}
	}

	member := restrictedTitleFixture(t, f, "one-in-a-crowd@example.com", visible)
	rr := f.createByTitle(member, "Crowd", "Found in the crowd")
	if rr.Code != http.StatusCreated {
		t.Fatalf("a caller who can see exactly ONE of %d matches must have it resolve, whatever the total: %d %s", hidden+1, rr.Code, rr.Body.String())
	}
	var created models.Item
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	stored, ok := f.storedRelation(created.ID)
	if !ok {
		t.Fatal("no owner_ref stored")
	}
	if stored != visible.ID {
		t.Errorf("stored %v, want the one visible match %s", stored, visible.ID)
	}

	// CONTROL: the OWNER sees all 251 and is told it is ambiguous. Without
	// this, a narrowing that resolved to the first match for everybody would
	// pass the leg above.
	ownerRR := f.call(f.srv.handleCreateItem, "POST",
		"/api/v1/workspaces/"+f.ws.Slug+"/collections/"+f.tasks.Slug+"/items",
		map[string]string{"collSlug": f.tasks.Slug},
		map[string]any{"title": "Owner tries", "fields": map[string]any{"owner_ref": "Crowd"}})
	if ownerRR.Code != http.StatusBadRequest {
		t.Fatalf("control: a caller who sees every match must be refused, got %d: %s", ownerRR.Code, ownerRR.Body.String())
	}
}

// TestRelationTitleDoors_UpdateDoorAppliesTheSameNarrowing answers codex round
// 2's P2 on coverage: the three ambiguity legs only drove CREATE, and each door
// reaches the resolver by its own route.
func TestRelationTitleDoors_UpdateDoorAppliesTheSameNarrowing(t *testing.T) {
	f := newDoorFixture(t)
	twinA, err := f.srv.store.CreateItem(f.ws.ID, f.people.ID, models.ItemCreate{Title: "Sam", CreatedBy: f.owner.ID})
	if err != nil {
		t.Fatalf("CreateItem(twinA): %v", err)
	}
	if _, err := f.srv.store.CreateItem(f.ws.ID, f.people.ID, models.ItemCreate{Title: "Sam", CreatedBy: f.owner.ID}); err != nil {
		t.Fatalf("CreateItem(twinB): %v", err)
	}

	subject := f.seed(`{"status":"open"}`)
	member := restrictedTitleFixture(t, f, "update-sees-one@example.com", twinA, subject)

	rr := f.callAs(member, "editor", f.srv.handleUpdateItem, "PATCH",
		"/api/v1/workspaces/"+f.ws.Slug+"/items/"+subject.Slug,
		map[string]string{"itemSlug": subject.Slug},
		map[string]any{"fields_patch": map[string]any{"owner_ref": "Sam"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("the update door must narrow the same way create does: %d %s", rr.Code, rr.Body.String())
	}
	stored, ok := f.storedRelation(subject.ID)
	if !ok {
		t.Fatal("no owner_ref stored")
	}
	if stored != twinA.ID {
		t.Errorf("stored %v, want the VISIBLE twin %s", stored, twinA.ID)
	}
}

// TestRelationTitleDoors_LateDefaultResolvesForACallerWhoSeesOne is codex round
// 2's other P1, and it is a leak through the OUTCOME rather than the message.
//
// A relation DEFAULT declared as a title goes through the late pass, which runs
// after validation injects it. The first fix could only downgrade a narrowed
// ambiguity to not_found there — ResolveLateRelationDefaults has already
// deleted the key and the collapse had no field map to restore it into. So a
// caller who was the twin's only visible match got the default APPLIED when
// they were the sole match and DROPPED when a twin they cannot see existed. The
// reason said nothing, and the outcome said everything.
func TestRelationTitleDoors_LateDefaultResolvesForACallerWhoSeesOne(t *testing.T) {
	f := newDoorFixture(t)

	visible, err := f.srv.store.CreateItem(f.ws.ID, f.people.ID, models.ItemCreate{Title: "Morgan", CreatedBy: f.owner.ID})
	if err != nil {
		t.Fatalf("CreateItem(visible): %v", err)
	}
	if _, err := f.srv.store.CreateItem(f.ws.ID, f.people.ID, models.ItemCreate{Title: "Morgan", CreatedBy: f.owner.ID}); err != nil {
		t.Fatalf("CreateItem(hidden twin): %v", err)
	}

	// The default is a TITLE, and it is ambiguous in the store's own counting.
	defaulted := mustSchemaCollection(t, f.srv, f.ws.ID, "TitleDefaulted", fmt.Sprintf(`{"fields":[
		{"key":"status","label":"Status","type":"select","options":["open","done"]},
		{"key":"priority","label":"Priority","type":"select","options":["low","high"]},
		{"key":"owner_ref","label":"Owner","type":"relation","collection":%q,"default":"Morgan"}
	]}`, f.people.Slug))

	subject, err := f.srv.store.CreateItem(f.ws.ID, defaulted.ID, models.ItemCreate{
		Title: "Gets a defaulted owner", Fields: `{"status":"open"}`, CreatedBy: f.owner.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem(subject): %v", err)
	}

	member := restrictedTitleFixture(t, f, "late-default@example.com", visible, subject)
	if err := f.srv.store.SetMemberCollectionAccess(f.ws.ID, member.ID, "specific",
		[]string{f.tasks.ID, defaulted.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}

	// The BULK door, not fields_patch. The patch route resolves only the keys
	// the caller named, so a defaulted relation never reaches the late pass
	// there at all — I aimed this at fields_patch first and it proved nothing.
	// `set-priority` is the same route TestRelationDoors_BulkUpdateResolves-
	// InjectedRelationDefault uses, and it does run the injection.
	rr := f.callAs(member, "editor", f.srv.handleBulkItems, "POST",
		"/api/v1/workspaces/"+f.ws.Slug+"/items/bulk", nil,
		map[string]any{"op": "set-priority", "ids": []string{subject.ID}, "priority": "high"})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	stored, ok := f.storedRelationKey(subject.ID, "owner_ref")
	if !ok {
		t.Fatal("the defaulted relation was DROPPED for a caller who can see exactly one match; the drop itself tells them a twin they cannot see exists")
	}
	if stored != visible.ID {
		t.Errorf("stored %v, want the one VISIBLE match %s", stored, visible.ID)
	}
}

// TestRelationTitleDoors_ProbeFindsAVisibleMatchNotAnArbitraryOne is codex
// round 4's P2, and it is the property failing in the other direction.
//
// When nothing in the declared collection carries the title, the resolver
// probes the workspace so it can answer `wrong_collection` instead of
// not_found. That probe took ONE arbitrary row and visibility-filtered it — so
// if the row it happened to pick was hidden, the caller got not_found even
// though a DIFFERENT live match they can see would have earned the specific
// reason. A hidden match changed the answer again.
func TestRelationTitleDoors_ProbeFindsAVisibleMatchNotAnArbitraryOne(t *testing.T) {
	f := newDoorFixture(t)

	// Two collections, NEITHER of them the relation's declared target, each
	// holding an item with the same title. The caller can see one.
	collOne := mustSchemaCollection(t, f.srv, f.ws.ID, "Vault One", `{"fields":[]}`)
	collTwo := mustSchemaCollection(t, f.srv, f.ws.ID, "Vault Two", `{"fields":[]}`)
	itemOne, err := f.srv.store.CreateItem(f.ws.ID, collOne.ID, models.ItemCreate{Title: "Split Title", CreatedBy: f.owner.ID})
	if err != nil {
		t.Fatalf("CreateItem(one): %v", err)
	}
	itemTwo, err := f.srv.store.CreateItem(f.ws.ID, collTwo.ID, models.ItemCreate{Title: "Split Title", CreatedBy: f.owner.ID})
	if err != nil {
		t.Fatalf("CreateItem(two): %v", err)
	}

	// DETERMINISM. The probe walks by `id`, and ids are random UUIDs, so which
	// row it reaches first is chance — a test that hid an arbitrary one would
	// pass roughly half the time against the BROKEN code and look fine. Hide
	// the collection holding the LEXICALLY SMALLER id, so the first row the
	// probe sees is always the hidden one and the leg always exercises the
	// case it names.
	hiddenColl, openColl := collOne, collTwo
	if itemTwo.ID < itemOne.ID {
		hiddenColl, openColl = collTwo, collOne
	}

	member := mustUser(t, f.srv, "probe-split@example.com", "probesplit", "")
	if err := f.srv.store.AddWorkspaceMember(f.ws.ID, member.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	// Can see the collection under test and openColl; NOT hiddenColl, whose
	// item the probe reaches first.
	if err := f.srv.store.SetMemberCollectionAccess(f.ws.ID, member.ID, "specific",
		[]string{f.tasks.ID, openColl.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}

	rr := f.createByTitle(member, "Split Title", "Probing")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "is not an item in collection") {
		t.Errorf("the caller CAN see the match in %q, so they are owed the specific reason; the probe reaches %q first and must keep walking rather than filtering one arbitrary row: %s", openColl.Name, hiddenColl.Name, body)
	}
}
