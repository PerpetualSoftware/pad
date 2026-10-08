package mcp

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// BUG-3480: an item list filter on a key no schema declares (and no item
// stores) is now a 400. Stdio classifies the CLI's stderr prose, so the
// refusal must read as validation there too, or an agent is told to retry a
// deterministic refusal. The server's messages start "invalid list filter";
// internal/server's TestBUG3480_ListFilterKeys pins that wording.
func TestBUG3480_ListFilterRefusalClassifiesAsValidation(t *testing.T) {
	cases := []struct{ stderr, body string }{
		{
			`Error: invalid list filter "all": it is not a list parameter, collection "conventions" declares no such field, and no item stores one`,
			`{"error":{"code":"validation_error","message":"invalid list filter \"all\": it is not a list parameter, collection \"conventions\" declares no such field, and no item stores one"}}`,
		},
		{
			`Error: invalid list filter "bad.key": a field key may hold only letters, digits, '_' and '-'`,
			`{"error":{"code":"validation_error","message":"invalid list filter \"bad.key\": a field key may hold only letters, digits, '_' and '-'"}}`,
		},
	}
	for _, c := range cases {
		stdio := classifyExecError(context.Background(), []string{"item", "list"}, errors.New("exit 1"), c.stderr, nil)
		if env, ok := stdio.StructuredContent.(ErrorEnvelope); !ok || env.Error.Code != ErrValidationFailed {
			t.Errorf("stdio %q: got %+v, want %q", c.stderr, stdio.StructuredContent, ErrValidationFailed)
		}
		remote := classifyHTTPStatus(context.Background(), "item list", http.StatusBadRequest, []byte(c.body), nil)
		if env, ok := remote.StructuredContent.(ErrorEnvelope); !ok || env.Error.Code != ErrValidationFailed {
			t.Errorf("remote %q: got %+v, want %q", c.body, remote.StructuredContent, ErrValidationFailed)
		}
	}

	// Control: the wording first drafted for this refusal matches no stdio
	// pattern and lands as the retryable server_error, which is why the
	// messages lead with "invalid".
	const drafted = `Error: unknown list filter "all": it is not a list parameter, collection "conventions" declares no such field, and no item stores one`
	stdio := classifyExecError(context.Background(), []string{"item", "list"}, errors.New("exit 1"), drafted, nil)
	if env, ok := stdio.StructuredContent.(ErrorEnvelope); !ok || env.Error.Code != ErrServerError {
		t.Errorf("control: got %+v, want %q", stdio.StructuredContent, ErrServerError)
	}
}
