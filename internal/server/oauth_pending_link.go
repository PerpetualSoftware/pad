package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/secret"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// Pending provider links (TASK-4745). An OAuth sign-in whose verified address
// matches an existing account without that provider linked is refused
// oauth_provider_not_linked. When the sidecar asks (pending_link), the refusal
// also keeps the asserted identity server-side for ten minutes behind a
// single-use token. The sidecar holds that token only as the browser's
// HttpOnly __Host- cookie below; it never appears in a URL, a redirect, an
// error shown to a browser or a log line. The account's next FULL sign-in in
// that browser (password and 2FA, or another linked provider) links it, with
// the user's consent, and the owner is emailed. Any completed sign-in spends
// the token; one by a different account links nothing.

// pendingLinkCookieName is set and cleared by pad-cloud on the provider
// callback; pad reads it on the password and 2FA doors and clears it there.
const pendingLinkCookieName = "__Host-pad_pending_link"

// Codes of a pending_link {linked: false} reply. A code only: never the
// provider account's subject.
const (
	pendingLinkCodeInvalid      = "pending_link_invalid"       // unknown, spent, expired, or another account's
	pendingLinkCodeNotRequested = "pending_link_not_requested" // the user unticked the link
	pendingLinkCodeIneligible   = "pending_link_ineligible"    // the account changed since (address, verification, disabled)
	pendingLinkCodeSubjectBound = "oauth_subject_mismatch"     // the provider account is bound to another Pad account
	pendingLinkCodeFailed       = "pending_link_failed"        // a store error, logged
)

// pendingLinkEligible: whether a not-linked refusal may hold the identity
// for a later link. Only an account that proved its address (an unverified
// one goes through the claim path, or nowhere, BUG-3348), live, not a bot,
// and only when the provider vouches for the address as GitHub does for a
// primary one (providerVouchesForClaim). An address alone never links: the
// link still needs the account's own sign-in.
func pendingLinkEligible(user *models.User, provider string, emailPrimary *bool) bool {
	return user != nil && user.IsEmailVerified() && !user.IsDisabled() && !user.IsApp() &&
		providerVouchesForClaim(provider, emailPrimary)
}

// pendingLinkCookie returns the browser's pending-link token, if any.
func pendingLinkCookie(r *http.Request) (secret.Secret, bool) {
	tok, ok := cookieSecret(r, pendingLinkCookieName)
	if !ok || tok.IsZero() {
		return secret.Secret{}, false
	}
	return tok, true
}

// clearPendingLinkCookie expires the cookie. __Host- requires Secure and
// Path=/, so the clear carries both whatever the server's own cookie mode.
func clearPendingLinkCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     pendingLinkCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// completePendingLinkFromCookie runs completePendingLink for a password or 2FA
// sign-in, whose token is the browser's cookie. Consent is the request's
// link_pending. The cookie is cleared whatever the outcome. Returns nil when
// the server is not in cloud mode or the browser holds no token.
func (s *Server) completePendingLinkFromCookie(w http.ResponseWriter, r *http.Request, user *models.User, consent bool) map[string]interface{} {
	if !s.cloudMode {
		return nil
	}
	tok, ok := pendingLinkCookie(r)
	if !ok {
		return nil
	}
	clearPendingLinkCookie(w)
	return s.completePendingLink(r, user, tok, consent)
}

