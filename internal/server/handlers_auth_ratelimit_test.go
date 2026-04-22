package server

import (
	"net/http"
	"testing"
)

// TestHandleLogin_PerEmailRateLimit asserts that the per-email limiter
// (10/hour burst 10) triggers before the per-IP limiter (5/min burst 5)
// when attempts come from many distinct IPs — exactly the credential-
// spraying scenario the limiter is designed to defeat.
func TestHandleLogin_PerEmailRateLimit(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "victim@example.com", "Victim")

	// 10 wrong-password attempts from 10 distinct IPs should all get 401
	// (password wrong) but consume the email limiter to exhaustion.
	for i := 0; i < 10; i++ {
		ip := []string{
			"203.0.113.1:1", "203.0.113.2:1", "203.0.113.3:1", "203.0.113.4:1",
			"203.0.113.5:1", "203.0.113.6:1", "203.0.113.7:1", "203.0.113.8:1",
			"203.0.113.9:1", "203.0.113.10:1",
		}[i]
		rr := doRequestFromRemoteAddr(srv, "POST", "/api/v1/auth/login", map[string]string{
			"email":    "victim@example.com",
			"password": "wrong-password",
		}, ip)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d from %s: expected 401, got %d: %s", i, ip, rr.Code, rr.Body.String())
		}
	}

	// 11th attempt from a fresh IP — per-IP limiter is still fine, but the
	// per-email limiter should now reject.
	rr := doRequestFromRemoteAddr(srv, "POST", "/api/v1/auth/login", map[string]string{
		"email":    "victim@example.com",
		"password": "wrong-password",
	}, "203.0.113.11:1")
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("11th attempt: expected 429 (per-email lockout), got %d: %s", rr.Code, rr.Body.String())
	}

	// Sanity check: a DIFFERENT email from a fresh IP should still work (401,
	// not 429) — we only want the sprayed target to be locked out.
	rr = doRequestFromRemoteAddr(srv, "POST", "/api/v1/auth/login", map[string]string{
		"email":    "other@example.com",
		"password": "wrong-password",
	}, "198.51.100.1:1")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("other email: expected 401, got %d (per-email lockout leaked across accounts?)", rr.Code)
	}
}

// TestHandleLogin_EmailCaseInsensitive ensures the per-email limiter keys
// by lowercased email, so an attacker can't bypass by alternating casing.
// Each attempt uses a distinct IP so the per-IP limiter doesn't trip first.
func TestHandleLogin_EmailCaseInsensitive(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "mixed@example.com", "Mixed")

	ips := []string{
		"203.0.113.101:1", "203.0.113.102:1", "203.0.113.103:1", "203.0.113.104:1",
		"203.0.113.105:1", "203.0.113.106:1", "203.0.113.107:1", "203.0.113.108:1",
		"203.0.113.109:1", "203.0.113.110:1",
	}
	for i := 0; i < 10; i++ {
		// Alternate casing each attempt.
		email := "MIXED@example.com"
		if i%2 == 0 {
			email = "mixed@Example.com"
		}
		rr := doRequestFromRemoteAddr(srv, "POST", "/api/v1/auth/login", map[string]string{
			"email":    email,
			"password": "wrong",
		}, ips[i])
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d (%s from %s): expected 401, got %d", i, email, ips[i], rr.Code)
		}
	}

	rr := doRequestFromRemoteAddr(srv, "POST", "/api/v1/auth/login", map[string]string{
		"email":    "Mixed@Example.Com",
		"password": "wrong",
	}, "198.51.100.100:1")
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("casing-variant attempt: expected 429, got %d (limiter key not normalized?)", rr.Code)
	}
}
