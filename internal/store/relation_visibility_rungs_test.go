package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Visibility on the UUID and REF rungs (PLAN-2857 U4, lead ruling day 64 after
// codex round 1's P1).
//
// It used to live only in the SERVER's post-resolution pass, which is gated on
// the field TYPE — so every element of a `multi_relation` went unjudged and an
// editor with no access to the target collection could plant a hidden item's
// ref inside an array and get its canonical UUID back. Gating a security check
// on a type is the defect; the fix is the one function BOTH types resolve
// through, which is why that function was extracted.
//
// These legs drive the store resolver directly with a visibility predicate,
// because that is now where the answer is decided. The door-level binding has
// its own leg in internal/server.

// hides returns a RelationVisibilityFunc that can see everything except the
// named item ids.
func hides(ids ...string) RelationVisibilityFunc {
	hidden := map[string]bool{}
	for _, id := range ids {
		hidden[id] = true
	}
	return func(_ Queryer, _ string, item *models.Item) (bool, error) {
		if item == nil {
			return false, nil
		}
		return !hidden[item.ID], nil
	}
}

func TestRelationVisibility_ScalarRefToAnInvisibleTargetIsNotFound(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, _, red := relationFixture(t, s)
	schema := models.CollectionSchema{Fields: []models.FieldDef{
		{Key: "color", Type: "relation", Collection: "colors"},
	}}

	fields := map[string]any{"color": red.Ref}
	issues, err := s.ResolveRelationReferents(ws.ID, schema, fields, hides(red.ID))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("issues = %+v, want exactly one — an invisible target must not resolve", issues)
	}
	if issues[0].Reason != RelationTargetNotFound {
		t.Errorf("reason = %q, want %q: any other reason tells the caller the value names something real", issues[0].Reason, RelationTargetNotFound)
	}
	if got := fields["color"]; got != red.Ref {
		t.Errorf("the value was rewritten to %v; a refused write must not hand back the canonical id of an item the caller may not see", got)
	}
}

func TestRelationVisibility_EveryMultiElementIsJudged(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, colors, _, red := relationFixture(t, s)
	blue := createTestItem(t, s, ws.ID, colors.ID, "Blue", "")
	schema := multiRelationSchemaFor("colors")

	// The SECOND element is the hidden one, deliberately: a loop that checked
	// only the first — or only the scalar shape — passes a first-element
	// fixture.
	fields := map[string]any{"color": []any{blue.Ref, red.Ref}}
	issues, err := s.ResolveRelationReferents(ws.ID, schema, fields, hides(red.ID))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("issues = %+v, want exactly one for the hidden element", issues)
	}
	if issues[0].Reason != RelationTargetNotFound {
		t.Errorf("reason = %q, want %q", issues[0].Reason, RelationTargetNotFound)
	}
	if issues[0].Value != red.Ref {
		t.Errorf("issue names %q, want the caller's own spelling %q — never the canonical id of an unseen item", issues[0].Value, red.Ref)
	}
	// AND NOTHING WAS WRITTEN BACK. The refusal alone does not say this: an
	// array canonicalised in place would have leaked the visible element's id
	// into a blob the door is about to refuse.
	arr, ok := fields["color"].([]any)
	if !ok || len(arr) != 2 || arr[0] != blue.Ref || arr[1] != red.Ref {
		t.Errorf("value = %#v, want the supplied array untouched", fields["color"])
	}
}

func TestRelationVisibility_ControlAVisibleElementStillResolves(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, colors, _, red := relationFixture(t, s)
	blue := createTestItem(t, s, ws.ID, colors.ID, "Blue", "")
	schema := multiRelationSchemaFor("colors")

	// Without this, a predicate that hid EVERYTHING — or a resolver that
	// refused every array — would satisfy both legs above, and this file would
	// read as proof of a working check.
	fields := map[string]any{"color": []any{blue.Ref, red.Ref}}
	issues, err := s.ResolveRelationReferents(ws.ID, schema, fields, hides("no-such-id"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(issues) != 0 {
		t.Fatalf("issues = %+v, want none when every element is visible", issues)
	}
	arr, _ := fields["color"].([]any)
	if len(arr) != 2 || arr[0] != blue.ID || arr[1] != red.ID {
		t.Errorf("value = %#v, want both canonicalised in order", fields["color"])
	}
}

func TestRelationVisibility_WrongCollectionIsCollapsedForAnUnseenTarget(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, cars, _ := relationFixture(t, s)
	// A live item in the WRONG collection. `wrong_collection` is the one reason
	// whose message says the value names something real, so for a caller who
	// cannot see that item it is an existence oracle.
	car := createTestItem(t, s, ws.ID, cars.ID, "Saab", "")
	schema := multiRelationSchemaFor("colors")

	fields := map[string]any{"color": []any{car.Ref}}
	issues, err := s.ResolveRelationReferents(ws.ID, schema, fields, hides(car.ID))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("issues = %+v, want one", issues)
	}
	if issues[0].Reason != RelationTargetNotFound {
		t.Errorf("reason = %q, want %q — order matters here: the visibility check has to run BEFORE the collection check, or the collection answer discloses existence first", issues[0].Reason, RelationTargetNotFound)
	}
}

func TestRelationVisibility_ControlWrongCollectionSurvivesForASeenTarget(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, cars, _ := relationFixture(t, s)
	car := createTestItem(t, s, ws.ID, cars.ID, "Saab", "")
	schema := multiRelationSchemaFor("colors")

	// The useful half of that reason: "you linked a car where a colour belongs"
	// is worth saying to someone entitled to hear it. A collapse that fired
	// unconditionally would pass the leg above and destroy this.
	fields := map[string]any{"color": []any{car.Ref}}
	issues, err := s.ResolveRelationReferents(ws.ID, schema, fields, hides("no-such-id"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(issues) != 1 || issues[0].Reason != RelationTargetWrongCollection {
		t.Fatalf("issues = %+v, want one wrong_collection", issues)
	}
	if !issues[0].VisibilityChecked {
		t.Error("VisibilityChecked is false, so the server's collapse will re-resolve and re-judge a value this resolver already judged")
	}
}

func TestRelationVisibility_NoPredicateMeansNoJudgement(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, _, red := relationFixture(t, s)
	schema := multiRelationSchemaFor("colors")

	// A nil predicate is how every internal caller with no requester asks —
	// migrations, imports, tests. It must mean "not asked", never "deny".
	fields := map[string]any{"color": []any{red.Ref}}
	issues, err := s.ResolveRelationReferents(ws.ID, schema, fields, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(issues) != 0 {
		t.Fatalf("issues = %+v, want none: a nil predicate must not refuse", issues)
	}
	arr, _ := fields["color"].([]any)
	if len(arr) != 1 || arr[0] != red.ID {
		t.Errorf("value = %#v, want the canonical id", fields["color"])
	}
}
