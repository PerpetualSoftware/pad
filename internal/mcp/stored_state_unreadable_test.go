package mcp

// BUG-2675 — the retry-hostile classification for "the item's stored state is
// unreadable", on BOTH transports.
//
// Both halves are tested here on purpose. v0.16 shipped a fix that reached the
// remote transport only and v0.17 had to finish it a version later; the way
// that happens is a test file that proves the half its author was looking at.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestStructuredAppendErrorResultClassifiesTheRefusal is the HTTP-transport
// half: the append guard's sentinel becomes stored_state_unreadable, and
// anything else keeps dispatcherErrorResult's server_error.
//
// The second leg is the one that makes the first mean something. Without it a
// classifier that returned stored_state_unreadable for EVERY error would pass.
func TestStructuredAppendErrorResultClassifiesTheRefusal(t *testing.T) {
	refusal := fmt.Errorf("%w: %q holds a value that is not a list of entries",
		models.ErrStructuredFieldUnreadable, models.ItemFieldImplementationNotes)

	res := structuredAppendErrorResult("item note", "append note", refusal)
	env, ok := res.StructuredContent.(ErrorEnvelope)
	if !ok {
		t.Fatalf("expected ErrorEnvelope, got %T", res.StructuredContent)
	}
	if env.Error.Code != ErrStoredStateUnreadable {
		t.Fatalf("code: got %q, want %q", env.Error.Code, ErrStoredStateUnreadable)
	}
	if !res.IsError {
		t.Error("result must carry IsError")
	}
	// The whole point of the code is that an agent stops instead of retrying,
	// so the hint has to say so where the agent reads it.
	if !strings.Contains(strings.ToLower(env.Error.Hint), "retry") {
		t.Errorf("hint must tell the agent retrying is pointless; got: %s", env.Error.Hint)
	}
	if !strings.Contains(env.Error.Message, models.ItemFieldImplementationNotes) {
		t.Errorf("message must carry the refusal text naming the field; got: %s", env.Error.Message)
	}

	// Control leg: an ordinary dispatcher fault is NOT retry-hostile and must
	// keep its old classification.
	other := structuredAppendErrorResult("item note", "encode body", errors.New("boom"))
	otherEnv, ok := other.StructuredContent.(ErrorEnvelope)
	if !ok {
		t.Fatalf("expected ErrorEnvelope, got %T", other.StructuredContent)
	}
	if otherEnv.Error.Code != ErrServerError {
		t.Errorf("unrelated dispatcher error: got %q, want %q", otherEnv.Error.Code, ErrServerError)
	}
}

// TestStdioSurfacesStoredStateUnreadable is the stdio-transport half, driven
// end to end through the REAL writer: the bytes the CLI puts on stderr are the
// bytes the classifier parses. A hand-written marker line would test the
// classifier against a fixture I control on both sides, which is precisely the
// pair that can drift.
func TestStdioSurfacesStoredStateUnreadable(t *testing.T) {
	refusal := fmt.Errorf("%w: %q holds a value that is not a list of entries, so appending would overwrite and destroy it",
		models.ErrStructuredFieldUnreadable, models.ItemFieldDecisionLog)

	var stderr bytes.Buffer
	cli.WriteStoredStateUnreadableError(&stderr, refusal)

	res := extractStructuredCLIError(stderr.String())
	if res == nil {
		t.Fatal("stdio classifier did not recognise the CLI's marker line — " +
			"an agent on this transport would receive server_error and retry a permanent failure")
	}
	env, ok := res.StructuredContent.(ErrorEnvelope)
	if !ok {
		t.Fatalf("expected ErrorEnvelope, got %T", res.StructuredContent)
	}
	if env.Error.Code != ErrStoredStateUnreadable {
		t.Fatalf("code: got %q, want %q", env.Error.Code, ErrStoredStateUnreadable)
	}
	if !strings.Contains(env.Error.Message, models.ItemFieldDecisionLog) {
		t.Errorf("message must carry the refusal text; got: %s", env.Error.Message)
	}
	// Codex round 2: the same CODE with different GUIDANCE per transport is
	// the same gap as a code only one transport emits. Compared against the
	// HTTP path's envelope rather than against a literal, so the assertion
	// fails if either side changes alone.
	httpEnv := structuredAppendErrorResult("item decide", "append decision", refusal).
		StructuredContent.(ErrorEnvelope)
	if env.Error.Hint != httpEnv.Error.Hint {
		t.Errorf("stdio hint differs from the HTTP hint for the same condition:\n stdio: %q\n http:  %q",
			env.Error.Hint, httpEnv.Error.Hint)
	}
	if env.Error.Hint == "" {
		t.Error("hint is empty on both transports — the retry-hostile guidance is the point of the code")
	}

	// The human line must survive too — a marker-only stderr would leave a
	// person running the CLI staring at a JSON blob.
	human := stderr.String()
	human = human[strings.Index(human, "\n")+1:]
	if !strings.Contains(human, "overwrite and destroy") {
		t.Errorf("human-readable line missing from stderr; got: %q", stderr.String())
	}
}

