package mcp

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// BUG-4811: workspace create refuses an unknown template with a 400 before
// the mint. Stdio classifies the CLI's stderr prose, so the message must
// read as validation there too, the code the HTTP 400 maps to; otherwise an
// agent is told to retry a typo. internal/server's
// TestBUG4811_UnknownTemplateIs400BeforeTheMint pins the wording.
func TestBUG4811_UnknownTemplateClassifiesAsValidation(t *testing.T) {
	const msg = `invalid template "software": must be one of startup, scrum, product`
	stdio := classifyExecError(context.Background(), []string{"workspace", "create"}, errors.New("exit 1"), "Error: "+msg, nil)
	if env, ok := stdio.StructuredContent.(ErrorEnvelope); !ok || env.Error.Code != ErrValidationFailed {
		t.Errorf("stdio: got %+v, want %q", stdio.StructuredContent, ErrValidationFailed)
	}
	body := `{"error":{"code":"bad_request","message":"invalid template \"software\": must be one of startup, scrum, product"}}`
	remote := classifyHTTPStatus(context.Background(), "workspace create", http.StatusBadRequest, []byte(body), nil)
	if env, ok := remote.StructuredContent.(ErrorEnvelope); !ok || env.Error.Code != ErrValidationFailed {
		t.Errorf("remote: got %+v, want %q", remote.StructuredContent, ErrValidationFailed)
	}
}
