package server

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// TASK-3462 U3a, codex r1: the replaced-fields summary.
func TestTASK3462U3a_ReplacedFieldsSummary(t *testing.T) {
	// Item fields keep their numbers as written (DecodeJSONKeepingNumbers),
	// so 1e3 and 1000 must not read as a change.
	if got := builtinReplacedFields(
		map[string]any{"n": json.Number("1e3")},
		map[string]any{"n": json.Number("1000")},
	); got != "" {
		t.Fatalf("numerically equal values listed as replaced: %q", got)
	}

	// A long value is bounded at 120 bytes, ellipsis included, and never
	// cut inside a multibyte character.
	long := strings.Repeat("é", 200) // 2 bytes each
	got := builtinReplacedFields(map[string]any{"s": long}, map[string]any{"s": "short"})
	if !utf8.ValidString(got) {
		t.Fatalf("summary is not valid UTF-8: %q", got)
	}
	from := strings.TrimSuffix(strings.SplitN(strings.TrimPrefix(got, "s: "), " → ", 2)[0], "")
	if len(from) > 120 {
		t.Fatalf("shown value is %d bytes, want at most 120: %q", len(from), from)
	}
	if !strings.HasSuffix(from, "…") {
		t.Fatalf("a truncated value does not say so: %q", from)
	}
}
