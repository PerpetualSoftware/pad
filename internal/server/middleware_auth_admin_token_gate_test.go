package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-1616 regression suite — verifies that an admin platform role does
// NOT silently grant cross-workspace access when the request is
// authenticated via an API token (PAT on /api/v1, PAT on /mcp, or OAuth
// bearer on /mcp). Session-cookie admin access — the canonical web UI /
// CLI session affordance — is unaffected.
//
// All four tests exercise RequireWorkspaceAccess end-to-end via
// srv.ServeHTTP. The hot endpoint is GET /api/v1/workspaces/{slug}/dashboard
// — it's behind RequireWorkspaceAccess + requireMinRole("viewer"), so the
// middleware decision is what we're measuring; the dashboard payload is
// irrelevant for these tests.

// Uses doRequestWithBearer from middleware_auth_public_paths_test.go —
// signature is (srv, method, path, bearer, body). We pass nil bodies.

// TestRequireWorkspaceAccess_AdminPAT_DeniedOnNonMemberWorkspace pins the
// BUG-1616 fix: an admin user holding a PAT cannot reach a workspace
// they are not a member of. Pre-fix this returned 200 because the admin
// bypass at middleware_auth.go:480 short-circuited to owner role without
// consulting workspace_members. Post-fix the token-auth signal
// (isAPITokenAuth(r)) suppresses the bypass and the regular membership
// check produces a 403.
func TestRequireWorkspaceAccess_AdminPAT_DeniedOnNonMemberWorkspace(t *testing.T) {
	srv := testServer(t)

	// One platform admin, plus a regular user who owns the workspace.
	// The admin is NOT a member of that workspace — that's the
	// invariant the bypass used to silently violate.
	admin, err := srv.store.CreateUser(models.UserCreate{
		Email: "admin@example.com", Name: "Admin", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "owner@example.com", Name: "Owner", Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Private", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add owner as member: %v", err)
	}

	// Issue an unscoped PAT for the admin user. Unscoped (no
	// WorkspaceID) is the user-owned-token shape that TokenAuth
	// resolves to the admin user via apiToken.UserID — exactly the
	// shape MCP also uses.
	// CreateAPIToken requires WorkspaceID (legacy NOT NULL column,
	// see migrations/011_api_tokens.sql). We satisfy the constraint
	// by reusing ws.ID — the legacy-workspace guard in
	// RequireWorkspaceAccess at middleware_auth.go:463 only fires
	// when currentUser is nil, and the user-owned token resolves to
	// the admin user via apiToken.UserID, so the column value is
	// effectively a no-op for routing. The interesting axis is
	// whether the admin's USER row gets owner access despite not
	// being in workspace_members.
	tok, err := srv.store.CreateAPIToken(admin.ID, models.APITokenCreate{
		Name: "admin-pat", WorkspaceID: ws.ID,
	}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	rr := doRequestWithBearer(srv, "GET", "/api/v1/workspaces/"+ws.Slug+"/dashboard", tok.Token, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("admin PAT against non-member workspace: got status %d, want 403 (BUG-1616 regression); body=%s",
			rr.Code, rr.Body.String())
	}
}

// TestRequireWorkspaceAccess_AdminPAT_DeniedEvenWithGuestGrants confirms
// the membership-only stance (policy decision #1 on BUG-1616): admin
// via token doesn't fall back to the guest-grants path either. Even
// when the admin holds a workspace-scoped grant on a collection, a
// token-borne request is denied with `not_a_member`.
//
// Rationale: token-borne admin identity should be strictly the
// workspaces the admin actually joined. A leaked admin token with a
// stray collection grant somewhere shouldn't reach that collection's
// workspace through the token surface.
func TestRequireWorkspaceAccess_AdminPAT_DeniedEvenWithGuestGrants(t *testing.T) {
	srv := testServer(t)

	admin, err := srv.store.CreateUser(models.UserCreate{
		Email: "admin@example.com", Name: "Admin", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "owner@example.com", Name: "Owner", Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Granted", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add owner as member: %v", err)
	}

	// Seed a collection on the target workspace (testServer doesn't
	// pre-seed any defaults) and grant the admin view access on it.
	// They are still NOT a workspace_members row — they're a guest.
	// The pre-BUG-1616 code path for token+admin would have already
	// 200'd via the bypass; with the fix the admin-token gate fires
	// BEFORE the guest-grants fallback gets a chance.
	coll, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Slug: "tasks", Prefix: "TASK",
	})
	if err != nil {
		t.Fatalf("create collection: %v", err)
	}
	if _, err := srv.store.CreateCollectionGrant(ws.ID, coll.ID, admin.ID, "view", owner.ID); err != nil {
		t.Fatalf("create grant: %v", err)
	}

	// Token's WorkspaceID column is satisfied by reusing ws.ID — the
	// legacy-workspace guard in RequireWorkspaceAccess doesn't fire
	// because the token carries a user_id (currentUser is non-nil),
	// so the column value is effectively a no-op for routing.
	tok, err := srv.store.CreateAPIToken(admin.ID, models.APITokenCreate{
		Name: "admin-pat", WorkspaceID: ws.ID,
	}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	rr := doRequestWithBearer(srv, "GET", "/api/v1/workspaces/"+ws.Slug+"/dashboard", tok.Token, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("admin PAT against grant-only workspace: got status %d, want 403 (membership-only stance); body=%s",
			rr.Code, rr.Body.String())
	}
}

