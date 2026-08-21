package main

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

// BUG-2699 codex round 9 — the CLI's half of "is this safe to re-run?".
//
// The push handler publishes BEFORE it writes its response, so an error is
// not proof that nothing went out. The web dialog has distinguished the
// two since TASK-2588; the CLI reported every failure identically, so the
// same server outcome produced a safe message in a browser and a
// misleading one in a terminal.
//
// The default matters more than any single code: anything unrecognised
// must read as POSSIBLY DELIVERED, because being wrong that way costs a
// warning while being wrong the other way costs a duplicate dispatch into
// an agent harness.
func TestPushRefusedBeforePublishing(t *testing.T) {
	t.Parallel()

	t.Run("recognised pre-publish refusals are safe to re-run", func(t *testing.T) {
		for _, code := range []string{
			"bad_request", "unauthorized", "not_found", "forbidden",
			"permission_denied", "unavailable", "rate_limited",
			"plan_limit_exceeded", "csrf_error", "email_not_verified",
		} {
			if !pushRefusedBeforePublishing(&cli.APIError{Code: code, Message: "x"}) {
				t.Errorf("%s should be recognised as a pre-publish refusal", code)
			}
		}
	})

	t.Run("push_unconfirmed is NOT safe", func(t *testing.T) {
		// The code the server emits precisely to say it does not know. If
		// this were ever added to the map, the CLI would tell users to
		// re-run the one case that is most likely to duplicate.
		if pushRefusedBeforePublishing(&cli.APIError{Code: "push_unconfirmed", Message: "x"}) {
			t.Fatal("push_unconfirmed must never be treated as a pre-publish refusal")
		}
	})

	t.Run("unknown shapes default to possibly-delivered", func(t *testing.T) {
		cases := []struct {
			name string
			err  error
		}{
			{"transport error", errors.New("request failed: connection reset")},
			{"unrecognised code", &cli.APIError{Code: "bad_gateway", Message: "x"}},
			{"empty code", &cli.APIError{Code: "", Message: "x"}},
			{"gateway envelope", errors.New("API error: 502 <html>bad gateway</html>")},
		}
		for _, tc := range cases {
			if pushRefusedBeforePublishing(tc.err) {
				t.Errorf("%s must default to possibly-delivered", tc.name)
			}
		}
	})

	t.Run("wrapped API errors are still recognised", func(t *testing.T) {
		// errors.As, not a type assertion: a wrapped refusal is still a
		// refusal, and treating it as unknown would warn on a case that is
		// provably safe.
		wrapped := errors.Join(errors.New("context"), &cli.APIError{Code: "bad_request", Message: "x"})
		if !pushRefusedBeforePublishing(wrapped) {
			t.Fatal("a wrapped pre-publish refusal must still be recognised")
		}
	})
}

// TestPushRefusalCodesMatchTheWebAllowList pins the two surfaces together.
//
// They answer the same question about the same endpoint, so drift means a
// push that is safe to retry in a browser and unsafe in a terminal, or the
// reverse. Checked against the TypeScript source rather than a copy of it,
// so editing one side without the other fails here.
func TestPushRefusalCodesMatchTheWebAllowList(t *testing.T) {
	t.Parallel()
	webCodes := readWebPrePublishCodes(t)
	for code := range pushPrePublishRefusalCodes {
		if !webCodes[code] {
			t.Errorf("%q is a pre-publish refusal in the CLI but not in web/src/lib/push/dispatch.ts", code)
		}
	}
	for code := range webCodes {
		if !pushPrePublishRefusalCodes[code] {
			t.Errorf("%q is a pre-publish refusal in web/src/lib/push/dispatch.ts but not in the CLI", code)
		}
	}
	// POSITIVE CONTROL: a parse that found nothing would make both loops
	// vacuous and report agreement between a list and an empty set.
	if len(webCodes) == 0 {
		t.Fatal("parsed no codes out of the web allow-list — the parser is broken, not the lists")
	}
}

// readWebPrePublishCodes extracts the string literals from
// PUSH_PRE_PUBLISH_ERROR_CODES in the web client.
func readWebPrePublishCodes(t *testing.T) map[string]bool {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "lib", "push", "dispatch.ts"))
	if err != nil {
		t.Fatalf("read web dispatch.ts: %v", err)
	}
	text := string(src)
	start := strings.Index(text, "PUSH_PRE_PUBLISH_ERROR_CODES")
	if start < 0 {
		t.Fatal("PUSH_PRE_PUBLISH_ERROR_CODES not found — it was renamed, and this pin needs updating with it")
	}
	open := strings.Index(text[start:], "([")
	closeIdx := strings.Index(text[start:], "])")
	if open < 0 || closeIdx < 0 || closeIdx < open {
		t.Fatal("could not locate the code list body")
	}
	body := text[start+open : start+closeIdx]

	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(body, -1) {
		out[m[1]] = true
	}
	return out
}
