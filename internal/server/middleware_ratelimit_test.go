package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRateLimit_AuthEndpointLimited(t *testing.T) {
	srv := testServer(t)

	// Bootstrap so auth endpoints actually process (not just "setup required")
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	// Login attempts should be rate-limited after burst (5)
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
			strings.NewReader(`{"email":"wrong@test.com","password":"wrong"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "10.0.0.1:1234"
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		// These should go through (even if returning 401)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d should not be rate-limited yet", i+1)
		}
	}

	// The 6th should be rate-limited
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"email":"wrong@test.com","password":"wrong"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "10.0.0.1:1234"
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 after burst, got %d", w.Code)
	}

	// Check Retry-After header
	if w.Header().Get("Retry-After") == "" {
		t.Error("expected Retry-After header on 429 response")
	}
}

func TestRateLimit_DifferentIPsNotAffected(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	// Exhaust rate limit for IP 10.0.0.1
	for i := 0; i < 6; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
			strings.NewReader(`{"email":"wrong@test.com","password":"wrong"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "10.0.0.1:1234"
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
	}

	// Different IP should still be allowed
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"email":"wrong@test.com","password":"wrong"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "10.0.0.2:1234"
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code == http.StatusTooManyRequests {
		t.Error("different IP should not be rate-limited")
	}
}

func TestRateLimit_SearchEndpointLimited(t *testing.T) {
	srv := testServer(t)

	// Search limiter has burst=10, so first 10 should succeed
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=test", nil)
		req.RemoteAddr = "10.0.0.3:1234"
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d should not be rate-limited yet (search burst=10)", i+1)
		}
	}

	// The 11th should be rate-limited
	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=test", nil)
	req.RemoteAddr = "10.0.0.3:1234"
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 after search burst, got %d", w.Code)
	}
}

func TestRateLimit_NonAPIPathsExempt(t *testing.T) {
	srv := testServer(t)

	// Non-API paths should not be rate-limited
	for i := 0; i < 50; i++ {
		req := httptest.NewRequest(http.MethodGet, "/login", nil)
		req.RemoteAddr = "10.0.0.4:1234"
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("non-API request %d should not be rate-limited", i+1)
		}
	}
}

func TestClientIP(t *testing.T) {
	// clientIP only reads RemoteAddr (proxy headers are handled by chimiddleware.RealIP)
	tests := []struct {
		name       string
		remoteAddr string
		want       string
	}{
		{"with port", "192.168.1.1:1234", "192.168.1.1"},
		{"no port", "10.0.0.1", "10.0.0.1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req.RemoteAddr = tt.remoteAddr
			got := clientIP(req)
			if got != tt.want {
				t.Errorf("clientIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClientIP_IgnoresProxyHeaders(t *testing.T) {
	// Ensure clientIP does NOT trust X-Real-IP or X-Forwarded-For
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "192.168.1.1:1234"
	req.Header.Set("X-Real-IP", "10.0.0.99")
	req.Header.Set("X-Forwarded-For", "10.0.0.88")

	got := clientIP(req)
	if got != "192.168.1.1" {
		t.Errorf("clientIP should ignore proxy headers, got %q", got)
	}
}
