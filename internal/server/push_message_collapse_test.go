package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file is one half of a cross-language contract test (PLAN-2558 S3).
//
// handlePushToItem measures a push message in runes AFTER whitespace collapse
// and REJECTS an over-length one with a 400 rather than truncating it. The web
// composer has to apply the same accounting to warn the user before they
// submit — but "the same" is a claim about Go's unicode.IsSpace vs
// JavaScript's \s, and those two genuinely differ (U+0085 is whitespace to Go
// only; U+FEFF to JS only). A TypeScript-only test would assert the author's
// BELIEF about Go, not Go's behaviour.
//
// So both suites read one fixture. This test pins it to what the server
// actually does; web/src/lib/push/message.test.ts pins the client to the same
// table. If either implementation drifts, exactly one of the two goes red and
// names the case.
func TestPushMessageCollapseFixture(t *testing.T) {
	type collapseCase struct {
		Name      string `json:"name"`
		Raw       string `json:"raw"`
		Collapsed string `json:"collapsed"`
		Runes     int    `json:"runes"`
	}
	var fixture struct {
		Cases []collapseCase `json:"cases"`
	}

	path := filepath.Join("testdata", "push_message_cases.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	// Guard against a fixture that silently empties out (a bad merge, a
	// rewritten generator): an empty table would make this test pass while
	// proving nothing, which is the exact false-green shape CONVE-12 names.
	if len(fixture.Cases) < 20 {
		t.Fatalf("fixture has %d cases, expected the full table — did it get truncated?", len(fixture.Cases))
	}

	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			// The exact expression handlePushToItem applies to input.Message.
			got := strings.Join(strings.Fields(tc.Raw), " ")
			if got != tc.Collapsed {
				t.Errorf("collapse(%q) = %q, fixture says %q", tc.Raw, got, tc.Collapsed)
			}
			// The exact expression its length check applies to the result.
			if n := len([]rune(got)); n != tc.Runes {
				t.Errorf("len([]rune(collapse(%q))) = %d, fixture says %d", tc.Raw, n, tc.Runes)
			}
		})
	}
}

// The fixture's whitespace cases are only meaningful if they agree with the
// constant the handler actually enforces. A composer that bounds at a
// hard-coded 4096 while the server moved on is wrong in whichever direction
// the constant went, so pin the number the web client mirrors
// (PUSH_MESSAGE_MAX_LEN in web/src/lib/push/message.ts).
func TestMaxPushMessageLenIsMirroredByTheWebComposer(t *testing.T) {
	const mirroredInWebClient = 4096
	if maxPushMessageLen != mirroredInWebClient {
		t.Fatalf("maxPushMessageLen = %d but web/src/lib/push/message.ts still says %d — update PUSH_MESSAGE_MAX_LEN in lockstep",
			maxPushMessageLen, mirroredInWebClient)
	}
}
