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

// dropReasonMapBody returns the body of the renderer's message map.
//
// REPOINTED IN IDEA-2894, and the repointing is the reason this helper still
// earns its keep. The mapper moved out of CopyItemDialog.svelte into
// `web/src/lib/items/copyDropReasons.ts` so it could be unit-tested at all,
// and this gate FAILED LOUDLY on that move — exactly as the fatal below says
// it should — rather than passing on a file that no longer contained what it
// was reading for. A parity gate that cannot tell "no such reason" from "no
// such function" is worse than none.
//
// Scoped to the map's body rather than searching the file whole for the same
// reason the old version scoped to the switch: the module also exports the
// reason LIST, and a reason present there but missing a message would satisfy
// a whole-file search while the dialog still rendered the raw enum.
func dropReasonMapBody(t *testing.T, src string) string {
	t.Helper()
	const marker = "const MESSAGES: Record<CopyDropReason, string> = {"
	start := strings.Index(src, marker)
	if start < 0 {
		t.Fatalf("copyDropReasons.ts no longer declares %q — this gate is reading for a "+
			"declaration that moved or was renamed, so its green means nothing until it is "+
			"repointed", marker)
	}
	rest := src[start+len(marker):]
	// The literal's own closing brace: the first line that is exactly `};` at
	// column zero, matching a top-level declaration in the module.
	end := strings.Index(rest, "\n};")
	if end < 0 {
		t.Fatalf("could not find the end of the MESSAGES map")
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
	renderer := readWebFile(t, "web/src/lib/items/copyDropReasons.ts")
	messages := dropReasonMapBody(t, renderer)

	for _, reason := range reasons {
		t.Run(reason, func(t *testing.T) {
			if !strings.Contains(types, "| '"+reason+"'") {
				t.Errorf("ItemCopyPreflightDropped['reason'] in web/src/lib/types/index.ts has no "+
					"member %q — the server can send it and the client's type says it cannot", reason)
			}
			// BOTH halves of the renderer, because they fail differently.
			// Missing from the LIST and the module's own completeness test
			// cannot see it either — that test iterates the list, so a reason
			// absent from it is absent from the test as well, and this gate is
			// the only thing that notices.
			if !strings.Contains(renderer, "\t'"+reason+"',") {
				t.Errorf("COPY_DROP_REASONS in web/src/lib/items/copyDropReasons.ts does not list "+
					"%q. That list drives the module's own completeness test, so a reason missing "+
					"from it is invisible to that test too — this gate is the only place it shows.",
					reason)
			}
			if !strings.Contains(messages, reason+":") {
				t.Errorf("copyDropReasons' MESSAGES map has no entry for %q, so "+
					"copyDropReasonMessage falls through to returning the raw string and shows a "+
					"user the enum", reason)
			}
		})
	}
}
