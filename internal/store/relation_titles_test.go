package store

import (
	"encoding/json"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Exact-title resolution and read-side hydration for `relation` values
// (PLAN-2857 U6).
//
// U1 accepted a UUID or a ref. U6 adds a THIRD spelling — an exact title,
// scoped to the field's declared collection — and the read side that lets a
// client render any of them. The legs below are the design row's proving test
// at the store level; the door-level equivalents live beside the U1 door tests.
//
// Every negative leg here is paired with a positive one that would break if the
// resolver simply refused everything (CONVE-34). That is not decoration: the
// first draft of a "title in the wrong collection must refuse" test passes
// against a resolver with no title support at all.

// resolveOne runs one supplied value through the resolver and returns the
// stored result plus any issues, so a leg reads as value-in / outcome-out.
func resolveOne(t *testing.T, s *Store, ws *models.Workspace, schema models.CollectionSchema, supplied string) (string, []RelationIssue) {
	t.Helper()
	fields := map[string]any{"color": supplied}
	issues, err := s.ResolveRelationReferents(ws.ID, schema, fields)
	if err != nil {
		t.Fatalf("resolve %q: %v", supplied, err)
	}
	stored, _ := fields["color"].(string)
	return stored, issues
}

// TestRelationTitle_ThreeSpellingsOfOneItemStoreTheSameID is the design row's
// proving test: the same relation written by ref, by exact title and by raw
// UUID must land on an identical stored ID.
//
// It is also the CONTROL for every refusal leg below. A resolver that refused
// every title would pass those legs and fail only this one.
func TestRelationTitle_ThreeSpellingsOfOneItemStoreTheSameID(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, _, red := relationFixture(t, s)
	schema := u1RelationSchema("colors")

	for _, supplied := range []string{red.ID, red.Ref, red.Title} {
		stored, issues := resolveOne(t, s, ws, schema, supplied)
		if len(issues) != 0 {
			t.Fatalf("%q: unexpected issues %v", supplied, issues)
		}
		if stored != red.ID {
			t.Errorf("%q stored %q, want the canonical id %q — three spellings of one item must not store three different values", supplied, stored, red.ID)
		}
	}
}

// TestRelationTitle_ScopeIsTheDeclaredCollection is ruling (3)'s store half and
// the R11 guard: a title unique in the WORKSPACE but living outside the
// field's declared collection must refuse, not resolve.
//
// The in-collection control runs in the same test so a uniformly-refusing
// resolver cannot pass it.
func TestRelationTitle_ScopeIsTheDeclaredCollection(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, cars, _ := relationFixture(t, s)
	schema := u1RelationSchema("colors")

	// "Sedan" exists, is unique workspace-wide, and is in the WRONG collection.
	sedan := createTestItem(t, s, ws.ID, cars.ID, "Sedan", "")

	stored, issues := resolveOne(t, s, ws, schema, "Sedan")
	if len(issues) != 1 {
		t.Fatalf("a title outside the declared collection must produce exactly one issue, got %d: %v", len(issues), issues)
	}
	if issues[0].Reason != RelationTargetWrongCollection {
		t.Errorf("reason = %q, want %q — resolving it workspace-wide is the R11 defect this guards", issues[0].Reason, RelationTargetWrongCollection)
	}
	if issues[0].MatchedID != sedan.ID {
		t.Errorf("MatchedID = %q, want %q — the server's visibility collapse judges THIS item, and cannot re-derive it from a title", issues[0].MatchedID, sedan.ID)
	}
	if stored == sedan.ID {
		t.Error("the out-of-collection item was stored anyway")
	}

	// CONTROL, same run: a title that IS in the declared collection resolves.
	// Without this leg, a resolver with no title support passes the assertions
	// above by refusing everything.
	blue := createTestItem(t, s, ws.ID, mustCollectionID(t, s, ws.ID, "colors"), "Blue", "")
	stored, issues = resolveOne(t, s, ws, schema, "Blue")
	if len(issues) != 0 {
		t.Fatalf("control: an in-collection title must resolve, got issues %v", issues)
	}
	if stored != blue.ID {
		t.Errorf("control: stored %q, want %q", stored, blue.ID)
	}
}

// TestRelationTitle_AmbiguousRefusesWithItsOwnReason covers ruling (2): two
// live items in the TARGET collection sharing a title name no single one.
//
// It must NOT collapse into not_found, which would tell the caller the opposite
// of what happened — the title matched too much, not too little.
func TestRelationTitle_AmbiguousRefusesWithItsOwnReason(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, colors, _, _ := relationFixture(t, s)
	schema := u1RelationSchema("colors")

	createTestItem(t, s, ws.ID, colors.ID, "Teal", "")
	createTestItem(t, s, ws.ID, colors.ID, "Teal", "")

	stored, issues := resolveOne(t, s, ws, schema, "Teal")
	if len(issues) != 1 {
		t.Fatalf("want exactly one issue, got %d: %v", len(issues), issues)
	}
	if issues[0].Reason != RelationTargetAmbiguous {
		t.Errorf("reason = %q, want %q", issues[0].Reason, RelationTargetAmbiguous)
	}
	if stored == "" {
		t.Error("the supplied value was blanked; an unresolvable supplied value is refused, not silently emptied")
	}

	// The vocabulary must be enumerable, since the copy preflight puts these on
	// the wire and a cross-stack gate checks the client against this list.
	found := false
	for _, r := range RelationIssueReasons() {
		if r == RelationTargetAmbiguous {
			found = true
		}
	}
	if !found {
		t.Error("RelationTargetAmbiguous is missing from RelationIssueReasons(), so no consumer can enumerate it")
	}
}

// TestRelationTitle_RefWinsOverAnItemTitledLikeARef pins the ladder's order and
// the shadowing case the resolver's doc comment names: UUID, then ref, then
// title. An item literally titled "COLO-3" is unreachable BY TITLE while a ref
// COLO-3 resolves.
//
// This is deliberate — refs are the canonical spelling and the ambiguity is
// created by the title — but it is a real edge, so it is pinned rather than
// left to be rediscovered as a bug.
func TestRelationTitle_RefWinsOverAnItemTitledLikeARef(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, colors, _, red := relationFixture(t, s)
	schema := u1RelationSchema("colors")

	// A second colour whose TITLE is the first colour's REF.
	impostor := createTestItem(t, s, ws.ID, colors.ID, red.Ref, "")

	stored, issues := resolveOne(t, s, ws, schema, red.Ref)
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %v", issues)
	}
	if stored != red.ID {
		t.Errorf("stored %q, want the REF's target %q — the ref rung must win over an item merely titled like one", stored, red.ID)
	}
	if stored == impostor.ID {
		t.Error("the title rung shadowed the ref rung")
	}
}

