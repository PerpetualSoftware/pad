package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type pushCollapseCase struct {
	Name      string `json:"name"`
	Raw       string `json:"raw"`
	Collapsed string `json:"collapsed"`
	Runes     int    `json:"runes"`
}

func loadPushCollapseCases(t *testing.T) []pushCollapseCase {
	t.Helper()
	var fixture struct {
		Cases []pushCollapseCase `json:"cases"`
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "push_message_cases.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	// Guard against a fixture that silently empties out (a bad merge, a
	// rewritten generator): an empty table would make these tests pass while
	// proving nothing, which is the exact false-green shape CONVE-12 names.
	if len(fixture.Cases) < 20 {
		t.Fatalf("fixture has %d cases, expected the full table — did it get truncated?", len(fixture.Cases))
	}
	return fixture.Cases
}

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
	for _, tc := range loadPushCollapseCases(t) {
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

// The test above asserts the EXPRESSION handlePushToItem uses, copied into the
// test — which cannot notice the handler switching to a different
// normalization (codex review). This one drives the real endpoint for every
// fixture case and reads the collapsed form back off the response, so the
// table is pinned to the code path a client actually talks to.
//
// The two are kept separate rather than merged: this one needs a server, a
// user and a workspace per case, while the expression-level test is instant
// and pinpoints WHICH of collapse-vs-count disagrees when one does.
func TestPushMessageCollapseThroughTheHandler(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)
	path := "/api/v1/workspaces/" + slug + "/items/" + item.Slug + "/push"

	for _, tc := range loadPushCollapseCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			rr := bearerJSON(t, srv, "POST", path, tok.Token,
				map[string]interface{}{"message": tc.Raw})

			// A case that collapses to nothing is the handler's own 400
			// condition — and the 400 IS the assertion that the collapse
			// emptied it, so it is evidence rather than an exempted case.
			if tc.Collapsed == "" {
				if rr.Code != http.StatusBadRequest {
					t.Fatalf("expected 400 for a message that collapses to empty, got %d (%s)", rr.Code, rr.Body.String())
				}
				return
			}

			if rr.Code != http.StatusOK {
				t.Fatalf("push: %d %s", rr.Code, rr.Body.String())
			}
			var resp pushResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
				t.Fatalf("parse response: %v", err)
			}
			// pushResponse.Message echoes the post-collapse text — the exact
			// bytes that went onto the bus as Notification.Summary.
			if resp.Message != tc.Collapsed {
				t.Errorf("handler collapsed %q to %q, fixture says %q", tc.Raw, resp.Message, tc.Collapsed)
			}
			if n := len([]rune(resp.Message)); n != tc.Runes {
				t.Errorf("handler produced %d runes for %q, fixture says %d", n, tc.Raw, tc.Runes)
			}
		})
	}
}

// The fixture's whitespace cases are only meaningful if they agree with the
// bound the handler actually enforces. Asserted THROUGH the endpoint at the
// boundary — 4096 collapsed runes accepted, 4097 refused — rather than by
// comparing the constant to a copy of itself, so the web composer's
// PUSH_MESSAGE_MAX_LEN is pinned to observed behaviour.
func TestPushMessageBoundIsWhatTheWebComposerMirrors(t *testing.T) {
	t.Parallel()
	// The number hard-coded in web/src/lib/push/message.ts.
	const mirroredInWebClient = 4096

	srv := testServerWithWatchEvents(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)
	path := "/api/v1/workspaces/" + slug + "/items/" + item.Slug + "/push"

	atLimit := strings.Repeat("a", mirroredInWebClient)
	if rr := bearerJSON(t, srv, "POST", path, tok.Token,
		map[string]interface{}{"message": atLimit}); rr.Code != http.StatusOK {
		t.Fatalf("a %d-rune message was refused (%d %s) — the server bounds lower than the web composer, which will refuse text users are entitled to send",
			mirroredInWebClient, rr.Code, rr.Body.String())
	}

	overLimit := strings.Repeat("a", mirroredInWebClient+1)
	if rr := bearerJSON(t, srv, "POST", path, tok.Token,
		map[string]interface{}{"message": overLimit}); rr.Code != http.StatusBadRequest {
		t.Fatalf("a %d-rune message was accepted (%d) — the server bounds higher than the web composer, which will block text the server would take",
			mirroredInWebClient+1, rr.Code)
	}
}
