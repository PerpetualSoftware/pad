package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2890: minting and rotating API tokens requires an INTERACTIVE
// SESSION. A PAT that can reach the create door mints further tokens with
// independent names and expiries, which survive the revocation of the
// token that made them — so revoking a leaked credential does not end the
// access it was used to establish.
//
// Dave ruled the shape on day 57: create and rotate require session auth
// and refuse a PAT-authenticated call with 403 `session_required`; list
// and revoke stay PAT-reachable, because neither extends access.
//
// The controls matter more than usual here. A PAT-refused leg alone is
// satisfied by a handler that refuses EVERYONE, and a gate written against
// "carries an Authorization header" rather than against "is a PAT" would
// pass every leg below except the CLI one — `padsess_` Bearer credentials
// are interactive sessions and must keep working.

type tokenAuthEnv struct {
	srv     *Server
	user    *models.User
	ws      *models.Workspace
	sessTok string // padsess_ — cookie AND CLI bearer
	patTok  string // pad_ — the credential under test
	patID   string
	spareID string // a second PAT, so revoke/rotate legs need not eat the one they authenticate with
}

func setupTokenAuthEnv(t *testing.T) *tokenAuthEnv {
	t.Helper()
	srv := testServer(t)

	user, err := srv.store.CreateUser(models.UserCreate{
		Email: "tokens@example.com", Name: "Token Owner",
		Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Tokens WS", OwnerID: user.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	// MEMBERSHIP IS LOAD-BEARING FOR THE WORKSPACE-DOOR LEG, not setup
	// boilerplate. CreateWorkspace records an owner_id; it does not add a
	// workspace_members row, and RequireWorkspaceAccess gates on
	// membership. Without this the workspace-door test gets its 403 from
	// "not a member" and discriminates nothing about the session gate —
	// measured, not assumed: the first run of this file failed on exactly
	// that, with `You are not a member of this workspace`.
	if err := srv.store.AddWorkspaceMember(ws.ID, user.ID, "owner"); err != nil {
		t.Fatalf("add owner as member: %v", err)
	}

	pat, err := srv.store.CreateAPIToken(user.ID, models.APITokenCreate{
		Name: "the-pat", WorkspaceID: ws.ID,
	}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	spare, err := srv.store.CreateAPIToken(user.ID, models.APITokenCreate{
		Name: "spare", WorkspaceID: ws.ID,
	}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken (spare): %v", err)
	}

	return &tokenAuthEnv{
		srv:     srv,
		user:    user,
		ws:      ws,
		sessTok: loginUser(t, srv, "tokens@example.com", "correct-horse-battery-staple"),
		patTok:  pat.Token,
		patID:   pat.ID,
		spareID: spare.ID,
	}
}

// THE RULE, create: a PAT cannot mint a token.
func TestCreateUserToken_PATIsRefused(t *testing.T) {
	t.Parallel()
	env := setupTokenAuthEnv(t)

	rr := doRequestWithBearer(env.srv, "POST", "/api/v1/auth/tokens", env.patTok,
		map[string]any{"name": "minted-by-a-pat"})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("PAT minted a token: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	if got := errorCode(t, rr); got != "session_required" {
		t.Errorf("error code = %v, want session_required; body=%s", got, rr.Body.String())
	}
}

// THE RULE, rotate: a PAT cannot rotate a token. Rotation re-issues a
// secret and can extend an expiry, so it establishes access exactly the
// way create does.
func TestRotateUserToken_PATIsRefused(t *testing.T) {
	t.Parallel()
	env := setupTokenAuthEnv(t)

	rr := doRequestWithBearer(env.srv, "POST", "/api/v1/auth/tokens/"+env.spareID+"/rotate", env.patTok, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("PAT rotated a token: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	if got := errorCode(t, rr); got != "session_required" {
		t.Errorf("error code = %v, want session_required; body=%s", got, rr.Body.String())
	}
}

// THE THIRD DOOR (population, not the two the filing named): the
// workspace-scoped mint at POST /workspaces/{ws}/tokens reaches the same
// store call with only requireMinRole("owner") in front of it, and a
// user-owned PAT held by an owner satisfies that.
func TestCreateWorkspaceToken_PATIsRefused(t *testing.T) {
	t.Parallel()
	env := setupTokenAuthEnv(t)

	rr := doRequestWithBearer(env.srv, "POST", "/api/v1/workspaces/"+env.ws.Slug+"/tokens", env.patTok,
		map[string]any{"name": "minted-by-a-pat-workspace-door"})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("PAT minted a workspace-scoped token: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	if got := errorCode(t, rr); got != "session_required" {
		t.Errorf("error code = %v, want session_required; body=%s", got, rr.Body.String())
	}
}

// CONTROL — the gate is on the CREDENTIAL, not on the door. A browser
// session still mints. Without this leg, a handler that refuses everyone
// passes every assertion above.
func TestCreateUserToken_SessionCookieStillWorks(t *testing.T) {
	t.Parallel()
	env := setupTokenAuthEnv(t)

	rr := doRequestWithCookie(env.srv, "POST", "/api/v1/auth/tokens",
		map[string]any{"name": "minted-by-a-session"}, env.sessTok)
	if rr.Code != http.StatusCreated {
		t.Fatalf("session could not mint a token: got %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
}

// CONTROL — and the sharpest one. A `padsess_` CLI bearer IS an
// interactive session: ctxValidatedSessionBearer, not ctxIsAPIToken. A
// gate written against "has an Authorization: Bearer header" would pass
// every other leg in this file and break every logged-in CLI.
func TestCreateUserToken_CLISessionBearerStillWorks(t *testing.T) {
	t.Parallel()
	env := setupTokenAuthEnv(t)

	rr := doRequestWithBearer(env.srv, "POST", "/api/v1/auth/tokens", env.sessTok,
		map[string]any{"name": "minted-by-a-cli-session"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("a CLI session bearer was refused: got %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
}

// CONTROL — list stays PAT-reachable. Reading which tokens exist extends
// no access, and gating it would break every agent that inventories its
// own credentials.
func TestListUserTokens_PATStillWorks(t *testing.T) {
	t.Parallel()
	env := setupTokenAuthEnv(t)

	rr := doRequestWithBearer(env.srv, "GET", "/api/v1/auth/tokens", env.patTok, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("PAT could not list tokens: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

// CONTROL — revoke stays PAT-reachable, and this is the load-bearing one:
// a compromised-credential response is REVOCATION, so gating it behind a
// session would make the incident path need a browser.
func TestDeleteUserToken_PATStillWorks(t *testing.T) {
	t.Parallel()
	env := setupTokenAuthEnv(t)

	rr := doRequestWithBearer(env.srv, "DELETE", "/api/v1/auth/tokens/"+env.spareID, env.patTok, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("PAT could not revoke a token: got %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}
