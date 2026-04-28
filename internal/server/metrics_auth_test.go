package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/metrics"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// newMetricsTestServer builds a Server with metrics enabled and the given
// static token. Uses a unique SQLite DB per test so tests never share state.
func newMetricsTestServer(t *testing.T, token string) *Server {
	t.Helper()
	dir := t.TempDir()
	s, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	srv := New(s)
	srv.SetMetrics(metrics.New())
	srv.SetMetricsToken(token)
	// Drain background goroutines BEFORE closing the store — see
	// testServer in server_test.go for the BUG-842 race details.
	t.Cleanup(func() {
		srv.Stop()
		s.Close()
	})
	return srv
}

func doMetricsRequest(srv *Server, remoteAddr, authHeader string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.RemoteAddr = remoteAddr
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// TestMetricsAuth_TokenUnset_LoopbackOnly: no PAD_METRICS_TOKEN → loopback
// callers succeed, everyone else gets 403. Safe default for self-hosters
// running Prometheus on the same box.
func TestMetricsAuth_TokenUnset_LoopbackOnly(t *testing.T) {
	srv := newMetricsTestServer(t, "")

	rr := doMetricsRequest(srv, "127.0.0.1:12345", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("loopback /metrics: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = doMetricsRequest(srv, "203.0.113.5:12345", "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("non-loopback /metrics: expected 403 (no token configured), got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestMetricsAuth_TokenSet_RequiresBearer: with PAD_METRICS_TOKEN set,
// every scrape — even loopback — must present the correct Bearer token.
func TestMetricsAuth_TokenSet_RequiresBearer(t *testing.T) {
	srv := newMetricsTestServer(t, "super-secret-token")

	// Missing header → 401
	rr := doMetricsRequest(srv, "127.0.0.1:1", "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no auth header: expected 401, got %d", rr.Code)
	}

	// Wrong token → 401
	rr = doMetricsRequest(srv, "127.0.0.1:1", "Bearer wrong-token")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: expected 401, got %d", rr.Code)
	}

	// Non-Bearer scheme → 401
	rr = doMetricsRequest(srv, "127.0.0.1:1", "Basic dXNlcjpwYXNz")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("basic auth: expected 401, got %d", rr.Code)
	}

	// Correct token — from anywhere — succeeds.
	rr = doMetricsRequest(srv, "203.0.113.5:1", "Bearer super-secret-token")
	if rr.Code != http.StatusOK {
		t.Fatalf("valid token: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// WWW-Authenticate header is set for failed attempts.
	rr = doMetricsRequest(srv, "127.0.0.1:1", "Bearer wrong")
	if got := rr.Header().Get("WWW-Authenticate"); got == "" {
		t.Error("expected WWW-Authenticate header on 401")
	}
}

// TestMetricsAuth_NoMetricsMeans404 sanity-checks that /metrics is
// simply not routed when metrics are disabled (SetMetrics never called).
func TestMetricsAuth_NoMetricsMeans404(t *testing.T) {
	srv := testServer(t) // no SetMetrics
	rr := doMetricsRequest(srv, "127.0.0.1:1", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("metrics disabled: expected 404, got %d", rr.Code)
	}
}
