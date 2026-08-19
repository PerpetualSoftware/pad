package items

import (
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestMigrateFields_MatchingTypes(t *testing.T) {
	source := []models.FieldDef{
		{Key: "status", Type: "select", Options: []string{"open", "done"}},
		{Key: "priority", Type: "select", Options: []string{"low", "high"}},
	}
	target := []models.FieldDef{
		{Key: "status", Type: "select", Options: []string{"open", "closed"}, Required: true},
		{Key: "priority", Type: "select", Options: []string{"low", "medium", "high"}},
	}
	fields := map[string]any{"status": "open", "priority": "high"}

	result := MigrateFields(fields, source, target, SameWorkspace)

	if result.Fields["status"] != "open" {
		t.Errorf("status: got %v, want 'open'", result.Fields["status"])
	}
	if result.Fields["priority"] != "high" {
		t.Errorf("priority: got %v, want 'high'", result.Fields["priority"])
	}
	if len(result.Dropped) != 0 {
		t.Errorf("dropped: got %v, want none", result.Dropped)
	}
}

func TestMigrateFields_SelectValueNotInTarget(t *testing.T) {
	source := []models.FieldDef{
		{Key: "status", Type: "select", Options: []string{"open", "in-progress", "done"}},
	}
	target := []models.FieldDef{
		{Key: "status", Type: "select", Options: []string{"todo", "doing", "done"}, Required: true, Default: "todo"},
	}
	fields := map[string]any{"status": "in-progress"}

	result := MigrateFields(fields, source, target, SameWorkspace)

	// "in-progress" is not in target options, should be dropped and default applied
	if result.Fields["status"] != "todo" {
		t.Errorf("status: got %v, want 'todo' (default after drop)", result.Fields["status"])
	}
}

func TestMigrateFields_DropsExtraFields(t *testing.T) {
	source := []models.FieldDef{
		{Key: "severity", Type: "select", Options: []string{"low", "high"}},
		{Key: "browser", Type: "text"},
	}
	target := []models.FieldDef{
		{Key: "priority", Type: "select", Options: []string{"low", "high"}},
	}
	fields := map[string]any{"severity": "high", "browser": "Chrome"}

	result := MigrateFields(fields, source, target, SameWorkspace)

	if len(result.Dropped) != 2 {
		t.Errorf("dropped: got %d, want 2", len(result.Dropped))
	}
}

func TestMigrateFields_TypeConversion(t *testing.T) {
	source := []models.FieldDef{
		{Key: "count", Type: "number"},
		{Key: "status", Type: "select", Options: []string{"open"}},
	}
	target := []models.FieldDef{
		{Key: "count", Type: "text"},
		{Key: "status", Type: "text"},
	}
	fields := map[string]any{"count": 42, "status": "open"}

	result := MigrateFields(fields, source, target, SameWorkspace)

	if result.Fields["count"] != "42" {
		t.Errorf("count: got %v, want '42'", result.Fields["count"])
	}
	if result.Fields["status"] != "open" {
		t.Errorf("status: got %v, want 'open'", result.Fields["status"])
	}
}

func TestMigrateFields_RequiredFieldMissing(t *testing.T) {
	source := []models.FieldDef{}
	target := []models.FieldDef{
		{Key: "status", Type: "select", Options: []string{"open"}, Required: true},
	}
	fields := map[string]any{}

	result := MigrateFields(fields, source, target, SameWorkspace)

	if len(result.Errors) != 1 {
		t.Errorf("errors: got %d, want 1", len(result.Errors))
	}
}

func TestMigrateFields_DefaultApplied(t *testing.T) {
	source := []models.FieldDef{}
	target := []models.FieldDef{
		{Key: "status", Type: "select", Options: []string{"open", "done"}, Required: true, Default: "open"},
	}
	fields := map[string]any{}

	result := MigrateFields(fields, source, target, SameWorkspace)

	if result.Fields["status"] != "open" {
		t.Errorf("status: got %v, want 'open'", result.Fields["status"])
	}
	if len(result.Errors) != 0 {
		t.Errorf("errors: got %v, want none", result.Errors)
	}
}

// BUG-2674. MigrateFields dropped every key absent from the TARGET schema, and
// implementation_notes / decision_log / github_pr / convention are reserved
// metadata that no schema declares — so a routine `pad item move` destroyed
// well-formed notes, decision logs and linked-PR metadata outright, silently,
// and reported success. Reproduced live before the fix: a note written through
// `pad item note` was gone after moving the item to another collection.
//
// The assertion is deliberately on the VALUE surviving intact, not merely on
// the key being present: a carry-through that re-encoded or zeroed the payload
// would satisfy a presence check while still losing the notes (CONVE-12).
func TestMigrateFields_ReservedKeysCarryThroughUntouched(t *testing.T) {
	source := []models.FieldDef{{Key: "severity", Type: "select", Options: []string{"low", "high"}}}
	target := []models.FieldDef{{Key: "priority", Type: "select", Options: []string{"low", "high"}}}

	notes := []any{map[string]any{"id": "note-1", "summary": "carried"}}
	decisions := []any{map[string]any{"id": "decision-1", "decision": "carried"}}
	pr := map[string]any{"number": float64(42), "url": "https://example.invalid/42"}
	convention := map[string]any{"trigger": "always", "enforcement": "must"}

	// The expectations are INDEPENDENT deep copies, not aliases of the values
	// handed to MigrateFields (Codex round 1). Comparing the result against the
	// same backing objects that were passed in cannot detect an in-place
	// mutation: both sides change together and DeepEqual stays true. These
	// copies are the only thing that makes "untouched" mean untouched.
	wantNotes := []any{map[string]any{"id": "note-1", "summary": "carried"}}
	wantDecisions := []any{map[string]any{"id": "decision-1", "decision": "carried"}}
	wantPR := map[string]any{"number": float64(42), "url": "https://example.invalid/42"}
	wantConvention := map[string]any{"trigger": "always", "enforcement": "must"}

	fields := map[string]any{
		"implementation_notes": notes,
		"decision_log":         decisions,
		"github_pr":            pr,
		"convention":           convention,
		"severity":             "high", // an ordinary key with no target home
	}

	result := MigrateFields(fields, source, target, SameWorkspace)

	for key, want := range map[string]any{
		"implementation_notes": wantNotes,
		"decision_log":         wantDecisions,
		"github_pr":            wantPR,
		"convention":           wantConvention,
	} {
		got, ok := result.Fields[key]
		if !ok {
			t.Errorf("%s must carry through a move, got dropped", key)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s must carry through UNTOUCHED:\n got %#v\nwant %#v", key, got, want)
		}
		for _, d := range result.Dropped {
			if d == key {
				t.Errorf("%s must not be reported dropped", key)
			}
		}
	}

	// Control: the ordinary key with no home in the target still drops, and
	// still reports. Without this leg, "carry everything" passes.
	if _, ok := result.Fields["severity"]; ok {
		t.Error("severity has no target field and must still drop")
	}
	if len(result.Dropped) != 1 || result.Dropped[0] != "severity" {
		t.Errorf("dropped: got %#v, want exactly [severity]", result.Dropped)
	}
}

// Reserved keys are carried by IDENTITY, not by name-matching against the
// target schema — so they survive even when the target happens to declare a
// field of the same name, and they are never run through migrateValue.
func TestMigrateFields_ReservedKeysBypassSchemaMatching(t *testing.T) {
	source := []models.FieldDef{}
	// A pathological target that declares implementation_notes as a plain
	// text field. Type-migrating a []any into "text" is exactly the kind of
	// coercion that would corrupt or drop the payload.
	target := []models.FieldDef{{Key: "implementation_notes", Type: "text"}}

	notes := []any{map[string]any{"id": "note-1", "summary": "carried"}}
	want := []any{map[string]any{"id": "note-1", "summary": "carried"}} // independent copy — see above
	result := MigrateFields(map[string]any{"implementation_notes": notes}, source, target, SameWorkspace)

	got, ok := result.Fields["implementation_notes"]
	if !ok {
		t.Fatal("implementation_notes must carry through even when the target declares the key")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("payload must not be coerced by the target FieldDef:\n got %#v\nwant %#v", got, want)
	}
}

// The schema collision the test above simulates must not be REACHABLE: a
// collection schema may not declare a reserved key in the first place. Without
// this, MigrateFields hands the array through untouched and
// ValidateFieldsDetailed — which iterates schema.Fields and therefore DOES see
// a declared key — rejects it, turning a move that used to silently destroy the
// notes into one that fails outright (Codex round 1 P2). Pinning the set here
// rather than only at the server handler keeps the two lists honest.
func TestReservedItemFieldKeysAreStableAndComplete(t *testing.T) {
	got := models.ReservedItemFieldKeys()
	want := []string{"convention", "decision_log", "github_pr", "implementation_notes"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reserved key set changed:\n got %#v\nwant %#v\n\nIf this is intentional, the new key must ALSO be added to the web's RESERVED_FIELD_KEYS (field-editor-types.ts) so the UI steers authors away before the server 400s.", got, want)
	}
	for _, k := range want {
		if !models.IsReservedItemField(k) {
			t.Errorf("IsReservedItemField(%q) = false, want true", k)
		}
	}
	// Control: a key that merely LOOKS internal is not reserved. Without
	// this, a helper returning true for everything passes.
	for _, k := range []string{"status", "priority", "notes", "github", ""} {
		if models.IsReservedItemField(k) {
			t.Errorf("IsReservedItemField(%q) = true, want false", k)
		}
	}
}

// The carry rule's own qualifier (BUG-2674, lead ruling): system-minted
// NON-REFERENTIAL data carries everywhere; REFERENTIAL system data carries only
// where its referent's context still holds. github_pr names a repository that
// belongs to the source workspace's context — carried into a different
// workspace it renders as a live PR link on an item whose project may have no
// relationship to that repo, which is a false statement rather than a preserved
// one. Notes and decisions describe the item's own history and are true wherever
// the item is.
//
// Both scopes are asserted in one test on purpose. The interesting property is
// the DIFFERENCE: a implementation that ignored scope entirely, in either
// direction, passes whichever half you write alone.
func TestMigrateFields_ReferentialKeysTravelOnlyWithinTheirContext(t *testing.T) {
	source := []models.FieldDef{}
	target := []models.FieldDef{}

	pr := map[string]any{"number": float64(42), "url": "https://example.invalid/42"}
	notes := []any{map[string]any{"id": "note-1", "summary": "history is true anywhere"}}
	build := func() map[string]any {
		return map[string]any{"github_pr": pr, "implementation_notes": notes}
	}

	t.Run("same workspace carries it", func(t *testing.T) {
		result := MigrateFields(build(), source, target, SameWorkspace)
		if _, ok := result.Fields["github_pr"]; !ok {
			t.Error("github_pr must carry within one workspace — the repo context is unchanged")
		}
		for _, d := range result.Dropped {
			if d == "github_pr" {
				t.Error("github_pr must not be reported dropped within one workspace")
			}
		}
	})

	t.Run("cross workspace drops it AND reports it", func(t *testing.T) {
		result := MigrateFields(build(), source, target, CrossWorkspace)
		if _, ok := result.Fields["github_pr"]; ok {
			t.Error("github_pr must not carry into another workspace — its referent's context is gone")
		}
		// Reported, not silently discarded. PLAN-2357 DR-17: "None of this
		// may be silent." A drop with no report is the defect this whole
		// unit exists to remove, and it would be perverse to reintroduce it
		// in the fix's own new branch.
		var reported bool
		for _, d := range result.Dropped {
			if d == "github_pr" {
				reported = true
			}
		}
		if !reported {
			t.Errorf("github_pr dropped without a report; Dropped = %#v", result.Dropped)
		}
	})

	t.Run("non-referential metadata is unaffected by scope", func(t *testing.T) {
		for _, scope := range []MigrateScope{SameWorkspace, CrossWorkspace} {
			result := MigrateFields(build(), source, target, scope)
			if _, ok := result.Fields["implementation_notes"]; !ok {
				t.Errorf("scope %v: implementation_notes must carry — it describes the item, not its surroundings", scope)
			}
		}
	})
}

// StillDropped exists because MigrateResult.Dropped is computed BEFORE
// overrides merge and before defaults are injected, so a key it lists may have
// been supplied moments later. Reporting those states something false about the
// item that is about to be written.
//
// The nil leg is the one that matters and the one a naive implementation gets
// wrong: the MOVE path writes overrides straight into the map including a nil,
// where the COPY path deletes the key. A presence-only filter therefore
// suppresses a real drop on a move whenever the caller nulls the key.
func TestStillDropped(t *testing.T) {
	cases := []struct {
		name    string
		dropped []string
		final   map[string]any
		want    []string
	}{
		{
			name:    "key restored by an override is no longer dropped",
			dropped: []string{"count"},
			final:   map[string]any{"count": "7"},
			want:    nil,
		},
		{
			name:    "key still absent stays dropped",
			dropped: []string{"count"},
			final:   map[string]any{"status": "open"},
			want:    []string{"count"},
		},
		{
			name:    "key present but NIL is still dropped",
			dropped: []string{"count"},
			final:   map[string]any{"count": nil},
			want:    []string{"count"},
		},
		{
			// Falsy but PRESENT values are restorations. A truthiness-based
			// filter drops these back into the report, telling the user their
			// `false` / `0` / `""` was discarded when it is on the item.
			name:    "false is a restored value, not an absent one",
			dropped: []string{"archived"},
			final:   map[string]any{"archived": false},
			want:    nil,
		},
		{
			name:    "zero is a restored value",
			dropped: []string{"count"},
			final:   map[string]any{"count": float64(0)},
			want:    nil,
		},
		{
			name:    "empty string is a restored value",
			dropped: []string{"note"},
			final:   map[string]any{"note": ""},
			want:    nil,
		},
		{
			name:    "output is sorted and de-duplicated",
			dropped: []string{"zeta", "alpha", "zeta"},
			final:   map[string]any{},
			want:    []string{"alpha", "zeta"},
		},
		{
			name:    "empty in, nil out",
			dropped: nil,
			final:   map[string]any{"count": "7"},
			want:    nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := StillDropped(tc.dropped, tc.final)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("StillDropped(%#v, %#v) = %#v, want %#v", tc.dropped, tc.final, got, tc.want)
			}
		})
	}
}

