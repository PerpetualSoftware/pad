package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// BUG-3310: the general API bucket is charged from two arms of the rate
// limit middleware: the auth-default arm (/api/v1/auth/* paths with no
// bucket of their own) and the general arm. Both must use one key, so a
// caller holds one API bucket, not one per arm.

// rl3310Request sends GET path from remote, with a session cookie when
// session is non-empty, and returns the status.
func rl3310Request(srv *Server, path, remote, session string) int {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = remote
	if session != "" {
		req.Header.Set("User-Agent", testSessionUA)
		req.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: session})
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w.Code
}

// rl3310Exhaust sends path until it draws a 429, failing the test if 200
// requests never do.
func rl3310Exhaust(t *testing.T, srv *Server, path, remote, session string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if rl3310Request(srv, path, remote, session) == http.StatusTooManyRequests {
			return
		}
	}
	t.Fatalf("200 requests to %s from %s never drew a 429", path, remote)
}

func TestRateLimit_AnonymousHoldsOneAPIBucketAcrossArms(t *testing.T) {
	const (
		authDefault = "/api/v1/auth/session"
		general     = "/api/v1/workspaces"
	)
	for _, c := range []struct{ name, spend, probe string }{
		{"auth-default then general", authDefault, general},
		{"general then auth-default", general, authDefault},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := testServer(t)
			freezeRefill(t, srv)
			rl3310Exhaust(t, srv, c.spend, "192.0.2.50:1", "")
			if code := rl3310Request(srv, c.probe, "192.0.2.50:1", ""); code != http.StatusTooManyRequests {
				t.Errorf("%s from the same address got %d, want 429: the arms keep separate API buckets", c.probe, code)
			}
			if code := rl3310Request(srv, c.probe, "192.0.2.51:1", ""); code == http.StatusTooManyRequests {
				t.Errorf("%s from another address got 429", c.probe)
			}
		})
	}
}

// A signed-in caller's auth-default requests draw from their USER bucket,
// as their other API requests do, so changing address does not refill it
// and an anonymous neighbour on their address is not charged for them.
func TestRateLimit_SignedInAuthDefaultUsesTheUserBucket(t *testing.T) {
	srv := testServer(t)
	freezeRefill(t, srv)
	session := bootstrapFirstUser(t, srv, "rl3310@test.com", "RL")

	rl3310Exhaust(t, srv, "/api/v1/workspaces", "192.0.2.60:1", session)
	if code := rl3310Request(srv, "/api/v1/auth/session", "192.0.2.61:1", session); code != http.StatusTooManyRequests {
		t.Errorf("auth-default request by the same user from another address got %d, want 429", code)
	}
	if code := rl3310Request(srv, "/api/v1/auth/session", "192.0.2.60:1", ""); code == http.StatusTooManyRequests {
		t.Error("an anonymous request from the user's address got 429: it was charged for the user's requests")
	}
}
