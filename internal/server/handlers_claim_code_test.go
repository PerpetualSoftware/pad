package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// seedActiveAccessTokenForChain inserts one active access-token row
// in the chain identified by requestID for subject userID. Lets the
// IsWorkspaceCoveredForUser query find the chain as "active." The
// oauth_clients row needed for FK is created inline so the helper
// is self-contained.
//
// We can't reuse the store_test seedClient/seedAccess helpers from
// here because they live in package store; the server-package test
// would need them exposed. Inlining keeps the test isolation tight.
func seedActiveAccessTokenForChain(t *testing.T, s *store.Store, requestID, userID string) {
	t.Helper()
	client, err := s.CreateOAuthClient(models.OAuthClientCreate{
		Name:                    "Test Client " + requestID,
		RedirectURIs:            []string{"https://example.test/cb"},
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "none",
		Scopes:                  []string{"pad:read", "pad:write"},
		Public:                  true,
	})
	if err != nil {
		t.Fatalf("CreateOAuthClient: %v", err)
	}
	sigBytes := make([]byte, 16)
	if _, err := rand.Read(sigBytes); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if err := s.CreateAccessToken(models.OAuthRequest{
		Signature:     hex.EncodeToString(sigBytes),
		RequestID:     requestID,
		RequestedAt:   time.Now().UTC(),
		ClientID:      client.ID,
		GrantedScopes: "pad:read pad:write",
		Subject:       userID,
	}); err != nil {
		t.Fatalf("CreateAccessToken: %v", err)
	}
}

// HTTP-handler tests for GET /api/v1/workspaces/{slug}/claim-code
// (PLAN-1519 / TASK-1525 / IDEA-1517 §4).
//
// What's covered:
//
//   - 412 claim_disabled when SetClaimSecret hasn't been called.
//   - 404 (via RequireWorkspaceAccess) when the workspace doesn't
//     exist OR the user isn't a member — uniform envelope so the
//     endpoint can't be used to probe workspace existence.
//   - 200 with a 6-digit code matching DeriveClaimCode for an
//     authenticated member with no covering grant.
//   - 200 with suppressed=true (and NO code field) when the user
//     has an active OAuth connection that covers the workspace —
//     both wildcard (all_current_workspaces=1) and explicit
//     allow-list rows trigger suppression.
//   - 200 with suppressed=false for a revoked connection (no
//     active tokens) — the dangling oauth_connections row alone
//     does NOT suppress.
//
// Auth is PAT Bearer for parity with handlers_oauth_claim_test.go;
// the suppression query consults oauth_connections directly so it
// doesn't care which auth shape the caller used.

type claimCodeResponse struct {
	Workspace             string `json:"workspace"`
	Code                  string `json:"code,omitempty"`
	ExpiresAt             string `json:"expires_at"`
	Suppressed            bool   `json:"suppressed"`
	SuppressionGrantName  string `json:"suppression_grant_name,omitempty"`
}

func newClaimCodeEnv(t *testing.T) *claimTestEnv {
	t.Helper()
	// Reuse the OAuth-claim test fixture — same server, same user,
	// same workspace + PAT. The generation endpoint is workspace-
	// scoped under RequireWorkspaceAccess so we hit it via a slug
	// URL rather than the static /oauth/claim path.
	return newClaimTestEnv(t)
}

func (e *claimTestEnv) doClaimCodeGet() *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET",
		"/api/v1/workspaces/"+e.wsSlug+"/claim-code", nil)
	req.Header.Set("Authorization", "Bearer "+e.pat)
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	e.srv.ServeHTTP(rr, req)
	return rr
}

