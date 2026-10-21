package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// tokenPin is the workspace a USER-owned API token was minted for (BUG-3982).
// A token with a pin reaches that workspace and nothing else: not another
// workspace its user belongs to, and not the routes that span workspaces.
//
// ID is always set. Slug is empty when the pinned workspace no longer exists
// (deleted or purged); the token then reaches no workspace at all.
type tokenPin struct {
	ID   string
	Slug string
}

// tokenPinFrom returns the request's token pin, or nil when the credential is
// not a workspace-pinned user token (a cookie or CLI session, an OAuth token,
// an unpinned PAT, a legacy user-less workspace token).
func tokenPinFrom(ctx context.Context) *tokenPin {
	p, _ := ctx.Value(ctxTokenPin).(*tokenPin)
	return p
}

// withTokenPin confines ctx to the workspace a user-owned token is pinned to.
// It records the pin, and sets the one-slug allow-list [workspace slug] that
// every workspace-global door already honours for OAuth consent scoping
// (search fan-out, GET /workspaces, backlinks, MCP lister and resources, item
// copy), so those doors need no second mechanism. A pin whose workspace is
// gone sets the EMPTY allow-list, which fails closed.
//
// A token with no user (legacy workspace token) or no workspace_id is
// returned unchanged: legacy tokens have their own confinement (BUG-3353).
func (s *Server) withTokenPin(ctx context.Context, t *models.APIToken) (context.Context, error) {
	if t == nil || t.UserID == "" || t.WorkspaceID == "" {
		return ctx, nil
	}
	ws, err := s.store.GetWorkspaceByID(t.WorkspaceID)
	if err != nil {
		return ctx, err
	}
	if ws == nil {
		ctx = context.WithValue(ctx, ctxTokenPin, &tokenPin{ID: t.WorkspaceID})
		return WithTokenAllowedWorkspaces(ctx, []string{}), nil
	}
	ctx = context.WithValue(ctx, ctxTokenPin, &tokenPin{ID: ws.ID, Slug: ws.Slug})
	return WithTokenAllowedWorkspaces(ctx, []string{ws.Slug}), nil
}

// pinAdmitsWorkspaceRef reports whether a pinned token may address the
// workspace named ref (a slug or an ID, as a request carries it), decided
// BEFORE ref is resolved (BUG-3982 r4): a slug that does not exist, one the
// user is not a member of, and one the user is a member of all get the same
// refusal, so the pin answers nothing about existence or membership. A pin
// whose workspace is gone admits nothing.
func pinAdmitsWorkspaceRef(pin *tokenPin, ref string) bool {
	return pin.Slug != "" && (ref == pin.Slug || ref == pin.ID)
}

// signedInFor is currentUser for a public route that reads the caller about a
// resource in workspaceID (a share link, an invitation). A token pinned to
// another workspace is anonymous there (BUG-3982 r4), so its user's identity
// unlocks nothing and says nothing outside the pin.
func signedInFor(r *http.Request, workspaceID string) *models.User {
	if pin := tokenPinFrom(r.Context()); pin != nil && pin.ID != workspaceID {
		return nil
	}
	return currentUser(r)
}

// pinnedTokenExtraRoutes are the non-workspace routes a pinned token may use
// beyond what a legacy workspace token may (BUG-3982): the account doors that
// show who the token is and let it list and revoke the tokens pinned to its
// own workspace (revoking is the incident path; the handlers narrow both to
// that set), the pre-auth flows a stale bearer must not break, and
// the caller's own to-do feed, which already filters to the pinned workspace.
func pinnedTokenExtraRoute(method, path string) bool {
	switch path {
	case "/api/v1/me/needs",
		"/api/v1/auth/session",
		"/api/v1/auth/me",
		"/api/v1/auth/tokens":
		return method == http.MethodGet
	case "/api/v1/auth/logout",
		"/api/v1/auth/login",
		"/api/v1/auth/register",
		"/api/v1/auth/bootstrap",
		"/api/v1/auth/forgot-password",
		"/api/v1/auth/reset-password",
		"/api/v1/auth/local-reset",
		"/api/v1/auth/verify-email",
		"/api/v1/auth/resend-verification",
		"/api/v1/auth/2fa/login-verify",
		"/api/v1/auth/cli/sessions":
		return method == http.MethodPost
	case "/api/v1/auth/check-username":
		return method == http.MethodGet
	}
	if id, ok := strings.CutPrefix(path, "/api/v1/auth/tokens/"); ok {
		return method == http.MethodDelete && id != "" && !strings.Contains(id, "/")
	}
	if code, ok := strings.CutPrefix(path, "/api/v1/auth/cli/sessions/"); ok {
		return method == http.MethodGet && code != "" && !strings.Contains(code, "/")
	}
	return false
}

// pinnedTokenRouteAllowed reports whether a workspace-pinned user token may
// use r's route (BUG-3982). A route is allowed when it is workspace-scoped (so
// RequireWorkspaceAccess confines it to the pin), when a legacy workspace
// token may use it for the same reason (legacyTokenRouteAllowed), when it is
// one of pinnedTokenExtraRoute, or when it is public and not under /auth/.
// Everything else is refused, so a route added later is closed to a pinned
// token until it is listed here.
func pinnedTokenRouteAllowed(r *http.Request) bool {
	path := r.URL.Path
	if legacyTokenRouteAllowed(r) {
		return true
	}
	if pinnedTokenExtraRoute(r.Method, path) {
		return true
	}
	return isPublicAPIPath(path) && !strings.HasPrefix(path, "/api/v1/auth/")
}

// tokenWorkspaceDenial describes why a token is refused a workspace: the
// message to say and the details to attach. details is nil for a token with no
// pin, which keeps the OAuth allow-list answer it always had.
func tokenWorkspaceDenial(pin *tokenPin) (message string, details map[string]interface{}) {
	if pin == nil {
		return "Token is not authorized for this workspace", nil
	}
	const mint = "To use other workspaces, create a token without a workspace: " +
		"pad token create --name <name>, or Account settings \u2192 API Tokens."
	if pin.Slug == "" {
		return "This API token is limited to a workspace that no longer exists. " + mint,
			map[string]interface{}{"reason": forbiddenReasonTokenWorkspacePinned}
	}
	return fmt.Sprintf("This API token is limited to workspace %q. ", pin.Slug) + mint,
		map[string]interface{}{"reason": forbiddenReasonTokenWorkspacePinned, "token_workspace": pin.Slug}
}

// writeTokenWorkspaceDeniedFor writes the 403 for a token refused a workspace.
// A pinned token gets the pin reason, naming its workspace and how to mint an
// unscoped token (BUG-3982); any other token keeps the OAuth allow-list
// answer it always had.
func writeTokenWorkspaceDeniedFor(w http.ResponseWriter, pin *tokenPin) {
	msg, details := tokenWorkspaceDenial(pin)
	if details != nil {
		writeError2(w, http.StatusForbidden, "permission_denied", msg, details)
		return
	}
	writeForbidden(w, "permission_denied", msg, forbiddenReasonWorkspaceNotAuthorized)
}

// writeTokenWorkspaceDenied is writeTokenWorkspaceDeniedFor for the request's
// own token.
func writeTokenWorkspaceDenied(w http.ResponseWriter, r *http.Request) {
	writeTokenWorkspaceDeniedFor(w, tokenPinFrom(r.Context()))
}
