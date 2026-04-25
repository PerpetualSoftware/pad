package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSecurityHeaders(t *testing.T) {
	srv := testServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	headers := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
		"Permissions-Policy":     "camera=(), microphone=(), geolocation=()",
	}

	for name, expected := range headers {
		got := w.Header().Get(name)
		if got != expected {
			t.Errorf("%s = %q, want %q", name, got, expected)
		}
	}

	// CSP should be set
	csp := w.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Error("Content-Security-Policy header not set")
	}

	// HSTS should NOT be set when secureCookies is false (default)
	if hsts := w.Header().Get("Strict-Transport-Security"); hsts != "" {
		t.Errorf("HSTS should not be set when secureCookies is off, got %q", hsts)
	}
}

func TestParseCORSOrigins(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"", []string{"http://localhost:*", "http://127.0.0.1:*"}},
		{"https://app.pad.dev", []string{"https://app.pad.dev"}},
		{"https://app.pad.dev, https://admin.pad.dev", []string{"https://app.pad.dev", "https://admin.pad.dev"}},
		{"  , ", []string{"http://localhost:*", "http://127.0.0.1:*"}}, // empty after trim
		// TASK-664: '*' is incompatible with AllowCredentials=true and must
		// be dropped. When it was the ONLY configured origin, parseCORSOrigins
		// falls back to localhost defaults rather than producing an empty list.
		{"*", []string{"http://localhost:*", "http://127.0.0.1:*"}},
		{"https://app.pad.dev, *", []string{"https://app.pad.dev"}},
		{"*, https://admin.pad.dev", []string{"https://admin.pad.dev"}},
	}

	for _, tt := range tests {
		got := parseCORSOrigins(tt.input)
		if len(got) != len(tt.want) {
			t.Errorf("parseCORSOrigins(%q) = %v, want %v", tt.input, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("parseCORSOrigins(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
			}
		}
	}
}

func TestCorsAllowCredentials(t *testing.T) {
	// When PAD_CORS_ORIGINS is empty (or whitespace-only), AllowCredentials
	// defaults to false. When an operator sets any non-empty value they
	// opt into credential sharing across the listed origins. We don't
	// second-guess a typo'd comma-only value — a stricter check should
	// happen at parseCORSOrigins time.
	tests := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"   ", false},
		{"\t", false},
		{"https://app.pad.dev", true},
	}
	for _, tt := range tests {
		if got := corsAllowCredentials(tt.in); got != tt.want {
			t.Errorf("corsAllowCredentials(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
