package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// PLAN-2857 U4's PROVING TEST — the design's Q2 round-trip property, whole:
// write a `multi_relation` value as a list of REFS through the write door, read
// it back hydrated, export the workspace and re-import it, and assert the
// stored array is identical and every element still resolves. Negative leg: one
// unresolvable element refuses the WHOLE write, naming that element, never a
// partial store.
//
// Why it lives at the DOOR and not in internal/store. Every component below it
// already has direct tests — the resolver's canonicalisation and ordering
// (`relation_multi_test.go`), the hydrator's list shape, the export remap.
// Those vouch for the components and say nothing about whether the doors are
// BOUND to them: a create handler that never reaches the resolver, or an import
// that never remaps an id inside an array, passes every one of them. The design
// row asks for a round trip, and a round trip is a statement about the seams.
//
// The export/import half is a VERIFICATION, not a feature: `remapFieldIDs`
// substitutes quoted JSON tokens, which reaches ids inside arrays without
// knowing the schema. The design said verify with a round-trip rather than
// rebuild; this is that verification, and it is the only thing that would go
// red if someone replaced that substitution with a scalar-shaped walk.

type multiDoorFixture struct {
	*doorFixture
	crews   *models.Collection
	members []*models.Item
}

// newMultiDoorFixture extends the shared door fixture with a `multi_relation`
// collection and three distinct targets in the existing `people` collection.
//
// Three, deliberately: with two, a reversed array and a sorted one are the same
// observation, and "order is preserved" would be provable by a resolver that
// sorted. The supplied order below is chosen so that sorted, reversed, and
// as-supplied are three different answers.
func newMultiDoorFixture(t *testing.T) *multiDoorFixture {
	t.Helper()
	f := newDoorFixture(t)
	crews := mustSchemaCollection(t, f.srv, f.ws.ID, "Crews", `{"fields":[
		{"key":"status","label":"Status","type":"select","options":["open","done"]},
		{"key":"members","label":"Members","type":"multi_relation","collection":"`+f.people.Slug+`"}
	]}`)
	var members []*models.Item
	for _, title := range []string{"Grace", "Ada Lovelace", "Barbara"} {
		it, err := f.srv.store.CreateItem(f.ws.ID, f.people.ID, models.ItemCreate{Title: title, CreatedBy: f.owner.ID})
		if err != nil {
			t.Fatalf("CreateItem(%s): %v", title, err)
		}
		members = append(members, it)
	}
	return &multiDoorFixture{doorFixture: f, crews: crews, members: members}
}

func (f *multiDoorFixture) createCrew(title string, supplied []any) *httptest.ResponseRecorder {
	return f.call(f.srv.handleCreateItem, "POST",
		"/api/v1/workspaces/"+f.ws.Slug+"/collections/"+f.crews.Slug+"/items",
		map[string]string{"collSlug": f.crews.Slug},
		map[string]any{"title": title, "fields": map[string]any{"members": supplied}})
}

// storedMembers reads the `members` array back out of the DATABASE, for the
// reason `storedRelation` documents: a handler that echoed the right thing
// while writing the wrong thing passes a response-only check.
func storedMembers(t *testing.T, srv *Server, itemID string) []string {
	t.Helper()
	it, err := srv.store.GetItem(itemID)
	if err != nil || it == nil {
		t.Fatalf("GetItem(%s): %v", itemID, err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(it.Fields), &m); err != nil {
		t.Fatalf("parse fields %q: %v", it.Fields, err)
	}
	raw, ok := m["members"]
	if !ok {
		t.Fatalf("no members key stored; fields=%s", it.Fields)
	}
	arr, ok := raw.([]any)
	if !ok {
		t.Fatalf("members stored as %T (%v), want an array", raw, raw)
	}
	out := make([]string, len(arr))
	for i, e := range arr {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("members[%d] stored as %T, want a string", i, e)
		}
		out[i] = s
	}
	return out
}

