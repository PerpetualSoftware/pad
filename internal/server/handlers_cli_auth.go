package server

import (
	"errors"
	"fmt"
	"github.com/PerpetualSoftware/pad/internal/store"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// handleCreateCLIAuthSession creates a new pending CLI auth session.
// The CLI calls this, then presents the auth URL to the user.
// POST /api/v1/auth/cli/sessions
func (s *Server) handleCreateCLIAuthSession(w http.ResponseWriter, r *http.Request) {
	// On a fresh instance (no users yet) this session is part of the
	// first-run setup handoff: the CLI mints it BEFORE the operator creates
	// the admin account in the browser, so /setup can redirect straight to
	// the approval page (BUG-1843). Grant the longer setup TTL so the
	// combined account-creation + approval window doesn't expire mid-flow.
	// Once users exist (normal `pad auth login`), use the shorter default.
	// A failed count falls back to the default TTL rather than blocking login.
	setup := false
	if count, err := s.store.UserCount(); err == nil && count == 0 {
		setup = true
	}

	// Who asked, for the approval page (TASK-2253). The User-Agent is the
	// requester's own claim; the store caps it and the page labels it so.
	sess, err := s.store.CreateCLIAuthSessionFrom(setup, store.CLIAuthRequester{
		IP:        clientIP(r),
		UserAgent: requestUserAgent(r),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to create CLI auth session")
		return
	}

	// Build the auth URL that the user will open in their browser.
	// Use the request's Host header to construct the URL so it works
	// regardless of whether the server is at localhost, a VPS, or cloud.
	//
	// Security: derive the scheme from r.TLS first. Any HTTP client can
	// inject X-Forwarded-Proto, so trust it ONLY when the request arrived
	// from a peer in PAD_TRUSTED_PROXIES. A forged https:// in the CLI
	// auth URL is low-impact phishing (user clicks a link in their own
	// terminal), but the safe default is to ignore unauthenticated proxy
	// headers on direct-exposed deployments.
	scheme := cliAuthScheme(r, s.trustedProxyCIDRs)
	authURL := fmt.Sprintf("%s://%s/auth/cli/%s", scheme, r.Host, sess.Code)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"session_code": sess.Code,
		"auth_url":     authURL,
		"expires_at":   sess.ExpiresAt,
	})
}

// handlePollCLIAuthSession checks the status of a CLI auth session.
// The CLI polls this until the session is approved or expired.
// GET /api/v1/auth/cli/sessions/{code}
func (s *Server) handlePollCLIAuthSession(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	if code == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Missing session code")
		return
	}

	sess, err := s.store.GetCLIAuthSession(code)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to check CLI auth session")
		return
	}
	if sess == nil {
		writeError(w, http.StatusNotFound, "not_found", "CLI auth session not found")
		return
	}
	if sess.Status == "denied" {
		// An error, not a 200 status (TASK-2253): a CLI from before Deny
		// existed switches on status with no default and would keep polling
		// a 200 "denied" for its full 20 minutes. A non-2xx counts as a poll
		// error there, so it gives up after five in a row and prints this
		// message. A current CLI recognises the code and stops at once.
		writeError(w, http.StatusGone, "cli_auth_denied", "This sign-in was denied in the browser.")
		return
	}

	response := map[string]interface{}{
		"status": sess.Status,
	}
	if sess.Status == "pending" {
		// Context for the person deciding (TASK-2253). Whoever holds the
		// code is the requester (this is their own address) or the person
		// they sent the link to, who needs it to decide.
		response["created_at"] = sess.CreatedAt
		if sess.RequesterIP != "" {
			response["requester_ip"] = sess.RequesterIP
		}
		if sess.RequesterUserAgent != "" {
			response["requester_user_agent"] = sess.RequesterUserAgent
		}
	}

	// Only include the token and user info when approved
	if sess.Status == "approved" && sess.Token != "" {
		response["token"] = sess.Token

		// Look up user info to return alongside the token
		if sess.UserID != "" {
			user, err := s.store.GetUser(sess.UserID)
			if err == nil && user != nil {
				response["user"] = sessionUserPayload(user)
			}
		}

		// Clean up the session after it's been consumed
		_ = s.store.DeleteCLIAuthSession(code)
	}

	writeJSON(w, http.StatusOK, response)
}

