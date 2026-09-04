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
