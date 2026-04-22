package server

import (
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"net/http"
	"strings"
)

// SecurityHeaders adds standard security headers to all responses.
// These protect against common web vulnerabilities like XSS, clickjacking,
// and MIME type sniffing.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()

		// Prevent the browser from MIME-sniffing the content type
		h.Set("X-Content-Type-Options", "nosniff")

		// Prevent the page from being embedded in frames (clickjacking protection)
		h.Set("X-Frame-Options", "DENY")

		// Control referrer information sent with requests
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")

		// Restrict browser features the app doesn't need
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")

		// CSP: strict policy for API responses. HTML pages served by spaHandler
		// override this with a nonce-based script-src for SvelteKit inline scripts.
		//
		// script-src-attr 'none' blocks inline event handlers (onerror=, onclick=,
		// onload=, …). Those bypass script-src per the CSP spec, so without this
		// directive an attacker who slips markup past the sanitizer can still
		// execute JS via event attributes. Defense-in-depth for the comment-XSS
		// fix (TASK-647) and for any future sanitizer regression.
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; script-src-attr 'none'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-ancestors 'none'")

		next.ServeHTTP(w, r)
	})
}

// StrictTransportSecurity adds HSTS header when secure cookies are enabled
// (indicating the server is behind TLS).
func StrictTransportSecurity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		next.ServeHTTP(w, r)
	})
}

// generateCSPNonce generates a cryptographically random nonce for
// Content-Security-Policy headers. Returns a 16-byte base64-encoded string.
func generateCSPNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

// parseCORSOrigins parses a comma-separated list of origins into a slice.
// Returns default localhost origins if the input is empty.
//
// The '*' wildcard is explicitly dropped (with a log warning): the CORS
// middleware is configured with AllowCredentials=true, and per the Fetch
// spec browsers refuse to honor `Access-Control-Allow-Origin: *` when
// credentials are being sent. Rejecting wildcards here prevents an
// operator misconfiguration (e.g. `PAD_CORS_ORIGINS=*`) from looking
// like it works in curl but failing silently in every real browser.
func parseCORSOrigins(origins string) []string {
	if origins == "" {
		return []string{"http://localhost:*", "http://127.0.0.1:*"}
	}

	var result []string
	for _, origin := range strings.Split(origins, ",") {
		origin = strings.TrimSpace(origin)
		if origin == "" {
			continue
		}
		if origin == "*" {
			slog.Warn("PAD_CORS_ORIGINS: dropping '*' — incompatible with AllowCredentials=true",
				"hint", "list specific origins instead, e.g. https://pad.example.com")
			continue
		}
		result = append(result, origin)
	}
	if len(result) == 0 {
		return []string{"http://localhost:*", "http://127.0.0.1:*"}
	}
	return result
}

// corsAllowCredentials reports whether the CORS middleware should set
// Access-Control-Allow-Credentials: true. The current CLI tooling uses
// Bearer tokens rather than cookies for cross-origin calls, so when no
// operator-configured origins are present we default to false — keeping
// a browser on a different origin from piggy-backing user cookies on
// its fetches. When operators provide explicit origins they opt into
// credential sharing.
func corsAllowCredentials(corsOrigins string) bool {
	return strings.TrimSpace(corsOrigins) != ""
}
