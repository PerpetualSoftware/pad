package mcp

// BUG-2675 — the retry-hostile classification for "the item's stored state is
// unreadable", on BOTH transports.
//
// Both halves are tested here on purpose. v0.16 shipped a fix that reached the
// remote transport only and v0.17 had to finish it a version later; the way
// that happens is a test file that proves the half its author was looking at.

import (
	"bytes"
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
