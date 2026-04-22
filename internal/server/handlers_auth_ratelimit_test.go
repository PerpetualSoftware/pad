package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestIsPlausibleEmail(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"valid email", "user@example.com", true},
		{"empty string", "", false},
		{"no at sign", "userexample.com", false},
		{"at at start", "@example.com", false},
		{"at at end", "user@", false},
		{"over 254 chars", strings.Repeat("a", 250) + "@b.com", false},
		{"exactly 254 chars", strings.Repeat("a", 246) + "@b.com", true}, // 246 + 1 + 5 = 252 → 254 cap OK
		{"unicode local part", "üser@example.com", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPlausibleEmail(tt.input); got != tt.want {
				t.Fatalf("isPlausibleEmail(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// TestHandleLogin_ImplausibleEmail_NoBucketCreated asserts that a long
// garbage string with no '@' does not create a bucket in the AuthEmail
// limiter — the memory-DoS defense Codex asked for on PR #178.
func TestHandleLogin_ImplausibleEmail_NoBucketCreated(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "victim@example.com", "Victim")

	// Send 50 login attempts with distinct garbage "emails" from distinct IPs.
	for i := 0; i < 50; i++ {
		garbage := strings.Repeat("x", 500) // way over 254 cap
		rr := doRequestFromRemoteAddr(srv, "POST", "/api/v1/auth/login", map[string]string{
			"email":    garbage,
			"password": "wrong",
		}, "203.0.113.200:"+string(rune('0'+i%10)))
		if rr.Code == http.StatusTooManyRequests {
			// Acceptable — could be hit by per-IP limiter depending on IP reuse.
			continue
		}
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("garbage email attempt %d: expected 401 or 429, got %d", i, rr.Code)
		}
	}

	// Confirm no bucket was created for the garbage: the map should be empty
	// of any entry whose key starts with 'x...' (the long string).
	srv.rateLimiters.AuthEmail.mu.Lock()
	defer srv.rateLimiters.AuthEmail.mu.Unlock()
	for key := range srv.rateLimiters.AuthEmail.limiters {
		if strings.HasPrefix(key, "xxxxxxx") {
			t.Fatalf("garbage email created bucket: %q (len=%d)", key[:20]+"…", len(key))
		}
	}
}

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
