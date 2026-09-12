package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// `multi_relation` resolution (PLAN-2857 U4, population-table rows 2/14/15).
//
// Every element runs the SAME ladder a scalar relation runs, through the same
// extracted function, so these legs are about what the ARRAY adds: order,
// length, mixed spellings, per-element issues, and duplicates-after-resolution.
//
// Each refusal leg below is paired with a positive control in the same test or
// the one above it, because a resolver that refused every array would pass a
// whole file of refusal legs (CONVE-34).

func multiRelationSchemaFor(target string) models.CollectionSchema {
	return models.CollectionSchema{Fields: []models.FieldDef{
		{Key: "status", Type: "select", Options: []string{"open", "done"}, Default: "open", Required: true},
		{Key: "color", Label: "Colour", Type: "multi_relation", Collection: target},
	}}
}

func resolveMulti(t *testing.T, s *Store, ws *models.Workspace, schema models.CollectionSchema, supplied any) ([]any, []RelationIssue) {
	t.Helper()
	fields := map[string]any{"color": supplied}
	issues, err := s.ResolveRelationReferents(ws.ID, schema, fields, nil)
	if err != nil {
		t.Fatalf("resolve %#v: %v", supplied, err)
	}
	switch v := fields["color"].(type) {
	case []any:
		return v, issues
	case []string:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = e
		}
		return out, issues
	default:
		return nil, issues
	}
}

// THE CONTROL, and the proving leg: mixed spellings of two items canonicalise
// to their ids, IN THE ORDER SUPPLIED. A resolver that refused every array, or
// one that returned a set, fails here and nowhere else.
func TestMultiRelation_CanonicalisesEveryElementAndKeepsOrder(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, colors, _, red := relationFixture(t, s)
	blue := createTestItem(t, s, ws.ID, colors.ID, "Blue", "")
	schema := multiRelationSchemaFor("colors")

	// Three spellings across two items: a raw UUID, a ref, and an exact title.
	got, issues := resolveMulti(t, s, ws, schema, []any{blue.Ref, red.ID})
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %+v", issues)
	}
	if len(got) != 2 {
		t.Fatalf("stored %d elements, want 2: %#v", len(got), got)
	}
	if got[0] != blue.ID || got[1] != red.ID {
		t.Errorf("stored %#v, want [%s %s] — order is part of the value, and a ref must canonicalise to its id", got, blue.ID, red.ID)
	}

	// The title spelling, in the other order, must reach the same two ids.
	got, issues = resolveMulti(t, s, ws, schema, []any{red.Title, blue.Title})
	if len(issues) != 0 {
		t.Fatalf("title spelling: unexpected issues: %+v", issues)
	}
	if len(got) != 2 || got[0] != red.ID || got[1] != blue.ID {
		t.Errorf("title spelling stored %#v, want [%s %s]", got, red.ID, blue.ID)
	}
}

// An element that does not resolve produces ONE issue for that element, and the
// stored blob is left EXACTLY as it arrived — not partially canonicalised. The
// doors turn any issue into a refusal, so a write the caller did not ask for
// must not have happened on the way to refusing them.
func TestMultiRelation_OneBadElementRefusesAndWritesNothing(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, _, red := relationFixture(t, s)
	schema := multiRelationSchemaFor("colors")

	supplied := []any{red.Ref, "no-such-colour"}
	fields := map[string]any{"color": supplied}
	issues, err := s.ResolveRelationReferents(ws.ID, schema, fields, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("want exactly 1 issue, got %d: %+v", len(issues), issues)
	}
	if issues[0].Reason != RelationTargetNotFound {
		t.Errorf("reason = %q, want %q", issues[0].Reason, RelationTargetNotFound)
	}
	if issues[0].Value != "no-such-colour" {
		t.Errorf("issue value = %q, want the offending ELEMENT — a caller with twelve references needs to know which one", issues[0].Value)
	}
	stored, _ := fields["color"].([]any)
	if len(stored) != 2 || stored[0] != red.Ref || stored[1] != "no-such-colour" {
		t.Errorf("blob was rewritten to %#v; it must arrive back untouched when the write is about to be refused (supplied %#v)", fields["color"], supplied)
	}
}

