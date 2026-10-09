package store

import (
	"encoding/json"
	"strings"
	"testing"
)

// TASK-2257: the reserved convention copy follows the ordinary keys a write
// changed.

func mirrorOf(t *testing.T, before, after string) map[string]any {
	t.Helper()
	out, err := mirrorConventionMetadata(before, after)
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	c, _ := m["convention"].(map[string]any)
	return c
}

const stored = `{"trigger":"always","scope":"all","priority":"should","enforcement":"should","convention":{"trigger":"always","surfaces":["all"],"enforcement":"should"}}`

func TestMirrorConvention_ChangedKeysFlowIntoTheCopy(t *testing.T) {
	after := `{"trigger":"on-commit","scope":"all","priority":"must","enforcement":"must","convention":{"trigger":"always","surfaces":["all"],"enforcement":"should"}}`
	c := mirrorOf(t, stored, after)
	if c["trigger"] != "on-commit" || c["enforcement"] != "must" {
		t.Fatalf("copy = %v, want trigger on-commit, enforcement must", c)
	}
	if s, _ := c["surfaces"].([]any); len(s) != 1 || s[0] != "all" {
		t.Fatalf("untouched surfaces changed: %v", c["surfaces"])
	}
}

func TestMirrorConvention_ScopeAndPriorityAreTheFallbackKeys(t *testing.T) {
	after := `{"trigger":"always","scope":"cli","priority":"nice-to-have","enforcement":"should","convention":{"trigger":"always","surfaces":["all"],"enforcement":"should"}}`
	c := mirrorOf(t, stored, after)
	if s, _ := c["surfaces"].([]any); len(s) != 1 || s[0] != "cli" {
		t.Fatalf("scope-only write: surfaces = %v, want [cli]", c["surfaces"])
	}
	if c["enforcement"] != "nice-to-have" {
		t.Fatalf("priority-only write: enforcement = %v, want nice-to-have", c["enforcement"])
	}
}

func TestMirrorConvention_NoCopyNoChange(t *testing.T) {
	before := `{"trigger":"always","n":12345678901234567890}`
	after := `{"trigger":"on-commit","n":12345678901234567890}`
	out, err := mirrorConventionMetadata(before, after)
	if err != nil || out != after {
		t.Fatalf("an item without a reserved copy must pass through byte for byte: %q, %v", out, err)
	}
}

func TestMirrorConvention_UnmirroredChangeLeavesBytesAlone(t *testing.T) {
	after := `{"trigger":"always","scope":"all","priority":"should","enforcement":"should","status":"draft","convention":{"trigger":"always","surfaces":["all"],"enforcement":"should"}}`
	out, err := mirrorConventionMetadata(stored, after)
	if err != nil || out != after {
		t.Fatalf("a write touching no mirrored key must not re-encode the blob: %q, %v", out, err)
	}
}

func TestMirrorConvention_KeepsLargeIntegersExact(t *testing.T) {
	before := `{"trigger":"always","n":12345678901234567890,"convention":{"trigger":"always"}}`
	after := `{"trigger":"on-commit","n":12345678901234567890,"convention":{"trigger":"always"}}`
	out, err := mirrorConventionMetadata(before, after)
	if err != nil {
		t.Fatal(err)
	}
	if want := `12345678901234567890`; !strings.Contains(out, `"n":`+want) {
		t.Fatalf("re-encoding rounded a large integer: %s", out)
	}
}