// A grandfathered schema may still declare a reserved key. Validating the
// MIGRATED map against that FieldDef would reject the system-owned value
// MigrateFields hands through by identity, failing the whole move.
//
// Scoped to migration deliberately: the same skip inside ValidateFieldsDetailed
// would stop enforcing the declaration on create and full update too, where the
// user really is authoring that key — and fields_patch would keep rejecting it,
// so full and partial updates would disagree (Codex round 3).
func TestSchemaForMigratedFields(t *testing.T) {
	t.Run("strips a grandfathered reserved declaration", func(t *testing.T) {
		// The reserved key is FIRST on purpose. With it last, an in-place
		// implementation (`out.Fields = schema.Fields[:0]` + appends) writes
		// the surviving field back into the slot it already occupied, so the
		// input looks untouched and the mutant survives. Putting it first
		// means the survivor lands in slot 0 and the corruption is visible.
		// Found by running that exact mutant against the first version of
		// this test, which passed.
		schema := models.CollectionSchema{Fields: []models.FieldDef{
			{Key: "implementation_notes", Type: "text", Required: true},
			{Key: "status", Type: "select", Options: []string{"open"}},
		}}

		out := SchemaForMigratedFields(schema)

		for _, f := range out.Fields {
			if f.Key == "implementation_notes" {
				t.Fatal("reserved declaration must not reach migration validation")
			}
		}
		if len(out.Fields) != 1 || out.Fields[0].Key != "status" {
			t.Fatalf("ordinary fields must survive: %#v", out.Fields)
		}
		// The input must not be mutated — it is the collection's real schema
		// and other callers still need the declaration to enforce it.
		//
		// Asserted on CONTENTS, not length. Go passes the struct by value, so
		// the caller's slice HEADER survives an in-place rewrite of the
		// backing array: `out.Fields = schema.Fields[:0]` followed by appends
		// leaves len(schema.Fields) == 2 while both elements have been
		// overwritten. A length check passes that mutant, which it did on the
		// first version of this test.
		if len(schema.Fields) != 2 ||
			schema.Fields[0].Key != "implementation_notes" ||
			schema.Fields[1].Key != "status" {
			t.Errorf("input schema was mutated: %#v", schema.Fields)
		}
	})

	t.Run("strips every reserved key, not just implementation_notes", func(t *testing.T) {
		// A mutant that special-cases one key passes a single-key fixture.
		// Driven off the canonical set so a key added later is covered here
		// without an edit.
		fields := []models.FieldDef{{Key: "status", Type: "select", Options: []string{"open"}}}
		for _, k := range models.ReservedItemFieldKeys() {
			fields = append(fields, models.FieldDef{Key: k, Type: "text"})
		}

		out := SchemaForMigratedFields(models.CollectionSchema{Fields: fields})

		if len(out.Fields) != 1 || out.Fields[0].Key != "status" {
			t.Fatalf("every reserved key must be stripped, got %#v", out.Fields)
		}
	})

	t.Run("a schema with no reserved key is returned unchanged", func(t *testing.T) {
		schema := models.CollectionSchema{Fields: []models.FieldDef{
			{Key: "status", Type: "select", Options: []string{"open"}},
		}}
		out := SchemaForMigratedFields(schema)
		if !reflect.DeepEqual(out, schema) {
			t.Errorf("got %#v, want the input unchanged", out)
		}
	})
}

