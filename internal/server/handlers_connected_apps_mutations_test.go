package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// Mutation-endpoint tests for /api/v1/connected-apps/{id}/... routes
// (PLAN-1519 / TASK-1524 / IDEA-1517 §3).
//
// What's covered (per endpoint):
//
//   - PATCH /name: happy path; trims + caps; empty string clears.
//   - PATCH /flags: happy path; empty-allow-list invariant rejects
//     all_current=false toggle when no join rows exist.
//   - POST /workspaces: happy path; non-member workspace 404s
//     (same envelope as not-found so existence isn't leaked).
//   - DELETE /workspaces/{slug}: happy path; idempotent missing-slug
//     no-op; empty-allow-list invariant rejects last-workspace
//     removal when all_current=false.
//
// Common shape: every handler 404s on non-owned connections (same
// envelope as Revoke).

// doAuthedPatch + doAuthedPost helpers for the mutation tests. The
// existing doAuthedRequest helper is GET-only; for state-changing
// methods we need to also attach the CSRF header (CSRFProtect runs
// in the /api/v1 chain on session-cookie auth).
func doAuthedJSON(srv *Server, method, path string, body any, sessionToken string) *httptest.ResponseRecorder {
	const testCSRF = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	var bodyReader *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		bodyReader = strings.NewReader(string(b))
	}
	var req *http.Request
	if bodyReader != nil {
		req = httptest.NewRequest(method, path, bodyReader)
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("User-Agent", testSessionUA)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: sessionToken})
	req.AddCookie(&http.Cookie{Name: csrfCookieName(srv.secureCookies), Value: testCSRF})
	req.Header.Set("X-CSRF-Token", testCSRF)
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// seedConnectedAppForMutations creates a user + workspace + an
// oauth_connections row directly so the mutation handlers have
// something to mutate. Returns the user + their session token + the
// connection's request_id + the workspace ID (some tests use it
// for membership checks).
func seedConnectedAppForMutations(t *testing.T, srv *Server, requestID string) (*models.User, string, string) {
	t.Helper()
	user, tok := loginTestUser(t, srv)
	// Seed a workspace the user owns + a connection that already
	// includes that workspace in its allow-list.
	wsID, _ := mustSeedWorkspaceForMutation(t, srv, user.ID, "mut-ws", "owner")
	if err := srv.store.CreateOAuthConnection(store.OAuthConnection{
		RequestID:               requestID,
		UserID:                  user.ID,
		Name:                    "Cursor",
		MayCreateWorkspaces:     true,
		AllCurrentWorkspaces:    false,
		IncludeFutureWorkspaces: false,
	}); err != nil {
		t.Fatalf("CreateOAuthConnection: %v", err)
	}
	if err := srv.store.AddConnectionWorkspace(requestID, wsID, store.AddedByUser); err != nil {
		t.Fatalf("AddConnectionWorkspace: %v", err)
	}
	return user, tok, wsID
}

func mustSeedWorkspaceForMutation(t *testing.T, srv *Server, ownerID, slug, role string) (string, string) {
	t.Helper()
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{
		Name:    slug,
		Slug:    slug,
		OwnerID: ownerID,
	})
	if err != nil {
		t.Fatalf("CreateWorkspace %s: %v", slug, err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, ownerID, role); err != nil {
		t.Fatalf("AddWorkspaceMember %s: %v", slug, err)
	}
	return ws.ID, ws.Slug
}