// TestRequireWorkspaceAccess_AdminPAT_AllowedOnMemberWorkspace is the
// positive control: when the admin IS a member of the workspace, the
// token request succeeds. This catches an over-correction where the new
// gate accidentally denies admins even on workspaces they legitimately
// joined.
func TestRequireWorkspaceAccess_AdminPAT_AllowedOnMemberWorkspace(t *testing.T) {
	srv := testServer(t)

	admin, err := srv.store.CreateUser(models.UserCreate{
		Email: "admin@example.com", Name: "Admin", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Mine", OwnerID: admin.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, admin.ID, "owner"); err != nil {
		t.Fatalf("add admin as member: %v", err)
	}

	tok, err := srv.store.CreateAPIToken(admin.ID, models.APITokenCreate{
		Name: "admin-pat", WorkspaceID: ws.ID,
	}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	rr := doRequestWithBearer(srv, "GET", "/api/v1/workspaces/"+ws.Slug+"/dashboard", tok.Token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin PAT against own workspace: got status %d, want 200; body=%s",
			rr.Code, rr.Body.String())
	}
}

// TestRequireWorkspaceAccess_AdminCLISessionBearer_DeniedOnNonMember
// extends BUG-1616 to cover the CLI auth path. `pad auth login` mints a
// padsess_<hex> session token and the CLI sends it via
// Authorization: Bearer (NOT as a cookie) — TokenAuth's session-bearer
// branch (middleware_auth.go:92-132) resolves it to the user but does
// NOT set ctxIsAPIToken. The gate-by-Authorization-header dual check in
// isBearerAuth is what closes this hole: admin via CLI to a workspace
// they didn't join → 403.
//
// Reproduces the user-reported expansion: "admin really shouldn't be
// able to access workspaces it's not a member of via CLI either."
func TestRequireWorkspaceAccess_AdminCLISessionBearer_DeniedOnNonMember(t *testing.T) {
	srv := testServer(t)

	admin, err := srv.store.CreateUser(models.UserCreate{
		Email: "admin@example.com", Name: "Admin", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "owner@example.com", Name: "Owner", Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "CLIPrivate", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add owner as member: %v", err)
	}

	// Mint a session token (the padsess_<hex> shape `pad auth login`
	// produces). UA empty so the binding check is a no-op against the
	// bearer call below (which also doesn't set a UA).
	sessTok, err := srv.store.CreateSession(admin.ID, "cli-test", "192.0.2.1", "", webSessionTTL)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Deliver the session token via Authorization: Bearer — the exact
	// shape internal/cli/client.go sends. This is the CLI path the
	// user wants closed.
	rr := doRequestWithBearer(srv, "GET", "/api/v1/workspaces/"+ws.Slug+"/dashboard", sessTok, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("admin CLI session bearer against non-member workspace: got status %d, want 403; body=%s",
			rr.Code, rr.Body.String())
	}
}

// TestRequireWorkspaceAccess_AdminSession_StillBypassesNonMember pins the
// preserved web-UI affordance: when the admin authenticates with a
// session cookie (no API token), the platform-admin global bypass still
// fires and grants owner-role access to any workspace. This is the
// expected behavior for the /console/admin pages and the workspace
// switcher, and the BUG-1616 fix must NOT regress it.
func TestRequireWorkspaceAccess_AdminSession_StillBypassesNonMember(t *testing.T) {
	srv := testServer(t)

	admin, err := srv.store.CreateUser(models.UserCreate{
		Email: "admin@example.com", Name: "Admin", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "owner@example.com", Name: "Owner", Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "OtherWS", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add owner as member: %v", err)
	}

	// Session cookie (NOT a Bearer token) — exercises the SessionAuth
	// path which does NOT set ctxIsAPIToken, so the admin global bypass
	// in RequireWorkspaceAccess still fires. UA is empty to match the
	// empty UA that doRequestWithCookie produces (UAHash binding check
	// compares hashes — empty vs empty is fine).
	sessTok, err := srv.store.CreateSession(admin.ID, "web-test", "192.0.2.1", "", webSessionTTL)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	rr := doRequestWithCookie(srv, "GET", "/api/v1/workspaces/"+ws.Slug+"/dashboard", nil, sessTok)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin session against non-member workspace: got status %d, want 200 (session-auth admin bypass preserved); body=%s",
			rr.Code, rr.Body.String())
	}
}
