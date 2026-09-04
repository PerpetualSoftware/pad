package store

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Referent validation for relation values (PLAN-2857 U1 / TASK-2878).
//
// These are the STORE-level rules — does the value name a live item in the
// declared target collection of this workspace — and deliberately not the
// visibility rule, which is request-scoped and lives in the server layer.
//
// The negative cases are the point of the unit: `internal/items` accepts any
// string for a relation, so every one of these is a value that is stored
// today.

func u1RelationSchema(target string) models.CollectionSchema {
	return models.CollectionSchema{Fields: []models.FieldDef{
		{Key: "status", Type: "select", Options: []string{"open", "done"}, Default: "open", Required: true},
		{Key: "color", Label: "Colour", Type: "relation", Collection: target},
	}}
}

// relationFixture builds the Cars/Colors shape from IDEA-2856 and returns the
// workspace, the two collections and one live colour.
func relationFixture(t *testing.T, s *Store) (*models.Workspace, *models.Collection, *models.Collection, *models.Item) {
	t.Helper()
	ws := createTestWorkspace(t, s, "Relations")
	colors, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Colors",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open","required":true}]}`,
	})
	if err != nil {
		t.Fatalf("create colors: %v", err)
	}
	cars, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Cars",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open","required":true},{"key":"color","type":"relation","collection":"colors"}]}`,
	})
	if err != nil {
		t.Fatalf("create cars: %v", err)
	}
	red := createTestItem(t, s, ws.ID, colors.ID, "Red", "")
	return ws, colors, cars, red
}

func TestResolveRelationReferents_AcceptsIDAndRefAndCanonicalises(t *testing.T) {
	s := testStore(t)
	ws, _, _, red := relationFixture(t, s)

	// The CONTROL for every negative below: a valid UUID and a valid ref must
	// both resolve, and both must land on the SAME stored value. Without this
	// leg a resolver that refused everything would pass the whole suite.
	for _, supplied := range []string{red.ID, red.Ref} {
		fields := map[string]any{"color": supplied}
		issues, err := s.ResolveRelationReferents(ws.ID, u1RelationSchema("colors"), fields)
		if err != nil {
			t.Fatalf("supplied %q: %v", supplied, err)
		}
		if len(issues) != 0 {
			t.Fatalf("supplied %q: expected no issues, got %+v", supplied, issues)
		}
		if fields["color"] != red.ID {
			t.Fatalf("supplied %q: canonicalised to %v, want the item ID %s", supplied, fields["color"], red.ID)
		}
	}
}

func TestResolveRelationReferents_RejectsUnresolvableValues(t *testing.T) {
	s := testStore(t)
	ws, _, cars, _ := relationFixture(t, s)
	// A live item in the WRONG collection, and a live item in ANOTHER
	// workspace — both resolve through a workspace-wide or global lookup,
	// which is exactly what makes them worth their own cases.
	otherCollectionItem := createTestItem(t, s, ws.ID, cars.ID, "Delorean", "")
	otherWS := createTestWorkspace(t, s, "Elsewhere")
	foreign := createTestCollection(t, s, otherWS.ID, "Colors")
	foreignItem := createTestItem(t, s, otherWS.ID, foreign.ID, "Foreign Red", "")

	cases := []struct {
		name   string
		value  string
		reason RelationIssueReason
	}{
		// "red" is the SLUG of the live Red colour, so this leg pins the
		// deliberate divergence from ResolveItem: slugs do not resolve. It is
		// also the exact value the pre-U2 free-text editor wrote into these
		// fields, so accepting it would make the corruption indistinguishable
		// from a legitimate write.
		{"a slug, which must NOT resolve", "red", RelationTargetNotFound},
		{"a well-formed UUID naming nothing", "0f4a3c2b-1111-4222-8333-444455556666", RelationTargetNotFound},
		{"a ref naming nothing", "COLO-9999", RelationTargetNotFound},
		// The defect the design pass recorded against ResolveItem (R11): the
		// lookup is workspace-wide, so an id from a DIFFERENT collection
		// resolves and would be stored as if it belonged.
		{"a live item in another collection", otherCollectionItem.ID, RelationTargetWrongCollection},
		// GetItem is GLOBAL, so without the workspace assert this one resolves
		// too — and stores a reference across a workspace boundary that
		// PLAN-2857 excludes from v1.
		{"a live item in another workspace", foreignItem.ID, RelationTargetNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fields := map[string]any{"color": tc.value}
			issues, err := s.ResolveRelationReferents(ws.ID, u1RelationSchema("colors"), fields)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(issues) != 1 {
				t.Fatalf("expected exactly one issue, got %+v", issues)
			}
			if issues[0].Reason != tc.reason {
				t.Errorf("reason = %q, want %q", issues[0].Reason, tc.reason)
			}
			if issues[0].Key != "color" || issues[0].Value != tc.value || issues[0].Target != "colors" {
				t.Errorf("issue does not carry what a message needs: %+v", issues[0])
			}
			// The value is left EXACTLY as supplied: the caller has to quote it
			// back, and a half-canonicalised map would make a drop report lie
			// about what the source held.
			if fields["color"] != tc.value {
				t.Errorf("value was mutated to %v; unresolvable values must be left alone", fields["color"])
			}
			// And the message names all three things a user needs.
			msg := issues[0].Message()
			for _, want := range []string{"color", tc.value} {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q omits %q", msg, want)
				}
			}
		})
	}
}

