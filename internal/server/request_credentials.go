package server

import (
	"net/http"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/secret"
)

// Request credentials (TASK-4323). A token read from a header or a cookie is
// held as a secret.Secret, so a log line or a %v that picks it up shows the
// marker. Reveal it only where it is used: a store lookup, a hash, a compare.

// bearerToken reads the Authorization header the way TokenAuth always has:
// the value is the header with one leading "Bearer " removed and spaces
// trimmed, and ok reports whether that exact, case-sensitive prefix was
// there. Callers that require the scheme check ok; the liveness and session
// readers, which run only after the middleware accepted a session bearer,
// take the value alone. extractBearer is the case-insensitive parse the MCP
// door uses.
func bearerToken(r *http.Request) (tok secret.Secret, ok bool) {
	h := r.Header.Get("Authorization")
	return secret.New(strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))), strings.HasPrefix(h, "Bearer ")
}

// cookieSecret returns the named cookie's value. ok is false when the cookie
// is absent; a present cookie may still be empty (IsZero).
func cookieSecret(r *http.Request, name string) (secret.Secret, bool) {
	c, err := r.Cookie(name)
	if err != nil {
		return secret.Secret{}, false
	}
	return secret.New(c.Value), true
}

// sessionCookie returns the session cookie under its current name, falling
// back to the unprefixed pad_session a pre-upgrade browser still holds. A
// present primary cookie wins even when empty, as before.
func (s *Server) sessionCookie(r *http.Request) (secret.Secret, bool) {
	if v, ok := cookieSecret(r, sessionCookieName(s.secureCookies)); ok {
		return v, true
	}
	return cookieSecret(r, "pad_session")
}
