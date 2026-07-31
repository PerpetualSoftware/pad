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
