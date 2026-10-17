package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// BUG-3864: the shared 403 gates name which gate refused, in details.reason,
// with code and message unchanged. cmd/pad's end-to-end test drives the role,
// token-scope and not-a-member gates through /mcp and the CLI; these are the
// gates a PAT fixture cannot reach (an OAuth consent allow-list) and the
// cross-workspace writer's two arms.

func bug3864Refusal(t *testing.T, rr *httptest.ResponseRecorder) (code, message, reason string) {
	t.Helper()
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403: %s", rr.Code, rr.Body.String())
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v: %s", err, rr.Body.String())
	}
	return env.Error.Code, env.Error.Message, env.Error.Details.Reason
}

func TestBUG3864_TheConsentAllowListNamesItself(t *testing.T) {
	srv := testServer(t)
	owner := mustCreateUser(t, srv, "owner@example.com", "Owner", "")
	ws := mustCreateOwnedWorkspace(t, srv, "Refused", owner)

	rr := httptest.NewRecorder()
	req := consentScopedRequest(http.MethodGet, "/api/v1/workspaces/"+ws.Slug, owner, []string{"some-other-workspace"}, map[string]string{"slug": ws.Slug})
	srv.RequireWorkspaceAccess(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("the allow-list gate let the request through")
	})).ServeHTTP(rr, req)

	code, message, reason := bug3864Refusal(t, rr)
	if code != "permission_denied" || message != "Token is not authorized for this workspace" || reason != forbiddenReasonWorkspaceNotAuthorized {
		t.Errorf("got %q %q reason %q", code, message, reason)
	}
}

func TestBUG3864_CrossWorkspaceRefusalsNameTheirGate(t *testing.T) {
	for _, tc := range []struct {
		access     CrossWorkspaceAccess
		code       string
		wantReason string
	}{
		{CrossWorkspaceAccess{Reason: CrossWorkspaceTokenNotAllowed}, "permission_denied", forbiddenReasonWorkspaceNotAuthorized},
		{CrossWorkspaceAccess{}, "forbidden", forbiddenReasonNoAccess},
	} {
		rr := httptest.NewRecorder()
		tc.access.WriteDenied(rr)
		code, _, reason := bug3864Refusal(t, rr)
		if code != tc.code || reason != tc.wantReason {
			t.Errorf("%+v: got %q reason %q, want %q reason %q", tc.access, code, reason, tc.code, tc.wantReason)
		}
	}
}

func TestBUG3864_RequireMinRoleSaysRole(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxWorkspaceRole, "viewer"))
	rr := httptest.NewRecorder()
	if requireMinRole(rr, req, "editor") {
		t.Fatal("a viewer passed requireMinRole(editor)")
	}
	code, message, reason := bug3864Refusal(t, rr)
	if code != "forbidden" || message != "Insufficient permissions" || reason != forbiddenReasonRole {
		t.Errorf("got %q %q reason %q", code, message, reason)
	}
}