// Duplicates are detected AFTER resolution — which is the whole reason they
// cannot be a shape check. These two elements are DIFFERENT STRINGS naming one
// item, so a pre-resolution comparison passes them.
func TestMultiRelation_DuplicateAfterResolutionIsRefused(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, _, red := relationFixture(t, s)
	schema := multiRelationSchemaFor("colors")

	for _, supplied := range [][]any{
		{red.ID, red.Ref},    // uuid then ref
		{red.Ref, red.Title}, // ref then title
		{red.ID, red.ID},     // the obvious case
	} {
		fields := map[string]any{"color": supplied}
		issues, err := s.ResolveRelationReferents(ws.ID, schema, fields, nil)
		if err != nil {
			t.Fatalf("%#v: %v", supplied, err)
		}
		if len(issues) != 1 {
			t.Fatalf("%#v: want 1 duplicate issue, got %d: %+v", supplied, len(issues), issues)
		}
		if issues[0].Reason != RelationTargetDuplicate {
			t.Errorf("%#v: reason = %q, want %q", supplied, issues[0].Reason, RelationTargetDuplicate)
		}
		// The SECOND occurrence is named, because that is the one to remove.
		if issues[0].Value != supplied[1] {
			t.Errorf("%#v: issue value = %q, want the second occurrence %q", supplied, issues[0].Value, supplied[1])
		}
	}

	// CONTROL: two DIFFERENT items are not duplicates. Without this leg a
	// resolver that called every array a duplicate would pass the loop above.
	blue := createTestItem(t, s, ws.ID, relationColorsID(t, s, ws), "Blue", "")
	got, issues := resolveMulti(t, s, ws, schema, []any{red.ID, blue.ID})
	if len(issues) != 0 {
		t.Fatalf("two distinct items reported issues: %+v", issues)
	}
	if len(got) != 2 {
		t.Errorf("stored %#v, want both ids", got)
	}
}

// An empty array resolves to an empty array with no issues. `[]` means "no
// targets"; normalising it to an absent key is the write door's job, and
// `required` is enforced against resolved elements elsewhere — so the resolver
// must not invent an issue for it.
func TestMultiRelation_EmptyArrayIsNotAnIssue(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, _, _ := relationFixture(t, s)
	schema := multiRelationSchemaFor("colors")

	got, issues := resolveMulti(t, s, ws, schema, []any{})
	if len(issues) != 0 {
		t.Fatalf("empty array produced issues: %+v", issues)
	}
	if len(got) != 0 {
		t.Errorf("stored %#v, want an empty array", got)
	}
}

// A field declaring no target collection reports ONE field-level issue, not one
// per element: every element fails for the identical reason and N copies bury
// the single thing the caller has to fix.
func TestMultiRelation_NoTargetCollectionReportsOnceForTheField(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, _, _, red := relationFixture(t, s)
	schema := models.CollectionSchema{Fields: []models.FieldDef{
		{Key: "status", Type: "select", Options: []string{"open", "done"}, Default: "open", Required: true},
		{Key: "color", Label: "Colour", Type: "multi_relation"}, // no Collection
	}}

	fields := map[string]any{"color": []any{red.ID, red.Ref, "anything"}}
	issues, err := s.ResolveRelationReferents(ws.ID, schema, fields, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("want 1 field-level issue for a schema with no target, got %d: %+v", len(issues), issues)
	}
	if issues[0].Reason != RelationTargetMissing {
		t.Errorf("reason = %q, want %q", issues[0].Reason, RelationTargetMissing)
	}
}

// relationColorsID looks the Colors collection up by slug so a leg can create a
// second colour without threading the fixture's return values around.
func relationColorsID(t *testing.T, s *Store, ws *models.Workspace) string {
	t.Helper()
	c, err := s.GetCollectionBySlug(ws.ID, "colors")
	if err != nil {
		t.Fatalf("get colors: %v", err)
	}
	return c.ID
}
