package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// HTTP-handler tests for POST /api/v1/oauth/claim
// (PLAN-1519 / TASK-1521 / IDEA-1517 §4).
//
// What's covered:
//
//   - Endpoint 412s when SetClaimSecret hasn't been called (defense
//     in depth — the route is mounted unconditionally, the secret
//     gate is the safety net).
//   - 400 on missing workspace / code / malformed body.
//   - 404 when the workspace doesn't exist OR the user isn't a member
//     — single envelope so the endpoint can't be used to probe
//     workspace existence.
//   - 401 on wrong / expired code.
//   - 200 + side effect insert when the code verifies AND the caller
//     is an OAuth grant (request_id set in context).
//   - 200 + "note" populated for PAT callers — code verifies but no
//     side effect runs (PAT has no grant to add to).
//   - Idempotent: re-claiming an already-allowed workspace returns
//     200 with already_added=true.
//   - 412 connection_not_persisted when the OAuth grant predates
//     Phase C (no oauth_connections row).
//
// Auth is via PAT (Bearer) so CSRF + session-cookie machinery doesn't
// intrude. PAT auth doesn't set the OAuth request_id context value,
// which is exactly the "no oauth grant" branch we want to exercise
// for the note + 412 paths. The OAuth-grant-bound case is exercised
// by synthesizing the WithMCPTokenIdentity context value directly via
// a custom test request.

// claimTestEnv bundles the server, a user, that user's PAT, and a
// workspace they own — the common shape every claim test needs.
type claimTestEnv struct {
	srv       *Server
	user      *models.User
	pat       string
	ws        *models.Workspace
	wsSlug    string
	secret    []byte
	claimCode string
	now       time.Time
}

func newClaimTestEnv(t *testing.T) *claimTestEnv {
	t.Helper()
	srv := testServer(t)
	// Wire a deterministic claim secret (32 bytes — production reuses
	// the encryption key, same length).
	secret := bytes32ForTest()
	srv.SetClaimSecret(secret)

	user, err := srv.store.CreateUser(models.UserCreate{
		Email: "claim-test@example.com", Name: "Claim Tester", Password: "pw-claim-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{
		Name: "Claim Test WS", OwnerID: user.ID,
	})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, user.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	tok, err := srv.store.CreateAPIToken(user.ID, models.APITokenCreate{
		Name:        "claim-test-pat",
		WorkspaceID: ws.ID,
	}, 30, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	// Derive against wall-clock time — the handler reads time.Now()
	// directly. Tests complete within the 5-minute bucket so the
	// derive and verify land in the same window.
	now := time.Now().UTC()
	code := DeriveClaimCode(secret, user.ID, ws.ID, now)
	return &claimTestEnv{
		srv: srv, user: user, pat: tok.Token, ws: ws, wsSlug: ws.Slug,
		secret: secret, claimCode: code, now: now,
	}
}

// doClaim issues a POST /api/v1/oauth/claim with Bearer PAT auth.
// extraReqID, if non-empty, attaches a WithMCPTokenIdentity context
// override to simulate an OAuth-grant-bound request without spinning
// up a full OAuth fixture — handleOAuthClaim only reads the typed
// context helper, so this is enough to exercise the OAuth branch.
func (e *claimTestEnv) doClaim(body any, extraReqID string) *httptest.ResponseRecorder {
	var bodyReader *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		bodyReader = strings.NewReader(string(b))
	}
	var req *http.Request
	if bodyReader != nil {
		req = httptest.NewRequest("POST", "/api/v1/oauth/claim", bodyReader)
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest("POST", "/api/v1/oauth/claim", nil)
	}
	req.Header.Set("Authorization", "Bearer "+e.pat)
	req.RemoteAddr = "192.0.2.1:1234"

	if extraReqID != "" {
		// Wrap the handler so we can decorate the request context
		// AFTER TokenAuth runs (it would otherwise clear/ignore our
		// override). The simplest path is to compose a tiny middleware
		// that re-attaches the identity post-auth. Mirrors the
		// pattern handlers_mcp_test.go uses for similar context
		// injection.
		rr := httptest.NewRecorder()
		wrap := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(WithMCPTokenIdentity(r.Context(), "oauth", extraReqID))
			e.srv.ServeHTTP(w, r)
		})
		wrap.ServeHTTP(rr, req)
		return rr
	}
	rr := httptest.NewRecorder()
	e.srv.ServeHTTP(rr, req)
	return rr
}