// TestNoRemoteEquivalentDoesNotAdvertiseTheBrokenPRWorkaround — Codex round 5.
//
// noRemoteEquivalent's text IS the message a remote agent receives when it
// calls `github link`, which makes it the artifact that reaches the actor. It
// told agents to use `item update --field github_pr=...`; that write stores a
// string, so no link appears and the call reports success (BUG-2696).
//
// This is deliberately a NEGATIVE assertion on the prescription rather than a
// positive one on the wording: the point is that no future edit quietly
// reinstates the recommendation while the underlying write is still broken.
// Delete this test when BUG-2696 is fixed — at that point the advice becomes
// true and should come back.
func TestNoRemoteEquivalentDoesNotAdvertiseTheBrokenPRWorkaround(t *testing.T) {
	for _, cmd := range []string{"github link", "github unlink"} {
		hint, ok := noRemoteEquivalent[cmd]
		if !ok {
			t.Fatalf("%q is no longer in noRemoteEquivalent — if it gained a remote implementation, "+
				"revisit BUG-2696 and this test", cmd)
		}
		if !strings.Contains(hint, "BUG-2696") {
			t.Errorf("%q hint must name the bug that makes the field workaround useless; got: %s", cmd, hint)
		}
		// The failure this guards: prescribing the write without the caveat.
		lower := strings.ToLower(hint)
		if strings.Contains(lower, "github_pr") &&
			!strings.Contains(lower, "no working remote") && !strings.Contains(lower, "rather than clearing") {
			t.Errorf("%q hint mentions the github_pr field write without saying it does not work; got: %s", cmd, hint)
		}
	}
}

// TestReservedKeyRefusalsAgreeAcrossTransports — Codex round 7.
//
// Both reserved-key refusals must reach an agent as validation_failed no
// matter which dispatcher delivered them, because that is what the catalog and
// instructions.md tell agents to branch on. The move/copy refusal (BUG-2674)
// matched none of the stdio patterns and arrived as server_error: the same
// deterministic 400 wearing a transient-looking code on one transport only.
//
// Driven through the REAL classifiers with the REAL server message text, so a
// reworded refusal that stops matching fails here rather than in the field.
func TestReservedKeyRefusalsAgreeAcrossTransports(t *testing.T) {
	// Verbatim from handlers_items.go's move/copy override gate.
	const moveRefusal = "Error: Field(s) reserved for system metadata and not settable here: implementation_notes"
	// Verbatim shape from the update gate's message builder.
	const updateRefusal = `Error: "implementation_notes" is system metadata and cannot be set through a field update.`

	for _, tc := range []struct {
		name   string
		stderr string
		body   string
	}{
		{"move/copy override refusal (BUG-2674)", moveRefusal,
			`{"error":{"code":"malformed_override","message":"Field(s) reserved for system metadata and not settable here: implementation_notes"}}`},
		{"update field-patch refusal (BUG-2627)", updateRefusal,
			`{"error":{"code":"validation_error","message":"\"implementation_notes\" is system metadata and cannot be set through a field update."}}`},
		// Codex round 8: the COPY path words its refusal differently again,
		// and round 7's fix only covered the move wording. Same class, third
		// message.
		{"copy undeclared-override refusal", "Error: Destination collection has no field(s): github_pr",
			`{"error":{"code":"malformed_override","message":"Destination collection has no field(s): github_pr"}}`},
		// Control leg: a message the pattern list ALREADY covered. Without it
		// this table could pass by matching everything.
		{"copy invalid-override refusal (preflight)", `Error: Invalid override value(s): effort must be one of s, m, l`,
			`{"error":{"code":"invalid_override","message":"Invalid override value(s): effort must be one of s, m, l"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdio := classifyExecError(context.Background(), []string{"item", "move"}, errors.New("exit 1"), tc.stderr, nil)
			stdioEnv, ok := stdio.StructuredContent.(ErrorEnvelope)
			if !ok {
				t.Fatalf("stdio: expected ErrorEnvelope, got %T", stdio.StructuredContent)
			}

			remote := classifyHTTPStatus(context.Background(), "item move", 400, []byte(tc.body), nil)
			remoteEnv, ok := remote.StructuredContent.(ErrorEnvelope)
			if !ok {
				t.Fatalf("remote: expected ErrorEnvelope, got %T", remote.StructuredContent)
			}

			if remoteEnv.Error.Code != ErrValidationFailed {
				t.Errorf("remote code: got %q, want %q", remoteEnv.Error.Code, ErrValidationFailed)
			}
			if stdioEnv.Error.Code != remoteEnv.Error.Code {
				t.Errorf("the same refusal is classified differently per transport: stdio=%q remote=%q — "+
					"server_error reads as transient and invites a retry that always fails",
					stdioEnv.Error.Code, remoteEnv.Error.Code)
			}
		})
	}
}

// TestCLIAndMCPAgreeOnTheCodeString pins the two duplicated string constants
// together. They are deliberately not shared (internal/mcp does not import
// internal/cli in production code), so the only thing keeping them equal is
// this assertion.
func TestCLIAndMCPAgreeOnTheCodeString(t *testing.T) {
	if cli.StoredStateUnreadableCode != string(ErrStoredStateUnreadable) {
		t.Fatalf("cli.StoredStateUnreadableCode = %q, mcp.ErrStoredStateUnreadable = %q — "+
			"the stdio marker would be dropped as an unknown code",
			cli.StoredStateUnreadableCode, ErrStoredStateUnreadable)
	}
	if cli.StoredStateUnreadableHint != storedStateUnreadableHint {
		t.Errorf("the duplicated hint constants have drifted:\n cli: %q\n mcp: %q",
			cli.StoredStateUnreadableHint, storedStateUnreadableHint)
	}
	if _, allowed := allowedStructuredErrorCodes[string(ErrStoredStateUnreadable)]; !allowed {
		t.Fatal("stored_state_unreadable missing from allowedStructuredErrorCodes — " +
			"the stdio classifier drops unknown codes, so the marker would never surface")
	}
}
