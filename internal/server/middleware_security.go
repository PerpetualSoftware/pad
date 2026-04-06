package server

import (
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

		// CSP: allow self-sourced content, inline styles for Svelte component scoping,
		// and inline scripts for SvelteKit's module bootstrap/hydration.
		// Without 'unsafe-inline' on script-src, SvelteKit's generated inline <script>
		// tags are blocked, causing a white screen (especially on mobile browsers).
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-ancestors 'none'")

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

// parseCORSOrigins parses a comma-separated list of origins into a slice.
// Returns default localhost origins if the input is empty.
func parseCORSOrigins(origins string) []string {
	if origins == "" {
		return []string{"http://localhost:*", "http://127.0.0.1:*"}
	}

	var result []string
	for _, origin := range strings.Split(origins, ",") {
		origin = strings.TrimSpace(origin)
		if origin != "" {
			result = append(result, origin)
		}
	}
	if len(result) == 0 {
		return []string{"http://localhost:*", "http://127.0.0.1:*"}
	}
	return result
}
