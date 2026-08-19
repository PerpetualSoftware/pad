package items

import (
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestUndeclaredOverrideKeys pins the shared rule both cross-workspace-copy
// consumers depend on (PLAN-2357 / TASK-2365). The SORT is part of the
// contract, not a convenience: both callers put the keys straight into a 400's
// message, and the copy preflight is specified to return identical bytes for
// identical input — a map-iteration-ordered list would break that on every
// other call.
func TestUndeclaredOverrideKeys(t *testing.T) {
	defs := []models.FieldDef{
		{Key: "status", Type: "select"},
		{Key: "priority", Type: "select"},
	}

	cases := []struct {
		name      string
		overrides map[string]any
		want      []string
	}{
		{"nil map", nil, nil},
		{"empty map", map[string]any{}, nil},
		{"all declared", map[string]any{"status": "open", "priority": "low"}, nil},
		{"one undeclared", map[string]any{"nope": 1}, []string{"nope"}},
		{
			"several undeclared come back sorted",
			map[string]any{"zeta": 1, "alpha": 2, "status": "open", "mid": 3},
			[]string{"alpha", "mid", "zeta"},
		},
		{
			// A nil VALUE still names a real key; only the key set matters
			// here. Whether a nil unsets or assigns is the merge loop's
			// business, and it never gets to decide for an undeclared key.
			"a nil value is still judged by its key",
			map[string]any{"status": nil, "nope": nil},
			[]string{"nope"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := UndeclaredOverrideKeys(tc.overrides, defs); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}

	// A schema with no fields declares nothing, so every override is an
	// orphan. The empty-schema case is reachable — a collection created with
	// no fields is legal.
	got := UndeclaredOverrideKeys(map[string]any{"b": 1, "a": 2}, nil)
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("empty schema: got %v, want [a b]", got)
	}
}

// Reserved keys are never settable through a field-override map, on any path
// (Codex round 4). Two distinct holes closed by one rule:
//
//   - the MOVE path has no declared-key gate at all, so an override could
//     write arbitrary junk straight into implementation_notes, bypassing both
//     the schema (which does not declare it) and the migrated-output
//     validation (which strips it);
//   - on the COPY it additionally defeats the scope rule — MigrateFields drops
//     github_pr when the item leaves its workspace, and an override applied
//     afterwards puts it straight back.
func TestReservedOverrideKeys(t *testing.T) {
	cases := []struct {
		name      string
		overrides map[string]any
		want      []string
	}{
		{
			name:      "ordinary overrides are allowed",
			overrides: map[string]any{"status": "open", "priority": "high"},
			want:      nil,
		},
		{
			name:      "a reserved key is refused",
			overrides: map[string]any{"status": "open", "implementation_notes": "junk"},
			want:      []string{"implementation_notes"},
		},
		{
			name: "the scope-defeating one is refused",
			// This is the case that matters most: MigrateFields has just
			// dropped github_pr for leaving its workspace.
			overrides: map[string]any{"github_pr": map[string]any{"number": 1}},
			want:      []string{"github_pr"},
		},
		{
			name:      "sorted, so the error message is stable",
			overrides: map[string]any{"github_pr": 1, "convention": 2, "decision_log": 3},
			want:      []string{"convention", "decision_log", "github_pr"},
		},
		{
			// A nil override means "unset" on the copy path. It is still an
			// attempt to write a reserved key and is still refused — silently
			// ignoring it would let a caller CLEAR system metadata through a
			// door that is supposed to be shut.
			name:      "a nil value does not make it allowed",
			overrides: map[string]any{"decision_log": nil},
			want:      []string{"decision_log"},
		},
		{
			name:      "empty map",
			overrides: nil,
			want:      nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ReservedOverrideKeys(tc.overrides)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ReservedOverrideKeys(%#v) = %#v, want %#v", tc.overrides, got, tc.want)
			}
		})
	}
}
