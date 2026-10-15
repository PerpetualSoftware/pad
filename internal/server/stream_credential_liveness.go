package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/ory/fosite"
)

// BUG-3007 — a long-lived connection must stop when the CREDENTIAL that opened
// it stops being valid.
//
// The three long-lived connections this was written for revalidated something
// on a jittered ~60s tick, and none of them asked this question (the MCP
// transport's GET and listen streams, a fourth, ask it since BUG-3837):
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

// credentialLiveness is the three-valued answer this predicate needs.
//
// TWO-VALUED WOULD BE WRONG, and the first version of this file was. "The
// lookup says invalid" and "the lookup could not be performed" are opposite
// facts that a bool collapses: returning false on a store error closed every
// affected stream on the next tick of a transient DB blip — a reconnect storm
// and an availability loss for users whose credentials were fine (codex round
// 1). Returning true on an UNRECOGNISED credential kind is the mirror error,
// and is the defect BUG-3007 exists to fix, wearing a different coat.
type credentialLiveness int

const (
	// credentialValid — re-checked and still good.
	credentialValid credentialLiveness = iota
	// credentialInvalid — re-checked and definitively gone. Close.
	credentialInvalid
	// credentialUnknown — could not be re-checked (store error). Keep the
	// connection and try again on the next tick; the caller logs.
	credentialUnknown
)

// streamCredentialStillValid reports whether a long-lived connection may keep
// running, and is the only thing the three tick sites call.
//
// It KEEPS the connection on `credentialUnknown`, because a database blip is
// not a revocation, and ENDS it on `credentialInvalid` and on an unrecognised
// credential kind.
func (s *Server) streamCredentialStillValid(r *http.Request) bool {
	switch s.credentialLiveness(r) {
	case credentialInvalid:
		return false
	case credentialUnknown:
		// One tick of extra staleness, bounded by the store recovering. The
		// alternative — closing — turns every transient store error into a
		// fleet-wide disconnect, which is worse than the bounded staleness
		// these loops already accept for membership changes.
		slog.Warn("stream: credential re-check failed, keeping the connection until the next tick",
			"path", r.URL.Path, "auth_kind", authKind(r))
		return true
	default:
		return true
	}
}