// TestRelationTitle_SoftDeletedTitleDoesNotResolve: a deleted item's title is
// not a handle. The control is the same title on a live item, so the leg
// cannot pass by refusing every title.
func TestRelationTitle_SoftDeletedTitleDoesNotResolve(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, colors, _, _ := relationFixture(t, s)
	schema := u1RelationSchema("colors")

	gone := createTestItem(t, s, ws.ID, colors.ID, "Ochre", "")
	if err := s.DeleteItem(gone.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	_, issues := resolveOne(t, s, ws, schema, "Ochre")
	if len(issues) != 1 || issues[0].Reason != RelationTargetNotFound {
		t.Fatalf("a soft-deleted title must be not_found, got %v", issues)
	}

	// CONTROL: the same title, live again, resolves.
	live := createTestItem(t, s, ws.ID, colors.ID, "Ochre", "")
	stored, issues := resolveOne(t, s, ws, schema, "Ochre")
	if len(issues) != 0 {
		t.Fatalf("control: a live title must resolve, got %v", issues)
	}
	if stored != live.ID {
		t.Errorf("control: stored %q, want %q", stored, live.ID)
	}
}

// TestHydrateRelationTargets_ResolvesDanglesAndRefusesForeignWorkspaces covers
// the read side: what a caller gets back for each kind of stored value.
//
// The foreign-workspace leg is the one worth the fixture cost. A stored
// relation value is just an id in a blob, and nothing stops a legacy row from
// naming an item in ANOTHER workspace; hydrating it would put that item's ref
// and title in a response to someone who cannot see its workspace at all.
func TestHydrateRelationTargets_ResolvesDanglesAndRefusesForeignWorkspaces(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, colors, cars, red := relationFixture(t, s)

	// A live colour in a DIFFERENT workspace, reachable only by its raw id.
	otherWS, otherColors, _, _ := relationFixture(t, s)
	foreign := createTestItem(t, s, otherWS.ID, otherColors.ID, "Foreign Red", "")
	_ = colors

	mk := func(value string) models.Item {
		it := createTestItem(t, s, ws.ID, cars.ID, "Car "+value[:6], "")
		blob, _ := json.Marshal(map[string]any{"status": "open", "color": value})
		if _, err := s.UpdateItem(it.ID, models.ItemUpdate{Fields: strPtr(string(blob))}); err != nil {
			t.Fatalf("seed fields: %v", err)
		}
		got, err := s.GetItem(it.ID)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		return *got
	}

	resolvable := mk(red.ID)
	dangling := mk("00000000-0000-4000-8000-00000000dead")
	crossWS := mk(foreign.ID)

	schemas := map[string]models.CollectionSchema{cars.ID: u1RelationSchema("colors")}
	out, collectionOf, err := s.HydrateRelationTargetsQ(s.DB(), ws.ID, []models.Item{resolvable, dangling, crossWS}, schemas)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}

	// Resolvable: the full triple.
	got := out[resolvable.ID]["color"]
	if got.ID != red.ID || got.Ref != red.Ref || got.Title != red.Title {
		t.Errorf("resolvable hydrated as %+v, want {id:%s ref:%s title:%s}", got, red.ID, red.Ref, red.Title)
	}
	if collectionOf[red.ID] == "" {
		t.Error("no collection id returned for a resolved target; the server cannot run its visibility collapse without one")
	}

	// Dangling: ID-ONLY, and PRESENT. Omitting the key would say "this item has
	// no relation", which is a different and false statement.
	got = out[dangling.ID]["color"]
	if _, present := out[dangling.ID]["color"]; !present {
		t.Fatal("a dangling value was omitted rather than hydrated id-only")
	}
	if got.Ref != "" || got.Title != "" {
		t.Errorf("dangling hydrated as %+v, want id-only", got)
	}

	// Cross-workspace: also ID-ONLY. This is the disclosure leg — a ref or a
	// title here is another workspace's data.
	got = out[crossWS.ID]["color"]
	if got.Ref != "" || got.Title != "" {
		t.Errorf("a foreign-workspace id hydrated as %+v — that is %s's ref/title in this workspace's response", got, otherWS.Slug)
	}
	if got.ID != foreign.ID {
		t.Errorf("cross-workspace entry lost its stored id: %+v", got)
	}
}

// mustCollectionID looks a collection up by slug, failing the test rather than
// returning an empty string that would make a later assertion fail for the
// wrong reason.
func mustCollectionID(t *testing.T, s *Store, workspaceID, slug string) string {
	t.Helper()
	colls, err := s.ListCollections(workspaceID)
	if err != nil {
		t.Fatalf("list collections: %v", err)
	}
	for i := range colls {
		if colls[i].Slug == slug {
			return colls[i].ID
		}
	}
	t.Fatalf("no collection %q in workspace %s (have %d)", slug, workspaceID, len(colls))
	return ""
}
