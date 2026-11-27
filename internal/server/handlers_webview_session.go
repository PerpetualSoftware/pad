package server

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/PerpetualSoftware/pad/internal/logging"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// webViewSessionTTL is a WebView session's whole life (TASK-4454). It never
// slides; the app mints a new one on each identity proof, about every 240 s.
const webViewSessionTTL = 15 * time.Minute

// webViewSessionBodyLimit caps the optional {"replace_previous": bool} body.
const webViewSessionBodyLimit = 1 << 10

type webViewSessionBody struct {
	ReplacePrevious bool `json:"replace_previous"`
}

// handleCreateWebViewSession handles POST /api/v1/auth/webview-session
// (TASK-4454). The mobile app calls it on an http connection, with the web
// session it holds as a bearer, after it has verified the server; it seeds the
// returned child into its WebView as the session cookie instead of its own
// token. The child is accepted only as that cookie, never slides, expires
// within 15 minutes or with the parent, and is refused at every door
// webViewRouteRefused names. The token is never logged and the response is
// not stored; no Set-Cookie is sent, because the caller is the native client.
func (s *Server) handleCreateWebViewSession(w http.ResponseWriter, r *http.Request) {
	switch authKind(r) {
	case authKindSessionBearer:
	case "":
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	default:
		writeError(w, http.StatusForbidden, "session_bearer_required",
			"A WebView session is minted with the app's signed-in session sent as a bearer, not a cookie or an API token")
		return
	}
	info := s.requestSessionInfo(r)
	if info == nil || info.ID == "" || info.User == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	}
	if info.Kind != store.SessionKindWeb {
		writeError(w, http.StatusForbidden, "webview_session_forbidden",
			"A CLI session cannot mint a WebView session")
		return
	}

	raw, err := readBodyForDecode(r, webViewSessionBodyLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}
	var in webViewSessionBody
	if len(bytes.TrimSpace(raw)) > 0 {
		if in, err = strictBodyBytes[webViewSessionBody](raw); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
			return
		}
	}

	if !s.allowWebViewSessionMint(w, r, info.ID) {
		return
	}

	// No User-Agent binding: the caller is the app's native client and the
	// child is used by its WebView, whose User-Agent differs, so a hash of
	// this request's header would mismatch on every WebView request (and
	// revoke the child under PAD_IP_CHANGE_ENFORCE=strict). The address
	// binding stays: both run on the same device.
	token, expiresAt, err := s.store.CreateWebViewSession(r.Context(), info.ID, info.User.ID,
		clientIP(r), "", webViewSessionTTL, in.ReplacePrevious)
	switch {
	case errors.Is(err, store.ErrSessionGone):
		writeError(w, http.StatusUnauthorized, "unauthorized", "Your session has ended. Sign in again.")
		return
	case errors.Is(err, store.ErrUserDisabled):
		writeError(w, http.StatusForbidden, "account_disabled", "Your account has been disabled. Contact an administrator.")
		return
	case err != nil:
		writeStoreErrorR(w, r, err, http.StatusInternalServerError, "internal_error", "Failed to create the WebView session")
		return
	}
	maxAge := int(time.Until(expiresAt).Seconds())
	if maxAge < 0 {
		maxAge = 0
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":      token,
		"expires_at": expiresAt.UTC().Format(time.RFC3339),
		"max_age":    maxAge,
	})
}

// allowWebViewSessionMint charges the parent session's WebViewSession bucket.
// It returns false having written the 429. A nil limiter set
// (PAD_DISABLE_RATE_LIMITS) allows.
func (s *Server) allowWebViewSessionMint(w http.ResponseWriter, r *http.Request, parentID string) bool {
	if s.rateLimiters == nil || s.rateLimiters.WebViewSession == nil {
		return true
	}
	if s.rateLimiters.WebViewSession.allow("session:" + parentID) {
		return true
	}
	slog.Warn("rate limited", "path", logging.RedactPath(r.URL.Path), "limiter", "webview_session")
	writeRateLimitResponse(w, s.rateLimiters.WebViewSession.config)
	return false
}
