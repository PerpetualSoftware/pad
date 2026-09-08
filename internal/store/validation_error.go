package store

import (
	"errors"
	"fmt"
)

// ErrValidation reports a write the store refused because of what the CALLER
// asked for, as opposed to anything that went wrong inside the server.
//
// The distinction is the whole point of the type. A handler that cannot tell
// the two apart has one honest option — 500 with no text — and that is what
// three of the four collection doors did with the prefix-grammar refusal
// (BUG-2951): the store composed a message naming the rule and the example,
// and the caller was told "An internal error occurred", which reads as an
// outage rather than as "fix your input". An MCP agent is told worse than
// that: internal/mcp/errors.go classifies stdio failures by matching CLI
// stderr prose, the generic message matches none of its validation patterns,
// so the refusal arrives as the RETRYABLE server_error code and the correct
// agent response to it is to retry a call that can never succeed.
var ErrValidation = errors.New("store: validation")

// ValidationError carries the caller-facing reason a write was refused.
//
// Reason is the contract: a door renders THAT field, never Error(), because
// Error() carries the sentinel prefix and any wrapping a call path has added,
// and none of that is addressed to the caller. Same rule, and for the same
// reason, as InvalidDocumentTitleError's Reason (documents.go).
//
// Constructing this type is the per-site DECISION that a message is safe to
// show — it says "this text quotes the caller's own input and the rule it
// broke, and nothing about how the server is built". The alternative shape,
// where a door returns err.Error() for anything it does not recognise, makes
// that decision by default for every error any layer might add later, which
// is how internal detail escapes.
type ValidationError struct{ Reason string }

func (e *ValidationError) Error() string { return ErrValidation.Error() + ": " + e.Reason }

func (e *ValidationError) Unwrap() error { return ErrValidation }

// AsValidationError reports whether err is (or wraps) a ValidationError, and
// returns it. Doors use this instead of a bare errors.As so the Reason-not-
// Error() rule has one place to be stated.
func AsValidationError(err error) (*ValidationError, bool) {
	var v *ValidationError
	if errors.As(err, &v) {
		return v, true
	}
	return nil, false
}

// invalidf builds a ValidationError. Unexported: the store decides what is
// caller-visible, and a caller-supplied Reason from outside the package would
// be a text-injection channel into 4xx bodies.
func invalidf(format string, args ...any) error {
	return &ValidationError{Reason: fmt.Sprintf(format, args...)}
}