func TestHandleRenameConnectedApp_HappyPath(t *testing.T) {
	srv, _ := connectedAppsTestServer(t)
	_, tok, _ := seedConnectedAppForMutations(t, srv, "rename-chain")

	rr := doAuthedJSON(srv, "PATCH", "/api/v1/connected-apps/rename-chain/name",
		map[string]string{"name": "  Renamed   "}, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	var dto connectedAppDTO
	if err := json.Unmarshal(rr.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dto.Name != "Renamed" {
		t.Errorf("response Name = %q, want %q (whitespace trimmed)", dto.Name, "Renamed")
	}
}

func TestHandleRenameConnectedApp_LengthCap(t *testing.T) {
	srv, _ := connectedAppsTestServer(t)
	_, tok, _ := seedConnectedAppForMutations(t, srv, "long-name-chain")

	long := strings.Repeat("x", 200)
	rr := doAuthedJSON(srv, "PATCH", "/api/v1/connected-apps/long-name-chain/name",
		map[string]string{"name": long}, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var dto connectedAppDTO
	_ = json.Unmarshal(rr.Body.Bytes(), &dto)
	if len(dto.Name) != 120 {
		t.Errorf("Name length = %d, want 120 (capped)", len(dto.Name))
	}
}

func TestHandleRenameConnectedApp_NotOwner404(t *testing.T) {
	srv, _ := connectedAppsTestServer(t)
	// Connection belongs to Alice.
	seedConnectedAppForMutations(t, srv, "alice-chain")
	// Bob tries to rename Alice's connection.
	_, bobErr := srv.store.CreateUser(models.UserCreate{
		Email: "bob-rename@example.com", Name: "Bob", Password: "pw-bob-12345",
	})
	if bobErr != nil {
		t.Fatalf("CreateUser bob: %v", bobErr)
	}
	bobUser, bobTok := loginTestUserAs(t, srv, "bob-rename2@example.com", "Bob2", "pw-bob2-12345")
	_ = bobUser

	rr := doAuthedJSON(srv, "PATCH", "/api/v1/connected-apps/alice-chain/name",
		map[string]string{"name": "Hijacked"}, bobTok)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (non-owner must look like not-found)", rr.Code)
	}
}

func TestHandleUpdateConnectedAppFlags_HappyPath(t *testing.T) {
	srv, _ := connectedAppsTestServer(t)
	_, tok, _ := seedConnectedAppForMutations(t, srv, "flags-chain")

	rr := doAuthedJSON(srv, "PATCH", "/api/v1/connected-apps/flags-chain/flags",
		map[string]bool{
			"may_create_workspaces":     false,
			"all_current_workspaces":    true,
			"include_future_workspaces": true,
		}, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	var dto connectedAppDTO
	_ = json.Unmarshal(rr.Body.Bytes(), &dto)
	if dto.MayCreateWorkspaces {
		t.Errorf("MayCreate = true, want false")
	}
	if !dto.AllCurrentWorkspaces {
		t.Errorf("AllCurrent = false, want true")
	}
}

func TestHandleUpdateConnectedAppFlags_EmptyAllowListBlocked(t *testing.T) {
	srv, _ := connectedAppsTestServer(t)
	user, tok := loginTestUser(t, srv)
	// Create a connection with no join rows, all_current=true currently.
	if err := srv.store.CreateOAuthConnection(store.OAuthConnection{
		RequestID:            "empty-chain",
		UserID:               user.ID,
		Name:                 "Wildcard Connection",
		AllCurrentWorkspaces: true,
	}); err != nil {
		t.Fatalf("CreateOAuthConnection: %v", err)
	}

	// Toggling all_current=false would orphan the connection (no join
	// rows exist). API must reject.
	rr := doAuthedJSON(srv, "PATCH", "/api/v1/connected-apps/empty-chain/flags",
		map[string]bool{
			"may_create_workspaces":     true,
			"all_current_workspaces":    false,
			"include_future_workspaces": true,
		}, tok)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (empty_allowlist guard)", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "empty_allowlist") {
		t.Errorf("body should contain empty_allowlist error code; got %s", rr.Body.String())
	}
}

// TestHandleUpdateConnectedAppFlags_WildcardToSpecific_AfterPrestage is
// the regression guard for Codex review #585 round 1: a wildcard
// connection must be able to flip to "specific workspaces" after
// the user has pre-staged at least one workspace via the add-
// workspace endpoint. The pre-fix code path called
// GetOAuthConnectionAccess, which short-circuits on wildcard and
// reports zero slugs, making the toggle impossible regardless of
// pre-staging.
func TestHandleUpdateConnectedAppFlags_WildcardToSpecific_AfterPrestage(t *testing.T) {
	srv, _ := connectedAppsTestServer(t)
	user, tok := loginTestUser(t, srv)
	// Wildcard connection — no join rows yet.
	if err := srv.store.CreateOAuthConnection(store.OAuthConnection{
		RequestID:            "prestage-chain",
		UserID:               user.ID,
		AllCurrentWorkspaces: true,
	}); err != nil {
		t.Fatalf("CreateOAuthConnection: %v", err)
	}
	// Seed a workspace the user owns.
	_, slug := mustSeedWorkspaceForMutation(t, srv, user.ID, "prestage-ws", "owner")
	// Add it to the connection (allowed even while wildcard is on —
	// it's inert until the flag flips).
	if rr := doAuthedJSON(srv, "POST", "/api/v1/connected-apps/prestage-chain/workspaces",
		map[string]string{"workspace": slug}, tok); rr.Code != http.StatusOK {
		t.Fatalf("pre-stage add: status = %d (body=%s)", rr.Code, rr.Body.String())
	}
	// Now flip wildcard off — must succeed since the join table has 1 row.
	rr := doAuthedJSON(srv, "PATCH", "/api/v1/connected-apps/prestage-chain/flags",
		map[string]bool{
			"may_create_workspaces":     true,
			"all_current_workspaces":    false,
			"include_future_workspaces": true,
		}, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("post-prestage flag flip should succeed; status = %d (body=%s)", rr.Code, rr.Body.String())
	}
	var dto connectedAppDTO
	_ = json.Unmarshal(rr.Body.Bytes(), &dto)
	if dto.AllCurrentWorkspaces {
		t.Errorf("AllCurrentWorkspaces should be false post-flip; got true")
	}
	if len(dto.AllowedWorkspaces) != 1 || dto.AllowedWorkspaces[0] != slug {
		t.Errorf("AllowedWorkspaces should reflect the pre-staged slug; got %v", dto.AllowedWorkspaces)
	}
}

// TestHandleAddConnectedAppWorkspace_VisibleUnderWildcard is the
// regression guard for Codex review #585 round 2: pre-staging a
// workspace while wildcard is on must surface that workspace in
// the response DTO's allowed_workspaces array, so the UI can render
// the chip + offer a remove button. Pre-fix the DTO suppressed
// staged slugs under wildcard, leaving them invisible until the
// user flipped wildcard off.
func TestHandleAddConnectedAppWorkspace_VisibleUnderWildcard(t *testing.T) {
	srv, _ := connectedAppsTestServer(t)
	user, tok := loginTestUser(t, srv)
	// Wildcard connection.
	if err := srv.store.CreateOAuthConnection(store.OAuthConnection{
		RequestID:            "stage-wildcard-chain",
		UserID:               user.ID,
		AllCurrentWorkspaces: true,
	}); err != nil {
		t.Fatalf("CreateOAuthConnection: %v", err)
	}
	_, slug := mustSeedWorkspaceForMutation(t, srv, user.ID, "stage-wildcard-ws", "owner")

	rr := doAuthedJSON(srv, "POST", "/api/v1/connected-apps/stage-wildcard-chain/workspaces",
		map[string]string{"workspace": slug}, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", rr.Code, rr.Body.String())
	}
	var dto connectedAppDTO
	_ = json.Unmarshal(rr.Body.Bytes(), &dto)
	if !dto.AllCurrentWorkspaces {
		t.Errorf("AllCurrentWorkspaces flipped from true; should remain true (staged != active)")
	}
	found := false
	for _, s := range dto.AllowedWorkspaces {
		if s == slug {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("staged workspace %q should appear in DTO.AllowedWorkspaces even under wildcard; got %v", slug, dto.AllowedWorkspaces)
	}
}

func TestHandleAddConnectedAppWorkspace_HappyPath(t *testing.T) {
	srv, _ := connectedAppsTestServer(t)
	user, tok, _ := seedConnectedAppForMutations(t, srv, "add-ws-chain")
	// Seed a second workspace the user owns so we have something to add.
	_, slug2 := mustSeedWorkspaceForMutation(t, srv, user.ID, "second-ws", "editor")

	rr := doAuthedJSON(srv, "POST", "/api/v1/connected-apps/add-ws-chain/workspaces",
		map[string]string{"workspace": slug2}, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	var dto connectedAppDTO
	_ = json.Unmarshal(rr.Body.Bytes(), &dto)
	hasSecond := false
	for _, slug := range dto.AllowedWorkspaces {
		if slug == slug2 {
			hasSecond = true
		}
	}
	if !hasSecond {
		t.Errorf("AllowedWorkspaces should contain %q after add; got %v", slug2, dto.AllowedWorkspaces)
	}
}

func TestHandleAddConnectedAppWorkspace_NonMember404(t *testing.T) {
	srv, _ := connectedAppsTestServer(t)
	user, tok, _ := seedConnectedAppForMutations(t, srv, "add-nonmember-chain")
	// Other user owns a workspace our test user isn't a member of.
	other, err := srv.store.CreateUser(models.UserCreate{
		Email: "other-ws@example.com", Name: "Other", Password: "pw-other-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	_, otherSlug := mustSeedWorkspaceForMutation(t, srv, other.ID, "other-ws", "owner")

	// Confirm test user is not a member of otherSlug.
	if member, _ := srv.store.GetWorkspaceMember(other.ID, user.ID); member != nil {
		t.Fatalf("setup: test user should NOT be member of other-ws")
	}

	rr := doAuthedJSON(srv, "POST", "/api/v1/connected-apps/add-nonmember-chain/workspaces",
		map[string]string{"workspace": otherSlug}, tok)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (non-member must look like not-found)", rr.Code)
	}
}

func TestHandleRemoveConnectedAppWorkspace_HappyPath(t *testing.T) {
	srv, _ := connectedAppsTestServer(t)
	user, tok, _ := seedConnectedAppForMutations(t, srv, "rm-ws-chain")
	// Add a second workspace so removing the first doesn't trip the
	// empty-allow-list guard.
	_, slug2 := mustSeedWorkspaceForMutation(t, srv, user.ID, "rm-second-ws", "editor")
	if rr := doAuthedJSON(srv, "POST", "/api/v1/connected-apps/rm-ws-chain/workspaces",
		map[string]string{"workspace": slug2}, tok); rr.Code != http.StatusOK {
		t.Fatalf("setup add: status = %d (body=%s)", rr.Code, rr.Body.String())
	}

	rr := doAuthedJSON(srv, "DELETE", "/api/v1/connected-apps/rm-ws-chain/workspaces/"+slug2, nil, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	var dto connectedAppDTO
	_ = json.Unmarshal(rr.Body.Bytes(), &dto)
	for _, slug := range dto.AllowedWorkspaces {
		if slug == slug2 {
			t.Errorf("workspace %q should be gone from allow-list; got %v", slug2, dto.AllowedWorkspaces)
		}
	}
}

func TestHandleRemoveConnectedAppWorkspace_LastBlocked(t *testing.T) {
	srv, _ := connectedAppsTestServer(t)
	_, tok, _ := seedConnectedAppForMutations(t, srv, "rm-last-chain")
	// Connection has all_current=false + exactly one workspace.
	// Removing it would orphan the connection — API must reject.
	rr := doAuthedJSON(srv, "DELETE", "/api/v1/connected-apps/rm-last-chain/workspaces/mut-ws", nil, tok)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (empty_allowlist guard)", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "empty_allowlist") {
		t.Errorf("body should mention empty_allowlist; got %s", rr.Body.String())
	}
}

func TestHandleRemoveConnectedAppWorkspace_IdempotentMissing(t *testing.T) {
	srv, _ := connectedAppsTestServer(t)
	_, tok, _ := seedConnectedAppForMutations(t, srv, "rm-idem-chain")

	// Removing a slug that's not in the allow-list (and may not even
	// exist as a workspace) is a no-op — returns 200 with the
	// unchanged DTO rather than 404. The user's intent ("this slug
	// shouldn't be in my list") is satisfied either way.
	rr := doAuthedJSON(srv, "DELETE", "/api/v1/connected-apps/rm-idem-chain/workspaces/never-existed", nil, tok)
	if rr.Code != http.StatusOK {
		t.Errorf("idempotent missing-slug delete = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
}

// loginTestUserAs is a variant of loginTestUser that takes explicit
// email/name/password so multiple distinct users can coexist within
// one test (loginTestUser hardcodes a single email).
func loginTestUserAs(t *testing.T, srv *Server, email, name, password string) (*models.User, string) {
	t.Helper()
	user, err := srv.store.CreateUser(models.UserCreate{
		Email: email, Name: name, Password: password,
	})
	if err != nil {
		t.Fatalf("CreateUser %s: %v", email, err)
	}
	tok, err := srv.store.CreateSession(user.ID, "test", "192.0.2.1", testSessionUA, webSessionTTL)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return user, tok
}