// handleApproveCLIAuthSession approves a pending CLI auth session.
// Called from the browser by an authenticated user.
// POST /api/v1/auth/cli/sessions/{code}/approve
func (s *Server) handleApproveCLIAuthSession(w http.ResponseWriter, r *http.Request) {
	// Approval mints a 30-day session, so it is an interactive-session
	// action (BUG-3336, BUG-3349): a PAT that could approve would outlive
	// its own revocation through the session it minted.
	if isAPITokenAuth(r) {
		writeError(w, http.StatusForbidden, "session_required",
			"Approving a CLI sign-in requires an interactive session, not an API token")
		return
	}
	code := chi.URLParam(r, "code")
	if code == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Missing session code")
		return
	}

	// Resolve the authenticated user — try context first (set by middleware),
	// then fall back to session cookie (since auth routes are exempt from middleware).
	user := currentUser(r)
	if user == nil {
		user = s.validateSessionCookie(r)
	}
	// A bot is no signed-in person (TASK-3392). This is the THIRD layer, kept
	// as defence in depth: a bot never reaches here today, because (1)
	// ValidateSession resolves a bot's session to nothing, so currentUser and
	// validateSessionCookie return nil for it, and (2) the approver check
	// below requires a resolved session belonging to this same user. Neither
	// delete this as dead code nor rely on it alone; no test can isolate it.
	if user.IsApp() {
		user = nil
	}
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "You must be logged in to approve a CLI session")
		return
	}

	// Verify the session exists and is pending
	sess, err := s.store.GetCLIAuthSession(code)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to check CLI auth session")
		return
	}
	if sess == nil {
		writeError(w, http.StatusNotFound, "not_found", "CLI auth session not found")
		return
	}
	if sess.Status == "expired" {
		writeError(w, http.StatusGone, "expired", "This CLI auth session has expired. Run 'pad auth login' again.")
		return
	}
	if sess.Status == "approved" {
		writeError(w, http.StatusConflict, "already_approved", "This CLI session has already been approved")
		return
	}
	if sess.Status == "denied" {
		writeError(w, http.StatusConflict, "cli_auth_denied", "This sign-in was denied. Run 'pad auth login' again.")
		return
	}

	// Create a new session token for the CLI (long-lived, 30 days). It is
	// dated by the approving session's sign-in (BUG-3336): approving from an
	// existing session is not a fresh sign-in, so it must not hand out a
	// session that reads as one.
	approver := s.requestSessionInfo(r)
	if approver == nil || approver.User.ID != user.ID {
		writeError(w, http.StatusUnauthorized, "unauthorized", "You must be logged in to approve a CLI session")
		return
	}
	// Fenced on the approver's credential epoch as this request was admitted
	// (BUG-3382): a claim or reset since then makes the approval mint nothing.
	token, err := s.store.CreateSessionFenced(user.ID, user.CredentialEpoch, "cli-browser-auth", clientIP(r), "", 30*24*time.Hour, approver.CreatedAt)
	if errors.Is(err, store.ErrUserDisabled) {
		// Disabled between this request's admission and the mint (BUG-3349).
		writeError(w, http.StatusForbidden, "account_disabled", "Your account has been disabled. Contact an administrator.")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to create session")
		return
	}

	// Approve the CLI auth session with the new token
	if err := s.store.ApproveCLIAuthSession(code, token, user.ID); err != nil {
		// The session minted above reaches nobody now; end it rather than
		// leave a live 30-day credential unreferenced.
		_ = s.store.DeleteSession(token)
		if errors.Is(err, store.ErrCLIAuthNotPending) {
			// Denied (or approved elsewhere) between the read above and
			// this write (TASK-2253).
			writeError(w, http.StatusConflict, "cli_auth_denied", "This sign-in is no longer waiting for approval. Run 'pad auth login' again.")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to approve CLI auth session")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"approved": true,
		"user":     sessionUserPayload(user),
	})
}

// handleDenyCLIAuthSession refuses a pending CLI auth session (TASK-2253).
// POST /api/v1/auth/cli/sessions/{code}/deny
//
// The code is the authority, not an account: it is already a bearer secret
// (whoever holds it can poll, and collects the token after an approval),
// and a deny harms only the requester, who holds the same code. So the
// handler checks no user. It is not CSRF-exempt: from a browser it is a
// cookie-carrying POST like approve, and the approval page only offers it
// to a signed-in visitor anyway. Denying twice answers 200; an approved or
// expired session answers with what it is instead.
func (s *Server) handleDenyCLIAuthSession(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	if code == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Missing session code")
		return
	}
	err := s.store.DenyCLIAuthSession(code)
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"denied": true})
		return
	}
	if !errors.Is(err, store.ErrCLIAuthNotPending) {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to deny CLI auth session")
		return
	}
	sess, err := s.store.GetCLIAuthSession(code)
	switch {
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to check CLI auth session")
	case sess == nil:
		writeError(w, http.StatusNotFound, "not_found", "CLI auth session not found")
	case sess.Status == "approved":
		writeError(w, http.StatusConflict, "already_approved", "This CLI session has already been approved")
	default:
		writeError(w, http.StatusGone, "expired", "This CLI auth session has expired.")
	}
}

// cliAuthScheme picks the URL scheme for the CLI auth link.
//
// Precedence:
//  1. If the request hit this server over TLS (r.TLS != nil) → "https".
//  2. If a trusted proxy is configured AND the direct TCP peer is in one
//     of the trusted CIDRs, honor X-Forwarded-Proto. Only the first comma-
//     separated value is used and it must be "http" or "https".
//  3. Otherwise → "http". An untrusted client can still send X-Forwarded-
//     Proto but we ignore it.
//
// This prevents an attacker from forging https://… in the terminal URL
// printed by `pad auth login` on a plain-HTTP self-host deployment.
func cliAuthScheme(r *http.Request, trustedCIDRs []*net.IPNet) string {
	if r.TLS != nil {
		return "https"
	}
	if len(trustedCIDRs) > 0 {
		if peerIP := peerAddr(rawPeerAddr(r)); peerIP != nil && ipInCIDRs(peerIP, trustedCIDRs) {
			if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
				// Some proxies emit "https, http" when multiple hops are
				// involved; take the first value and normalize.
				first := strings.ToLower(strings.TrimSpace(strings.SplitN(proto, ",", 2)[0]))
				if first == "https" || first == "http" {
					return first
				}
			}
		}
	}
	return "http"
}
