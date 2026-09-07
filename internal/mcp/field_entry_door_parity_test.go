package mcp

import (
	"strings"
	"testing"
)

// BUG-2870: the remote /mcp door and the CLI doors must answer the same way for
// the same `key=value` entry. The three BUG-2850 tests that mention this door's
// trimming all exercise the CATALOG conflict pass, which never reaches
// ingestFieldKVP — so before this test nothing in the suite asserted what THIS
// door actually stored, which is the whole subject of the bug.
//
// The CLI half of the parity claim is pinned in cmd/pad
// (TestItemUpdateFieldEntry_PaddedKeyRefused); the shared rule itself is pinned
// in internal/items (TestSplitFieldEntry). This file is the door.

// A padded KEY is refused here now. It used to be trimmed, silently, so the
// same call wrote `effort` through this door and an undeclared " effort"
// through the CLI.
func TestParseFieldKVP_PaddedKeyRefused(t *testing.T) {
	for _, entry := range []string{" effort=l", "effort =l", "\teffort=l"} {
		t.Run(entry, func(t *testing.T) {
			got, err := parseFieldKVP([]any{entry})
			if err == nil {
				t.Fatalf("parseFieldKVP(%q) = %v, want a refusal", entry, got)
			}
			if !strings.Contains(err.Error(), "whitespace around its key") {
				t.Errorf("refusal should name the problem; got %q", err)
			}
			if !strings.Contains(err.Error(), `"effort=l"`) {
				t.Errorf("refusal should show the corrected entry; got %q", err)
			}
		})
	}
}

// A padded VALUE reaches the item verbatim now. This is the behaviour change a
// caller can notice: this door used to trim it (and therefore type it), so a
// padded value silently succeeded here and was refused at the CLI. Now both
// doors pass the bytes down and validation gives the same answer to both.
func TestParseFieldKVP_PaddedValueVerbatim(t *testing.T) {
	cases := map[string]string{
		"cost= 3":  " 3",
		"note=x ":  "x ",
		"note= ":   " ",
		"note=a b": "a b",
	}
	for entry, want := range cases {
		t.Run(entry, func(t *testing.T) {
			got, err := parseFieldKVP([]any{entry})
			if err != nil {
				t.Fatalf("parseFieldKVP(%q): %v", entry, err)
			}
			key := strings.SplitN(entry, "=", 2)[0]
			if got[key] != want {
				t.Errorf("parseFieldKVP(%q)[%q] = %q, want %q — a trimmed value here is "+
					"this door reinterpreting the caller's bytes", entry, key, got[key], want)
			}
		})
	}
}

// A refusal must abort the whole call rather than dropping one entry: a caller
// that asked for two fields and silently got one has no way to tell.
func TestParseFieldKVP_RefusalAbortsTheCall(t *testing.T) {
	got, err := parseFieldKVP([]any{"status=done", " effort=l"})
	if err == nil {
		t.Fatalf("parseFieldKVP = %v, want a refusal for the padded entry", got)
	}
	if got != nil {
		t.Errorf("a refused call must return no partial map; got %v", got)
	}
}