// A GRANDFATHERED schema can still declare a reserved key. Such a FieldDef must
// not reach the defaults/required pass: a Default would be injected into system
// metadata as though a user had authored it, and Required would raise a
// migration ERROR for a key the user has no way to supply.
//
// That error is the reachable half. Bulk move rejects on result.Errors BEFORE
// it reaches the stripped-schema validation, so a legacy target requiring
// implementation_notes failed bulk move while single move and copy succeeded —
// the same key on the same item getting two answers depending on which button
// was pressed (Codex round 4).
func TestMigrateFields_GrandfatheredReservedDeclarationIsInert(t *testing.T) {
	source := []models.FieldDef{}
	target := []models.FieldDef{
		{Key: "status", Type: "select", Options: []string{"open"}, Required: true, Default: "open"},
		{Key: "implementation_notes", Type: "text", Required: true, Default: "SHOULD NOT BE INJECTED"},
		{Key: "github_pr", Type: "text", Required: true},
	}

	result := MigrateFields(map[string]any{}, source, target, SameWorkspace)

	if len(result.Errors) != 0 {
		t.Errorf("a reserved declaration must not produce a required-field error — "+
			"the user cannot supply it and bulk move rejects on this list: %#v", result.Errors)
	}
	for _, key := range []string{"implementation_notes", "github_pr"} {
		if v, ok := result.Fields[key]; ok {
			t.Errorf("%s must not receive a schema default (%#v) — system metadata is not "+
				"authored through a FieldDef", key, v)
		}
	}
	// Control: the ordinary field's default still applies. Without this, a
	// mutant that skipped the whole defaults pass would pass the assertions
	// above.
	if result.Fields["status"] != "open" {
		t.Errorf("ordinary defaults must still apply, got %#v", result.Fields["status"])
	}
}