// credentialLiveness re-checks the credential that opened this request.
//
// It switches on how the MIDDLEWARE said the principal was established
// (`ctxAuthKind`) rather than re-sniffing the wire, so there is exactly one
// place on this server that decides whether a bearer is a session or an API
// token. Re-deriving it here would be a second place to get that split wrong,
// and getting it wrong closes every CLI stream on its first tick.
func (s *Server) credentialLiveness(r *http.Request) credentialLiveness {
	switch authKind(r) {
	case authKindSessionBearer:
		return s.sessionLiveness(strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))

	case authKindSessionCookie:
		// Both names SessionAuth accepts, including the unprefixed fallback it
		// keeps for the upgrade path.
		for _, name := range []string{sessionCookieName(s.secureCookies), "pad_session"} {
			cookie, err := r.Cookie(name)
			if err != nil || cookie.Value == "" {
				continue
			}
			return s.sessionLiveness(cookie.Value)
		}
		// The middleware said "session cookie" and the request has none. That
		// is not a state this server produces; treat it as unrecognised.
		slog.Warn("stream: auth_kind says session cookie but the request carries none, closing",
			"path", r.URL.Path)
		return credentialInvalid

	case authKindAPIToken:
		// extractBearer, the parse both bearer accept points use: the scheme
		// is case-insensitive (RFC 6750), so a case-sensitive trim here would
		// read "bearer <tok>" as the whole header and close a live stream
		// every tick (BUG-3837 hunter r1).
		token, ok := extractBearer(r.Header.Get("Authorization"))
		if !ok {
			return credentialInvalid
		}
		// The LIVENESS door, not the ordinary one: a revalidation tick must not
		// bump `last_used_at`. See ValidateTokenForLiveness for why that is a
		// meaning problem as much as a cost one.
		tok, err := s.store.ValidateTokenForLiveness(token)
		if err != nil {
			return credentialUnknown
		}
		if tok == nil {
			return credentialInvalid
		}
		return credentialValid

	case authKindOAuth:
		token, _ := extractBearer(r.Header.Get("Authorization"))
		return s.oauthLiveness(r.Context(), token)

	case "":
		// NOTHING TO REVALIDATE, and this branch is explicit rather than a
		// default so it cannot absorb a future kind by accident.
		//
		// Two principals reach the stream handlers with no credential on the
		// wire: the fresh-install window before the first admin exists, and a
		// legacy no-auth deployment. Neither has anything that can be revoked,
		// and closing their streams would turn a security fix into an
		// availability regression on exactly the deployments least able to
		// diagnose it.
		//
		// A RESOLVED PRINCIPAL WITH NO KIND IS NOT THAT CASE, and is the one
		// way the empty branch could quietly become the exemption the default
		// branch exists to prevent (codex round 2, P1). Somebody was
		// authenticated, so a credential was accepted; an accept point that
		// records the user and not how it established them leaves this
		// predicate with nothing to re-check and no way to notice. Both MCP
		// accept points were in exactly that state when this was written. Both
		// now record their kind (the OAuth one since BUG-3837, with its own
		// door), and an accept point added later without one lands here and
		// CLOSES rather than streaming past a revoked credential.
		if currentUser(r) != nil {
			slog.Warn("stream: a principal was resolved without an auth kind, closing the connection",
				"path", r.URL.Path)
			return credentialInvalid
		}
		return credentialValid

	default:
		// FAILS CLOSED, deliberately (lead ruling, day 63). A credential kind
		// nobody taught this predicate about is a connection nobody can
		// revoke, which is BUG-3007 again under a new name. A new auth path
		// that forgets to set `ctxAuthKind` therefore breaks loudly — its
		// long-lived connections end on the first tick and say why — rather
		// than quietly exempting itself from revalidation.
		slog.Warn("stream: unrecognised credential kind, closing the connection",
			"path", r.URL.Path, "auth_kind", authKind(r))
		return credentialInvalid
	}
}

// sessionLiveness re-validates a session token, distinguishing "gone" from
// "could not ask".
func (s *Server) sessionLiveness(token string) credentialLiveness {
	if token == "" {
		return credentialInvalid
	}
	info, err := s.store.ValidateSession(token)
	if err != nil {
		return credentialUnknown
	}
	if info == nil {
		return credentialInvalid
	}
	return credentialValid
}

// oauthLiveness re-introspects an MCP OAuth access token (BUG-3837). One
// fosite introspection answers all three ways the credential can end: a
// revoked token or grant (RevokeOAuthGrant flips every access token in the
// family to active=0, read back as ErrInactiveToken), a deleted row
// (ErrNotFound), and expiry (ErrTokenExpired, from the session's ExpiresAt).
// Introspection writes nothing.
//
// fosite wraps a STORAGE failure in ErrRequestUnauthorized too, so the error
// is classified by its cause, not its outer type: the named causes end the
// stream, and anything else is "could not ask", which keeps it, as the
// session and PAT doors do on a store error.
func (s *Server) oauthLiveness(ctx context.Context, token string) credentialLiveness {
	if token == "" || s.oauthServer == nil {
		return credentialInvalid
	}
	_, tokenUse, err := s.oauthServer.IntrospectToken(ctx, token)
	switch {
	case err == nil:
		if tokenUse != fosite.AccessToken {
			return credentialInvalid
		}
		return credentialValid
	case errors.Is(err, fosite.ErrInactiveToken),
		errors.Is(err, fosite.ErrNotFound),
		errors.Is(err, fosite.ErrTokenExpired):
		return credentialInvalid
	default:
		slog.Warn("stream: oauth liveness check could not complete", "error", err)
		return credentialUnknown
	}
}