func TestMultiRelationRoundTrip_RefListSurvivesWriteReadExportImport(t *testing.T) {
	f := newMultiDoorFixture(t)
	grace, ada, barbara := f.members[0], f.members[1], f.members[2]

	// THREE SPELLINGS, in an order that is neither sorted nor reversed-sorted by
	// any of id, ref or title — so "order preserved" is a claim only the
	// as-supplied answer satisfies.
	rr := f.createCrew("Night shift", []any{barbara.Ref, ada.Title, grace.ID})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var created models.Item
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	wantIDs := []string{barbara.ID, ada.ID, grace.ID}
	if got := storedMembers(t, f.srv, created.ID); !equalStrings(got, wantIDs) {
		t.Fatalf("stored %v, want %v — a ref list must canonicalise element-wise and keep the supplied order", got, wantIDs)
	}

	// The READ door, which is a separate binding: the hydrator is bound to it or
	// it is not, and no store test can tell.
	rr = f.call(f.srv.handleGetItem, "GET",
		"/api/v1/workspaces/"+f.ws.Slug+"/items/"+created.Slug,
		map[string]string{"itemSlug": created.Slug}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var got models.Item
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	set, ok := got.RelationTargets["members"]
	if !ok {
		t.Fatal("the read door returned no relation_targets for a multi_relation — the hydrator is not bound to it, which every store-level test still passes")
	}
	if set.List == nil {
		t.Fatalf("a multi_relation hydrated as a SCALAR (%+v); a consumer indexing the list breaks on it", set)
	}
	wantTitles := []string{barbara.Title, ada.Title, grace.Title}
	if len(set.List) != 3 {
		t.Fatalf("hydrated %d targets, want 3: %+v", len(set.List), set.List)
	}
	for i, target := range set.List {
		if target.Title != wantTitles[i] {
			t.Errorf("hydrated[%d].Title = %q, want %q — hydration must follow STORED order, not any order of its own", i, target.Title, wantTitles[i])
		}
		if target.Ref == "" || target.ID == "" {
			t.Errorf("hydrated[%d] = %+v resolves to nothing; every element of this value names a live item", i, target)
		}
	}

	// EXPORT → IMPORT. Every id changes, so an array that came back holding the
	// SOURCE ids would be dangling in the destination while looking untouched.
	bundle, err := f.srv.store.ExportWorkspace(f.ws.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}
	dst, err := f.srv.store.ImportWorkspace(bundle, "Night shift restored", f.owner.ID, "cli")
	if err != nil {
		t.Fatalf("ImportWorkspace: %v", err)
	}
	imported, err := f.srv.store.ResolveItem(dst.ID, created.Slug)
	if err != nil || imported == nil {
		t.Fatalf("resolve imported crew: item=%+v err=%v", imported, err)
	}

	importedIDs := storedMembers(t, f.srv, imported.ID)
	if len(importedIDs) != 3 {
		t.Fatalf("imported array holds %d elements, want 3: %v", len(importedIDs), importedIDs)
	}
	for i, id := range importedIDs {
		for _, sourceID := range wantIDs {
			if id == sourceID {
				t.Fatalf("imported[%d] is still the SOURCE id %s — it names a row in another workspace, which is dangling wearing the shape of a reference", i, id)
			}
		}
		row, err := f.srv.store.GetItem(id)
		if err != nil || row == nil {
			t.Fatalf("imported[%d]=%s resolves to nothing: %v", i, id, err)
		}
		if row.WorkspaceID != dst.ID {
			t.Errorf("imported[%d] resolves into workspace %s, want the DESTINATION %s", i, row.WorkspaceID, dst.ID)
		}
		if row.Title != wantTitles[i] {
			t.Errorf("imported[%d] resolves to %q, want %q — the array survived but its ORDER did not", i, row.Title, wantTitles[i])
		}
	}
}

// The negative half of the design row, and the half a partial store would pass
// if it were only asserted on the status code.
func TestMultiRelationRoundTrip_OneBadElementRefusesTheWholeWrite(t *testing.T) {
	f := newMultiDoorFixture(t)
	before := f.crewCount()

	rr := f.createCrew("Doomed", []any{f.members[0].Ref, "no-such-person", f.members[1].ID})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if body := rr.Body.String(); !strings.Contains(body, "no-such-person") {
		t.Errorf("the refusal does not NAME the offending element; a caller with a 12-element array cannot act on it: %s", body)
	}
	// NEVER A PARTIAL STORE — and this assertion is a REGRESSION GUARD, stated
	// as such rather than as evidence. Today the refusal happens before any
	// insert, so no mutant short of reordering the handler can turn it red, and
	// I did not construct one; the four mutants this file IS covered by are on
	// the other three claims. What it guards is a future create-then-validate
	// ordering, which the status code alone would not distinguish: a handler
	// that wrote the two good elements and then failed returns the same 400.
	if after := f.crewCount(); after != before {
		t.Errorf("a refused write created %d item(s); the whole write is refused or none of it is", after-before)
	}
}

// CONTROL for the leg above. A door that refused EVERY array would satisfy both
// of its assertions, and this file would read as proof of a working feature.
func TestMultiRelationRoundTrip_ControlTheSameShapeIsAcceptedWhenEveryElementResolves(t *testing.T) {
	f := newMultiDoorFixture(t)
	before := f.crewCount()

	rr := f.createCrew("Accepted", []any{f.members[0].Ref, f.members[1].ID})
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	if after := f.crewCount(); after != before+1 {
		t.Errorf("accepted write produced %d new items, want 1", after-before)
	}
}

func (f *multiDoorFixture) crewCount() int {
	f.t.Helper()
	items, err := f.srv.store.ListItems(f.ws.ID, models.ItemListParams{CollectionSlug: f.crews.Slug})
	if err != nil {
		f.t.Fatalf("ListItems(crews): %v", err)
	}
	return len(items)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
