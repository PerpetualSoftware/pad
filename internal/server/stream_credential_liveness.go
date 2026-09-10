package server

import (
	"net/http"
	"strings"
)

// BUG-3007 — a long-lived connection must stop when the CREDENTIAL that opened
// it stops being valid.
//
// All three long-lived connections on this server revalidate something on a
// jittered ~60s tick, and none of them asked this question:
//
//   - GET /api/v1/events revalidates the connecting user's MEMBERSHIP, which a
//     sign-out does not change;
//   - GET /api/v1/events/stream re-fetches by USER ID and fails closed on a
//     deleted or disabled user — stricter, and still not this, because a logout
//     destroys a SESSION and leaves a live enabled user behind;
//   - GET /api/v1/collab/{itemID} re-fetches the ITEM and re-runs access for
//     the principal captured at UPGRADE time.
//
// Each kept running indefinitely after its credential was destroyed, measured
// on a live instance rather than inferred: 165s / 180s / 150s after a logout
// whose `/auth/me` answered 401, and 64s / 120s / 100s after a PAT revocation.
//
// WHY THE TICK AND NOT PER DELIVERY. All three already have a tick and a
// fail-closed branch to reuse, and a store lookup per delivered event would sit
// on the hot path of a fan-out. Bounded staleness of one tick matches what
// these loops already promise for membership changes.
//
// WHY THE PREDICATE IS WIDER THAN "IS THE SESSION GONE". A revoked PAT still
// streaming is the same defect for a CLI or MCP caller as a destroyed session
// is for a browser, so the invariant is "the credential that opened this
// connection is still valid" and that is deliberate rather than incidental.
//
// It re-reads the credential from the RETAINED REQUEST, which every one of
// these loops already carries for its existing checks — so no session id has to
// be threaded through a context that does not carry one today.

// streamCredentialStillValid reports whether the credential presented on r is
// still valid RIGHT NOW.
//
// A request carrying NO credential returns true: on the fresh-install window
// and the legacy no-auth path there is nothing to invalidate, and returning
// false there would close streams that were never authenticated in the first
// place — turning a security fix into an availability regression on exactly the
// deployments least able to diagnose it.
func (s *Server) streamCredentialStillValid(r *http.Request) bool {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		if token == "" {
			return true
		}
		// A CLI session bearer is a SESSION, not an API token — the same
		// split TokenAuth makes at middleware_auth.go:107. Validating it as a
		// PAT would answer "no such token" for a perfectly live session and
		// close every CLI stream on its first tick.
		if strings.HasPrefix(token, "padsess_") {
			info, err := s.store.ValidateSession(token)
			return err == nil && info != nil
		}
		tok, err := s.store.ValidateToken(token)
		return err == nil && tok != nil
	}

	// Cookie path, including the unprefixed fallback SessionAuth accepts for
	// the upgrade path (middleware_auth.go:239).
	for _, name := range []string{sessionCookieName(s.secureCookies), "pad_session"} {
		cookie, err := r.Cookie(name)
		if err != nil || cookie.Value == "" {
			continue
		}
		info, verr := s.store.ValidateSession(cookie.Value)
		return verr == nil && info != nil
	}

	return true
}
