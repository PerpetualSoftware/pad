package server

import (
	"net/http"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// emailUnverifiedBlocked is the central predicate for PLAN-1933 DR-4's
// email-verification gate. It reports whether a request carrying `user`
// must be rejected because this is a Pad Cloud instance and the
// authenticated user has NOT verified their email address.
//
// Two short-circuits keep the gate a no-op everywhere it shouldn't fire:
//
//   - Self-hosted (`!s.cloudMode`) always returns false. Mandatory
//     email verification is a cloud-only feature (DR-9), so the gate is
//     entirely inert on self-hosted binaries — the middleware, the
//     collab upgrade check, the OAuth-provider checks, and the MCP
//     write gate all collapse to pass-through.
//   - An unauthenticated request (user == nil) returns false. The gate
//     only constrains what a logged-in-but-unverified user may do; it
//     never turns an anonymous request into a 403 (RequireAuth already
//     owns the "must be logged in" decision). Keying on user != nil is
//     also what auto-skips the unauthenticated auth routes — login,
//     register, forgot/reset-password, bootstrap, CLI-session creation —
//     so no fragile URL allowlist is needed for those (DR-4 core rule).
//     It is also why legacy workspace-scoped API tokens with no
//     currentUser (middleware_auth.go) are intentionally NOT gated:
//     currentUser==nil → false. That's acceptable per DR-4 (pre-existing
//     token owners are backfilled verified; unverified users cannot mint
//     tokens because token-create is blocked below; MCP PAT auth already
//     rejects legacy no-user workspace tokens in middleware_mcp_auth.go).
//
// Shared by the RequireVerifiedEmail middleware (the /api/v1 method
// gate), the collab WebSocket upgrade check (a GET the method gate
// misses), and the OAuth-provider authorize/decide handlers (mounted
// outside /api/v1). The remote MCP write path uses an equivalent inline
// predicate wired onto the dispatcher in cmd/pad/main.go.
func (s *Server) emailUnverifiedBlocked(user *models.User) bool {
	return s.cloudMode && user != nil && !user.IsEmailVerified()
}

// isMutatingMethod reports whether an HTTP method can mutate server
// state. The verified-email gate only fires on these — reads (GET /
// HEAD / OPTIONS) stay open for unverified users so they can still
// browse, poll their session, and see the "verify your email" banner.
func isMutatingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
		return true
	default:
		return false
	}
}

// verifiedEmailExemptPath reports whether a mutating /api/v1 request
// stays allowed for an authenticated-but-unverified cloud user. These
// are the narrow, deliberate carve-outs from DR-4:
//
//   - POST /api/v1/invitations/{code}/accept — the hard carve-out. It
//     lives OUTSIDE /auth/, and blocking it would strand an unverified
//     invitee (they could never join a workspace). Per DR-1, accepting
//     an email-bound invitation VERIFIES the account, so this is also
//     an escape hatch OUT of the unverified state.
//   - The account-lifecycle /auth/ mutations an unverified user must be
//     able to run: logout (end the session), verify-email +
//     resend-verification (the routes that clear the unverified state —
//     they land in Wave 3b; allowlisting them now is harmless because
//     they aren't routed yet), and delete-account (removing your own
//     unverified account is harmless).
//
// Everything else — token create/rotate/delete, PATCH /me, 2FA
// setup/disable, OAuth link/unlink, CLI-session approve, and every
// workspace/content mutation — is intentionally NOT here, so it gets
// the 403. The mutating auth routes that are unauthenticated (login,
// register, forgot/reset-password, bootstrap, 2fa/login-verify,
// oauth-login, CLI-session create) never reach this check: their
// callers have no currentUser, so emailUnverifiedBlocked already
// returned false.
func verifiedEmailExemptPath(path string) bool {
	// Invitation accept — matched structurally so any {code} passes,
	// while /invitations/{code}/preview (GET, already public) and any
	// other /invitations subpath are unaffected.
	if strings.HasPrefix(path, "/api/v1/invitations/") && strings.HasSuffix(path, "/accept") {
		return true
	}
	switch path {
	case "/api/v1/auth/logout",
		"/api/v1/auth/verify-email",
		"/api/v1/auth/resend-verification",
		"/api/v1/auth/delete-account":
		return true
	}
	return false
}

// RequireVerifiedEmail is the /api/v1 method-gated enforcement point for
// DR-4. Mounted immediately after RequireAuth in setupRouter, it rejects
// mutating requests (POST/PATCH/PUT/DELETE) from an authenticated cloud
// user whose email is unverified, returning 403 email_not_verified.
//
// It deliberately does NOT inherit CSRFProtect's / RequireAuth's blanket
// `/api/v1/auth/*` exemption — those exemptions exist so unauthenticated
// callers can reach login/register, but an authenticated-but-unverified
// user hitting POST /api/v1/auth/tokens must still be blocked. This
// middleware makes its own decision: emailUnverifiedBlocked auto-skips
// the unauthenticated routes (currentUser==nil), and
// verifiedEmailExemptPath allows the handful of authenticated auth
// mutations an unverified user legitimately needs.
//
// The method gate is necessary but NOT sufficient — several mutation
// surfaces are reached without a mutating /api/v1 method:
//
//   - the collab WebSocket upgrade is a GET (handled in
//     authorizeCollabAccess),
//   - the OAuth-provider authorize/decide endpoints are mounted outside
//     /api/v1 (handled in handlers_oauth.go),
//   - the remote MCP write path dispatches in-process (handled by the
//     dispatcher's RequireVerifiedEmail hook, wired in cmd/pad/main.go).
//
// Each of those is gated separately; see DR-4's systematic perimeter
// audit and the tests in middleware_verified_email_test.go.
func (s *Server) RequireVerifiedEmail(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.emailUnverifiedBlocked(currentUser(r)) {
			next.ServeHTTP(w, r)
			return
		}
		if !isMutatingMethod(r.Method) || verifiedEmailExemptPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		writeEmailNotVerified(w)
	})
}

// writeEmailNotVerified is the single 403 writer shared by every
// verified-email enforcement point so the code/message stay identical
// across perimeters (the middleware, the collab upgrade, the OAuth
// handlers). Clients branch on the stable `email_not_verified` code.
func writeEmailNotVerified(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, "email_not_verified",
		"Verify your email address to perform this action. Check your inbox for the verification link or request a new one.")
}