func TestHandleWorkspaceClaimCode_Disabled412(t *testing.T) {
	srv := testServer(t)
	user, err := srv.store.CreateUser(models.UserCreate{
		Email: "cc-disabled@example.com", Name: "D", Password: "pw-disabled-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "WS", OwnerID: user.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	_ = srv.store.AddWorkspaceMember(ws.ID, user.ID, "owner")
	tok, err := srv.store.CreateAPIToken(user.ID, models.APITokenCreate{
		Name: "d", WorkspaceID: ws.ID,
	}, 30, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	// Deliberately do NOT call SetClaimSecret.
	req := httptest.NewRequest("GET", "/api/v1/workspaces/"+ws.Slug+"/claim-code", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusPreconditionFailed {
		t.Errorf("status = %d, want 412 (claim_disabled), body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandleWorkspaceClaimCode_NotAMember404(t *testing.T) {
	e := newClaimCodeEnv(t)
	// Spin up a workspace the PAT user isn't a member of —
	// RequireWorkspaceAccess returns 404 (uniform with "not found").
	other, _ := e.srv.store.CreateUser(models.UserCreate{
		Email: "cc-other@example.com", Name: "Other", Password: "pw-other-12345",
	})
	otherWS, _ := e.srv.store.CreateWorkspace(models.WorkspaceCreate{
		Name: "Other WS", OwnerID: other.ID,
	})
	_ = e.srv.store.AddWorkspaceMember(otherWS.ID, other.ID, "owner")

	req := httptest.NewRequest("GET",
		"/api/v1/workspaces/"+otherWS.Slug+"/claim-code", nil)
	req.Header.Set("Authorization", "Bearer "+e.pat)
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	e.srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404, body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandleWorkspaceClaimCode_GrantOnlyGuest_403(t *testing.T) {
	e := newClaimCodeEnv(t)
	// RequireWorkspaceAccess admits grant-only guests (item-scoped
	// access without workspace membership). Claim-code redemption
	// requires membership; so generation must also require it, or a
	// guest would walk away with a code the claim endpoint will
	// always reject. Forge a guest scenario by creating a second user
	// who is NOT a member of e.ws and granting them an item-level
	// access; we approximate the post-RequireWorkspaceAccess state by
	// not adding them as a member at all and letting the user-only
	// auth path reach the handler. (RequireWorkspaceAccess's guest
	// admission path is the production case; this test only needs
	// the "user authenticated but not a member" branch to fire.)
	guest, err := e.srv.store.CreateUser(models.UserCreate{
		Email: "cc-guest@example.com", Name: "Guest", Password: "pw-guest-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	guestPAT, err := e.srv.store.CreateAPIToken(guest.ID, models.APITokenCreate{
		Name: "guest", WorkspaceID: e.ws.ID,
	}, 30, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	req := httptest.NewRequest("GET",
		"/api/v1/workspaces/"+e.wsSlug+"/claim-code", nil)
	req.Header.Set("Authorization", "Bearer "+guestPAT.Token)
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	e.srv.ServeHTTP(rr, req)
	// RequireWorkspaceAccess will 404 anyone without an item grant +
	// not a member — so the handler-level check fires only when the
	// outer gate admits a grant guest. Either 403 (the handler
	// fired) or 404 (the middleware caught the non-member first) is
	// an acceptable closed-door response. The point is that the
	// guest must NOT see a 200 + claim code.
	if rr.Code == http.StatusOK {
		t.Errorf("status = 200, want a closed-door response (403 from handler or 404 from middleware). body=%s", rr.Body.String())
	}
}

func TestHandleWorkspaceClaimCode_OK_FreshCode(t *testing.T) {
	e := newClaimCodeEnv(t)
	rr := e.doClaimCodeGet()
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rr.Code, rr.Body.String())
	}
	var resp claimCodeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Workspace != e.wsSlug {
		t.Errorf("workspace = %q, want %q", resp.Workspace, e.wsSlug)
	}
	if resp.Suppressed {
		t.Errorf("suppressed = true, want false (no covering grant exists)")
	}
	if len(resp.Code) != 6 {
		t.Errorf("code length = %d, want 6 (code=%q)", len(resp.Code), resp.Code)
	}
	// Verify the response matches DeriveClaimCode for the current
	// bucket — guards against the handler accidentally using a
	// different bucket or secret than the verifier.
	want := DeriveClaimCode(e.secret, e.user.ID, e.ws.ID, time.Now())
	if resp.Code != want {
		t.Errorf("code = %q, want %q (handler should match DeriveClaimCode)", resp.Code, want)
	}
	if resp.ExpiresAt == "" {
		t.Errorf("expires_at should be set for non-suppressed response")
	}
	if _, err := time.Parse(time.RFC3339, resp.ExpiresAt); err != nil {
		t.Errorf("expires_at = %q, not RFC3339: %v", resp.ExpiresAt, err)
	}
}

func TestHandleWorkspaceClaimCode_Suppressed_Wildcard(t *testing.T) {
	e := newClaimCodeEnv(t)
	// Seed a connection with all_current_workspaces=1 and an active
	// access token in the chain. Coverage should suppress.
	requestID := "fake-grant-wildcard"
	if err := e.srv.store.CreateOAuthConnection(store.OAuthConnection{
		RequestID: requestID, UserID: e.user.ID, Name: "Cursor",
		AllCurrentWorkspaces: true,
	}); err != nil {
		t.Fatalf("CreateOAuthConnection: %v", err)
	}
	seedActiveAccessTokenForChain(t, e.srv.store, requestID, e.user.ID)

	rr := e.doClaimCodeGet()
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rr.Code, rr.Body.String())
	}
	var resp claimCodeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Suppressed {
		t.Errorf("suppressed = false, want true (workspace covered by wildcard grant)")
	}
	if resp.Code != "" {
		t.Errorf("code = %q, want empty under suppression (no code to offer)", resp.Code)
	}
	if resp.SuppressionGrantName != "Cursor" {
		t.Errorf("suppression_grant_name = %q, want %q", resp.SuppressionGrantName, "Cursor")
	}
}

func TestHandleWorkspaceClaimCode_Suppressed_ExplicitAllowlistRow(t *testing.T) {
	e := newClaimCodeEnv(t)
	// Specific-allow-list path: not wildcard, but an explicit row in
	// oauth_connection_workspaces for this workspace.
	requestID := "fake-grant-specific"
	if err := e.srv.store.CreateOAuthConnection(store.OAuthConnection{
		RequestID: requestID, UserID: e.user.ID, Name: "Claude Code",
		AllCurrentWorkspaces: false,
	}); err != nil {
		t.Fatalf("CreateOAuthConnection: %v", err)
	}
	seedActiveAccessTokenForChain(t, e.srv.store, requestID, e.user.ID)
	if err := e.srv.store.AddConnectionWorkspace(requestID, e.ws.ID, store.AddedByUser); err != nil {
		t.Fatalf("AddConnectionWorkspace: %v", err)
	}

	rr := e.doClaimCodeGet()
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rr.Code, rr.Body.String())
	}
	var resp claimCodeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Suppressed {
		t.Errorf("suppressed = false, want true (workspace in explicit allow-list)")
	}
	if resp.SuppressionGrantName != "Claude Code" {
		t.Errorf("suppression_grant_name = %q, want %q", resp.SuppressionGrantName, "Claude Code")
	}
}

func TestHandleWorkspaceClaimCode_Suppressed_RevokedConnection_NotSuppressed(t *testing.T) {
	e := newClaimCodeEnv(t)
	// Connection row exists but no ACTIVE tokens in the chain — the
	// "active" predicate in IsWorkspaceCoveredForUser must filter
	// this out so a revoked grant doesn't suppress a fresh modal.
	requestID := "fake-grant-revoked"
	if err := e.srv.store.CreateOAuthConnection(store.OAuthConnection{
		RequestID: requestID, UserID: e.user.ID, Name: "Stale",
		AllCurrentWorkspaces: true,
	}); err != nil {
		t.Fatalf("CreateOAuthConnection: %v", err)
	}
	// No active token rows seeded.

	rr := e.doClaimCodeGet()
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rr.Code, rr.Body.String())
	}
	var resp claimCodeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Suppressed {
		t.Errorf("suppressed = true, want false (no ACTIVE tokens in chain → not covered)")
	}
	if resp.Code == "" {
		t.Errorf("code should be present when not suppressed")
	}
}
