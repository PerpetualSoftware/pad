package mcp

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// BUG-2951's load-bearing argument, measured rather than asserted.
//
// The stdio transport classifies a CLI failure by matching stderr PROSE. While
// the collection doors answered "An internal error occurred" for a refused
// prefix, that text matched none of the validation patterns and the refusal
// reached an agent as server_error — the RETRYABLE code — so the correct agent
// response to a permanently-invalid prefix was to retry it forever. That is
// worse than a lost message: it is a wrong instruction.
//
// Nothing in internal/mcp changed for this fix. What changed is that the
// store's own message now reaches stderr, and the EXISTING `invalid` pattern
// recognises it. This test is what keeps that true — a reworded refusal that
// drops the word the classifier matches would silently restore the retry loop,
// and it would do so with every server-side test still green.
func TestCollectionPrefixRefusalClassifiesAsValidation(t *testing.T) {
	// Verbatim shape of what the CLI prints now: "Error: " plus the API's
	// message, which is store.ValidationError.Reason.
	const fixedStderr = `Error: invalid prefix "ab1": a collection prefix must start with an uppercase letter and contain only uppercase letters or digits (e.g. TASK, AB1)`
	const fixedBody = `{"error":{"code":"bad_request","message":"invalid prefix \"ab1\": a collection prefix must start with an uppercase letter and contain only uppercase letters or digits (e.g. TASK, AB1)"}}`

	stdio := classifyExecError(context.Background(), []string{"collection", "update"}, errors.New("exit 1"), fixedStderr, nil)
	stdioEnv, ok := stdio.StructuredContent.(ErrorEnvelope)
	if !ok {
		t.Fatalf("stdio: expected ErrorEnvelope, got %T", stdio.StructuredContent)
	}
	if stdioEnv.Error.Code != ErrValidationFailed {
		t.Errorf("stdio code = %q, want %q — an unmatched refusal lands as server_error and invites an endless retry",
			stdioEnv.Error.Code, ErrValidationFailed)
	}

	remote := classifyHTTPStatus(context.Background(), "collection update", http.StatusBadRequest, []byte(fixedBody), nil)
	remoteEnv, ok := remote.StructuredContent.(ErrorEnvelope)
	if !ok {
		t.Fatalf("remote: expected ErrorEnvelope, got %T", remote.StructuredContent)
	}
	if remoteEnv.Error.Code != ErrValidationFailed {
		t.Errorf("remote code = %q, want %q", remoteEnv.Error.Code, ErrValidationFailed)
	}
	if stdioEnv.Error.Code != remoteEnv.Error.Code {
		t.Errorf("one refusal, two codes: stdio=%q remote=%q", stdioEnv.Error.Code, remoteEnv.Error.Code)
	}
}

// TestGenericInternalErrorStillClassifiesAsServerError is the negative control
// for the test above, and the measurement of the defect itself.
//
// Without it, the assertion "the fix changes what an agent is told" rests on
// the claim that the OLD text classified differently — a claim about a
// codepath, not an observation of one. This drives the same classifier with
// the exact string the doors used to emit. It must remain server_error: that
// is correct for a genuine internal failure, and it is precisely why the
// refusal had to stop wearing that message rather than the classifier being
// taught to read it as validation.
func TestGenericInternalErrorStillClassifiesAsServerError(t *testing.T) {
	const genericStderr = "Error: An internal error occurred"

	stdio := classifyExecError(context.Background(), []string{"collection", "update"}, errors.New("exit 1"), genericStderr, nil)
	env, ok := stdio.StructuredContent.(ErrorEnvelope)
	if !ok {
		t.Fatalf("stdio: expected ErrorEnvelope, got %T", stdio.StructuredContent)
	}
	if env.Error.Code != ErrServerError {
		t.Errorf("code = %q, want %q — this string is what a REAL internal failure says, and a retryable "+
			"classification is right for it", env.Error.Code, ErrServerError)
	}
}
