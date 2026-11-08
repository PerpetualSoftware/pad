package mcp

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestCommitmentErrorCodeParity(t *testing.T) {
	for _, tc := range []struct {
		code   string
		status int
	}{{"hard_deadline", 409}, {"deadline_owner_required", 403}, {"commitment_changed", 409}} {
		t.Run(tc.code, func(t *testing.T) {
			body := fmt.Sprintf(`{"error":{"code":%q,"message":"Refused","details":{"field":"deadline","code":"combination"}}}`, tc.code)
			remote := classifyHTTPStatus(context.Background(), "item remind edit", tc.status, []byte(body), nil)
			stdio := classifyExecError(context.Background(), []string{"item", "remind", "edit"}, errors.New("exit status 1"), structuredErrorMarker+" "+body+"\n", nil)
			for _, result := range []*CallToolResult{remote, stdio} {
				env, ok := result.StructuredContent.(ErrorEnvelope)
				if !ok || string(env.Error.Code) != tc.code {
					t.Fatalf("got %+v want %s", result, tc.code)
				}
			}
		})
	}
}