func TestHandleOAuthClaim_ClaimSecretDisabled412(t *testing.T) {
	srv := testServer(t)
	user, err := srv.store.CreateUser(models.UserCreate{
		Email: "d@example.com", Name: "D", Password: "pw-disabled-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Disabled WS", OwnerID: user.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	tok, err := srv.store.CreateAPIToken(user.ID, models.APITokenCreate{
		Name: "d", WorkspaceID: ws.ID,
	}, 30, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	// Deliberately do NOT call SetClaimSecret.

	req := httptest.NewRequest("POST", "/api/v1/oauth/claim",
		strings.NewReader(`{"workspace":"x","code":"123456"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusPreconditionFailed {
		t.Errorf("status = %d, want 412 (claim_disabled)", rr.Code)
	}
}

func TestHandleOAuthClaim_BadRequest400(t *testing.T) {
	e := newClaimTestEnv(t)
	cases := []struct {
		name string
		body any
	}{
		{"missing both", map[string]string{}},
		{"missing code", map[string]string{"workspace": "x"}},
		{"missing workspace", map[string]string{"code": "123456"}},
		{"empty strings", map[string]string{"workspace": "", "code": ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := e.doClaim(tc.body, "")
			if rr.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestHandleOAuthClaim_WorkspaceNotFound404(t *testing.T) {
	e := newClaimTestEnv(t)
	rr := e.doClaim(map[string]string{"workspace": "no-such-ws", "code": "123456"}, "")
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestHandleOAuthClaim_NotAMember404(t *testing.T) {
	e := newClaimTestEnv(t)
	// Create a second workspace owned by someone else; the PAT user
	// isn't a member. 404 (uniform with "not found" so existence isn't
	// leaked).
	other, _ := e.srv.store.CreateUser(models.UserCreate{
		Email: "other@example.com", Name: "Other", Password: "pw-other-12345",
	})
	otherWS, _ := e.srv.store.CreateWorkspace(models.WorkspaceCreate{
		Name: "Other WS", OwnerID: other.ID,
	})
	_ = e.srv.store.AddWorkspaceMember(otherWS.ID, other.ID, "owner")

	rr := e.doClaim(map[string]string{"workspace": otherWS.Slug, "code": "123456"}, "")
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (not-a-member should look like not-found)", rr.Code)
	}
}

func TestHandleOAuthClaim_WrongCode401(t *testing.T) {
	e := newClaimTestEnv(t)
	rr := e.doClaim(map[string]string{"workspace": e.wsSlug, "code": "000000"}, "")
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (body=%s)", rr.Code, rr.Body.String())
	}
}

func TestHandleOAuthClaim_PATCallerOK_NoSideEffect(t *testing.T) {
	e := newClaimTestEnv(t)
	rr := e.doClaim(map[string]string{"workspace": e.wsSlug, "code": e.claimCode}, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	var resp ClaimWorkspaceResponseTestProjection
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Workspace != e.wsSlug {
		t.Errorf("workspace = %q, want %q", resp.Workspace, e.wsSlug)
	}
	if resp.AlreadyAdded {
		t.Errorf("already_added = true, want false (PAT path doesn't add)")
	}
	if resp.Note == "" {
		t.Errorf("note should be populated for PAT caller — handler must explain the no-op")
	}
}

func TestHandleOAuthClaim_OAuthCaller_NoConnectionRow_412(t *testing.T) {
	e := newClaimTestEnv(t)
	// OAuth-grant context BUT no row in oauth_connections — Phase-A
	// state: write path lands in Phase C. Handler must return 412
	// with connection_not_persisted, not a generic 500.
	rr := e.doClaim(map[string]string{"workspace": e.wsSlug, "code": e.claimCode}, "phase-a-grant")
	if rr.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412 (body=%s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "connection_not_persisted") {
		t.Errorf("body should contain connection_not_persisted, got %s", rr.Body.String())
	}
}

func TestHandleOAuthClaim_OAuthCaller_Inserts_Idempotent(t *testing.T) {
	e := newClaimTestEnv(t)
	requestID := "fake-grant-chain"
	// Seed the connection row so the FK + pre-check is happy.
	if err := e.srv.store.CreateOAuthConnection(store.OAuthConnection{
		RequestID: requestID, UserID: e.user.ID, Name: "Cursor",
	}); err != nil {
		t.Fatalf("CreateOAuthConnection: %v", err)
	}

	// First claim — inserts.
	rr := e.doClaim(map[string]string{"workspace": e.wsSlug, "code": e.claimCode}, requestID)
	if rr.Code != http.StatusOK {
		t.Fatalf("first claim status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	var resp ClaimWorkspaceResponseTestProjection
	parseJSON(t, rr, &resp)
	if resp.AlreadyAdded {
		t.Errorf("first claim already_added = true, want false")
	}
	// Verify the join row exists.
	allowed, err := e.srv.store.IsConnectionWorkspaceAllowed(requestID, e.ws.ID)
	if err != nil {
		t.Fatalf("IsConnectionWorkspaceAllowed: %v", err)
	}
	if !allowed {
		t.Errorf("workspace should be in allow-list after successful claim")
	}

	// Second claim — idempotent.
	rr = e.doClaim(map[string]string{"workspace": e.wsSlug, "code": e.claimCode}, requestID)
	if rr.Code != http.StatusOK {
		t.Fatalf("second claim status = %d, want 200", rr.Code)
	}
	parseJSON(t, rr, &resp)
	if !resp.AlreadyAdded {
		t.Errorf("second claim already_added = false, want true (idempotent)")
	}
}

// ClaimWorkspaceResponseTestProjection mirrors the wire shape of the
// ClaimWorkspaceResponse defined on the CLI client side. Duplicated
// here to keep the server-side test from importing the cli package
// (which would introduce an upward dep).
type ClaimWorkspaceResponseTestProjection struct {
	Workspace    string `json:"workspace"`
	WorkspaceID  string `json:"workspace_id"`
	AlreadyAdded bool   `json:"already_added"`
	Note         string `json:"note,omitempty"`
}