func TestResolveRelationReferents_LeavesNonWritesAlone(t *testing.T) {
	s := testStore(t)
	ws, _, _, _ := relationFixture(t, s)

	// Clearing a relation is a legitimate write, an absent key is not a write
	// at all, and a required-field check belongs to ValidateFields. A resolver
	// that reported these would make every unrelated update fail.
	for _, name := range []string{"empty string", "whitespace", "absent", "explicit null"} {
		t.Run(name, func(t *testing.T) {
			fields := map[string]any{}
			switch name {
			case "empty string":
				fields["color"] = ""
			case "whitespace":
				fields["color"] = "   "
			case "explicit null":
				fields["color"] = nil
			}
			issues, err := s.ResolveRelationReferents(ws.ID, u1RelationSchema("colors"), fields)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(issues) != 0 {
				t.Fatalf("expected no issues, got %+v", issues)
			}
		})
	}
}

func TestResolveRelationReferents_SoftDeletedTargetDoesNotResolve(t *testing.T) {
	s := testStore(t)
	ws, colors, _, _ := relationFixture(t, s)
	gone := createTestItem(t, s, ws.ID, colors.ID, "Retired Blue", "")
	if err := s.DeleteItem(gone.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	// Writing a NEW reference to a deleted item is refused; an ALREADY-STORED
	// one still renders honestly on read, which is the half U2 shipped. The
	// two halves are what make "deleted" distinguishable from "never resolved"
	// in the UI, so this must not quietly become not_found's twin.
	fields := map[string]any{"color": gone.ID}
	issues, err := s.ResolveRelationReferents(ws.ID, u1RelationSchema("colors"), fields)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 1 || issues[0].Reason != RelationTargetNotFound {
		t.Fatalf("expected one not_found issue, got %+v", issues)
	}
}

func TestResolveRelationReferents_TargetCollectionProblems(t *testing.T) {
	s := testStore(t)
	ws, _, _, red := relationFixture(t, s)

	t.Run("no declared target", func(t *testing.T) {
		// A relation field with no target cannot be checked against anything.
		// Surfaced rather than treated as permission to store whatever.
		fields := map[string]any{"color": red.ID}
		issues, err := s.ResolveRelationReferents(ws.ID, u1RelationSchema(""), fields)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(issues) != 1 || issues[0].Reason != RelationTargetMissing {
			t.Fatalf("expected target_missing, got %+v", issues)
		}
	})

	t.Run("target names no collection", func(t *testing.T) {
		fields := map[string]any{"color": red.ID}
		issues, err := s.ResolveRelationReferents(ws.ID, u1RelationSchema("nonexistent"), fields)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(issues) != 1 || issues[0].Reason != RelationTargetMissing {
			t.Fatalf("expected target_missing, got %+v", issues)
		}
	})
}

func TestResolveRelationReferents_IsDeterministicAndBatched(t *testing.T) {
	s := testStore(t)
	ws, _, _, _ := relationFixture(t, s)
	// Two relation fields, both broken, both aimed at the same collection.
	schema := models.CollectionSchema{Fields: []models.FieldDef{
		{Key: "color", Type: "relation", Collection: "colors"},
		{Key: "accent", Type: "relation", Collection: "colors"},
	}}

	// The copy PREFLIGHT is one of this function's callers and is specified to
	// be safe to call repeatedly and return identical results, so issue order
	// follows the SCHEMA and not map iteration.
	var first string
	for i := 0; i < 8; i++ {
		fields := map[string]any{"accent": "nope-a", "color": "nope-b"}
		issues, err := s.ResolveRelationReferents(ws.ID, schema, fields)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(issues) != 2 {
			t.Fatalf("expected both fields reported, got %+v", issues)
		}
		encoded, _ := json.Marshal(issues)
		if i == 0 {
			first = string(encoded)
			if issues[0].Key != "color" || issues[1].Key != "accent" {
				t.Fatalf("issues are not in schema order: %+v", issues)
			}
			continue
		}
		if string(encoded) != first {
			t.Fatalf("run %d differs from run 0:\n %s\n %s", i, first, encoded)
		}
	}
}

// --- Migrate doors (PLAN-2857 U1, lead ruling: provenance, not door) ---

func TestMigrateRelationReferents_SameWorkspaceKeepsWhatResolves(t *testing.T) {
	s := testStore(t)
	ws, _, _, red := relationFixture(t, s)

	// A move within the workspace: the target is still here, so a VALID
	// relation must survive. Dropping it would lose data on every move of a
	// correctly-related item.
	fields := map[string]any{"color": red.Ref, "status": "open"}
	refusals, dropped, err := s.MigrateRelationReferents(ws.ID, u1RelationSchema("colors"), fields, nil, RelationCarryWithinWorkspace)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(refusals) != 0 || len(dropped) != 0 {
		t.Fatalf("a valid carried relation must survive: refusals=%+v dropped=%+v", refusals, dropped)
	}
	if fields["color"] != red.ID {
		t.Fatalf("survivor not canonicalised: got %v want %s", fields["color"], red.ID)
	}
}

func TestMigrateRelationReferents_SameWorkspaceDropsWhatDoesNot(t *testing.T) {
	s := testStore(t)
	ws, _, _, _ := relationFixture(t, s)

	// The legacy case, and the reason move doors do not refuse: `items` has
	// accepted any string for a relation all along, so an item carrying "red"
	// must stay MOVABLE. Dropped and reported, never refused.
	fields := map[string]any{"color": "red", "status": "open"}
	refusals, dropped, err := s.MigrateRelationReferents(ws.ID, u1RelationSchema("colors"), fields, nil, RelationCarryWithinWorkspace)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(refusals) != 0 {
		t.Fatalf("a carried value must never refuse: %+v", refusals)
	}
	if len(dropped) != 1 || dropped[0].Key != "color" || dropped[0].Value != "red" {
		t.Fatalf("expected one reported drop naming the value, got %+v", dropped)
	}
	if _, present := fields["color"]; present {
		t.Fatalf("dropped key must be removed from the map, got %v", fields["color"])
	}
	if fields["status"] != "open" {
		t.Fatalf("a drop must not disturb other fields: %+v", fields)
	}
}

func TestMigrateRelationReferents_CrossWorkspaceDropsEveryCarriedRelation(t *testing.T) {
	s := testStore(t)
	ws, _, _, red := relationFixture(t, s)

	// Even a PERFECTLY VALID source relation goes, and without a lookup: it
	// names a source-workspace row, and v1 excludes cross-workspace targets,
	// so there is nothing in the destination it could mean. Same reason
	// github_pr uses, because it is the same fact about the same kind of value.
	fields := map[string]any{"color": red.ID, "status": "open"}
	refusals, dropped, err := s.MigrateRelationReferents(ws.ID, u1RelationSchema("colors"), fields, nil, RelationCarryCrossWorkspace)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(refusals) != 0 {
		t.Fatalf("a carried value must never refuse: %+v", refusals)
	}
	if len(dropped) != 1 || dropped[0].Reason != RelationTargetNotPortable {
		t.Fatalf("expected one referent_not_portable drop, got %+v", dropped)
	}
	if _, present := fields["color"]; present {
		t.Fatalf("carried relation must be removed on a cross-workspace copy")
	}
}

func TestMigrateRelationReferents_SuppliedOverrideRefusesOnEitherMode(t *testing.T) {
	s := testStore(t)
	ws, _, _, red := relationFixture(t, s)

	// Provenance is the whole rule: the SAME bad value is a drop when carried
	// and a refusal when supplied, on both modes. A test that only drove
	// carried values would pass against a build that never refuses anything.
	for _, mode := range []RelationCarryMode{RelationCarryWithinWorkspace, RelationCarryCrossWorkspace} {
		fields := map[string]any{"color": "nope"}
		supplied := map[string]any{"color": "nope"}
		refusals, dropped, err := s.MigrateRelationReferents(ws.ID, u1RelationSchema("colors"), fields, supplied, mode)
		if err != nil {
			t.Fatalf("mode %v: %v", mode, err)
		}
		if len(refusals) != 1 || refusals[0].Key != "color" {
			t.Fatalf("mode %v: a SUPPLIED bad value must refuse, got refusals=%+v dropped=%+v", mode, refusals, dropped)
		}
	}

	// One workspace stands in for two here: in production the cross-workspace
	// copy passes the DESTINATION workspace and schema, so a supplied override
	// names a destination item. The rule under test — supplied is resolved,
	// carried is not — does not depend on which workspace that is, and the
	// door-level pins drive the real two-workspace path.
	//
	// And a supplied VALID override survives even on a cross-workspace copy,
	// where every carried relation is dropped — the caller named a
	// destination item explicitly, which is the one way a relation can be set
	// on a copy at all.
	fields := map[string]any{"color": red.ID}
	supplied := map[string]any{"color": red.ID}
	refusals, dropped, err := s.MigrateRelationReferents(ws.ID, u1RelationSchema("colors"), fields, supplied, RelationCarryCrossWorkspace)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(refusals) != 0 || len(dropped) != 0 {
		t.Fatalf("a supplied valid override must survive: refusals=%+v dropped=%+v", refusals, dropped)
	}
	if fields["color"] != red.ID {
		t.Fatalf("supplied override lost: %v", fields["color"])
	}
}
