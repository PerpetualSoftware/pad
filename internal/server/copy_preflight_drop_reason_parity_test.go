package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The copy preflight's `dropped[].reason` is a wire enum with a renderer on
// the other side of a language boundary, and nothing made the two meet.
//
// This is not a hypothetical gap. BUG-2674 added `referent_not_portable`
// server-side; the TypeScript union and CopyItemDialog's `dropReason` switch
// never learned it, so it fell through `default: return reason` and the dialog
// showed a user the raw string `referent_not_portable`. It stayed unnoticed
// because only `github_pr` produced it. TASK-2878 made it the reason for EVERY
// carried relation value on a cross-workspace copy, and added three more
// (`not_found`, `wrong_collection`, `target_missing`) — turning a rare wart
// into the common case.
//
// So: the Go side ENUMERATES the vocabulary (preflightDropReasons), and this
// test requires each entry to exist in both consumers. A reason added without
// a renderer fails here rather than in front of a user.
//
// WHAT THIS DOES NOT CLAIM. It checks that a case EXISTS, not that its
// sentence is good — no test can judge that. It also cannot see a renderer
// that handles a reason the server never sends; that direction is harmless
// (dead branch) and the opposite one is the defect.

func readWebFile(t *testing.T, rel string) string {
	t.Helper()
	path := filepath.Join("..", "..", rel)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s (%s): %v", rel, path, err)
	}
	return string(b)
}

// dropReasonSwitchBody returns just the body of CopyItemDialog's `dropReason`
// function. Scoped rather than searched whole: the component has other
// switches, and one of them matching `case 'not_found':` for an unrelated
// purpose would satisfy this test while the dialog still rendered the raw enum
// — a guard that passes on the wrong evidence.
func dropReasonSwitchBody(t *testing.T, src string) string {
	t.Helper()
	const marker = "function dropReason(reason: string): string {"
	start := strings.Index(src, marker)
	if start < 0 {
		t.Fatalf("CopyItemDialog.svelte no longer declares %q — this gate is reading for a "+
			"function that moved or was renamed, so its green means nothing until it is repointed", marker)
	}
	rest := src[start+len(marker):]
	// The function's own closing brace: the first line that is exactly a tab
	// followed by `}`, matching the component's indentation for a top-level
	// declaration in <script>.
	end := strings.Index(rest, "\n\t}")
	if end < 0 {
		t.Fatalf("could not find the end of dropReason's body")
	}
	return rest[:end]
}

func TestCopyPreflightDropReasonsAreRenderedByTheDialog(t *testing.T) {
	reasons := preflightDropReasons()
	if len(reasons) < 6 {
		t.Fatalf("preflightDropReasons() returned %d entries; the enumeration looks truncated, "+
			"and a short list makes this whole gate pass vacuously", len(reasons))
	}

	types := readWebFile(t, "web/src/lib/types/index.ts")
	dialog := readWebFile(t, "web/src/lib/components/items/CopyItemDialog.svelte")
	switchBody := dropReasonSwitchBody(t, dialog)

	for _, reason := range reasons {
		t.Run(reason, func(t *testing.T) {
			if !strings.Contains(types, "| '"+reason+"'") {
				t.Errorf("ItemCopyPreflightDropped['reason'] in web/src/lib/types/index.ts has no "+
					"member %q — the server can send it and the client's type says it cannot", reason)
			}
			if !strings.Contains(switchBody, "case '"+reason+"':") {
				t.Errorf("CopyItemDialog's dropReason() has no case for %q, so the dialog falls "+
					"through to `default: return reason` and shows a user the raw enum string", reason)
			}
		})
	}
}