// completePendingLink runs after a sign-in minted its session (cloud mode
// only). It spends the token, then links its provider only when the token is
// this account's, consent was given, and the account as re-read now is still
// verified, live, not a bot, and at the address the provider asserted. The
// reply is {provider, linked: true} or {linked: false, code}.
func (s *Server) completePendingLink(r *http.Request, user *models.User, token secret.Secret, consent bool) map[string]interface{} {
	if !s.cloudMode || token.IsZero() || user == nil {
		return nil
	}
	notLinked := func(code string) map[string]interface{} {
		return map[string]interface{}{"linked": false, "code": code}
	}
	// Any completed sign-in spends it, the right account's or not.
	pl, err := s.store.ConsumeOAuthPendingLink(token.Reveal())
	if err != nil {
		slog.Error("pending link: consume failed", "error", err, "user_id", user.ID)
		return notLinked(pendingLinkCodeFailed)
	}
	if pl == nil || pl.UserID != user.ID {
		return notLinked(pendingLinkCodeInvalid)
	}
	if !consent {
		return notLinked(pendingLinkCodeNotRequested)
	}
	current, err := s.store.GetUser(user.ID)
	if err != nil {
		slog.Error("pending link: re-read account failed", "error", err, "user_id", user.ID)
		return notLinked(pendingLinkCodeFailed)
	}
	if current == nil || !current.IsEmailVerified() || current.IsDisabled() || current.IsApp() ||
		!strings.EqualFold(current.Email, pl.Email) {
		return notLinked(pendingLinkCodeIneligible)
	}
	if err := s.store.LinkOAuthProvider(current.ID, pl.Provider, pl.Subject); err != nil {
		if errors.Is(err, store.ErrOAuthSubjectMismatch) {
			s.logAuditEventForUser(models.ActionOAuthLoginFailed, r, current.ID, auditMeta(map[string]string{
				"provider": pl.Provider,
				"email":    current.Email,
				"reason":   "subject_mismatch",
				"via":      "pending_link",
			}))
			return notLinked(pendingLinkCodeSubjectBound)
		}
		slog.Error("pending link: link failed", "error", err, "user_id", current.ID, "provider", pl.Provider)
		return notLinked(pendingLinkCodeFailed)
	}
	s.logAuditEventForUser(models.ActionOAuthLogin, r, current.ID, auditMeta(map[string]string{
		"provider": pl.Provider,
		"email":    current.Email,
		"action":   "link_provider",
		"via":      "pending_link",
	}))
	slog.Info("pending link: provider linked", "provider", pl.Provider, "user_id", current.ID)
	s.sendProviderLinkedNotice(current, pl.Provider)
	return map[string]interface{}{"provider": pl.Provider, "linked": true}
}

// sendProviderLinkedNotice emails the account's owner that a provider was
// linked (TASK-4745 ruling). Best effort and asynchronous: a send failure is
// logged and never fails the sign-in or link that caused it.
func (s *Server) sendProviderLinkedNotice(user *models.User, provider string) {
	if s.email == nil {
		slog.Info("provider linked; no email configured for the notice", "user_id", user.ID, "provider", provider)
		return
	}
	to, name, userID := user.Email, user.Name, user.ID
	at := time.Now()
	s.goAsync(func() {
		if err := s.email.SendProviderLinked(context.Background(), to, name, provider, at); err != nil {
			slog.Error("failed to send provider-linked notice", "error", err, "user_id", userID, "provider", provider)
		}
	})
}

// handlePendingLink handles GET /api/v1/auth/pending-link (cloud mode only):
// what the login page needs to offer the link. With a live token in the
// browser's cookie: {pending: true, provider, email, password, providers}
// (whether the account has a password, and its other linked providers).
// Anything else is a byte-identical {"pending": false}. Never cached, and
// never readable cross-origin with credentials.
func (s *Server) handlePendingLink(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Del("Access-Control-Allow-Origin")
	h.Del("Access-Control-Allow-Credentials")
	none := func() { writeJSON(w, http.StatusOK, map[string]interface{}{"pending": false}) }

	tok, ok := pendingLinkCookie(r)
	if !ok {
		none()
		return
	}
	pl, err := s.store.PeekOAuthPendingLink(tok.Reveal())
	if err != nil {
		slog.Error("pending link: peek failed", "error", err)
		none()
		return
	}
	if pl == nil {
		none()
		return
	}
	user, err := s.store.GetUser(pl.UserID)
	if err != nil {
		slog.Error("pending link: read account failed", "error", err)
		none()
		return
	}
	if user == nil || !user.IsEmailVerified() || user.IsDisabled() || user.IsApp() || !strings.EqualFold(user.Email, pl.Email) {
		none()
		return
	}
	others := []string{}
	for _, p := range user.GetOAuthProviders() {
		if p != pl.Provider {
			others = append(others, p)
		}
	}
	sort.Strings(others)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"pending":   true,
		"provider":  pl.Provider,
		"email":     pl.Email,
		"password":  user.HasPassword(),
		"providers": others,
	})
}
