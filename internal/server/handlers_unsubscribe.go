package server

import (
	"crypto/sha256"
	"encoding/hex"
	"html/template"
	"log/slog"
	"net/http"
	"strings"

	"github.com/xarmian/pad/internal/email"
)

// unsubscribeSecret derives a stable HMAC key from the Maileroo API key.
// This avoids needing a separate secret config — if the API key changes,
// old unsubscribe links stop working, which is an acceptable trade-off.
func (s *Server) unsubscribeSecret() string {
	if s.email == nil {
		return ""
	}
	h := sha256.Sum256([]byte("pad-unsubscribe:" + s.emailAPIKey))
	return hex.EncodeToString(h[:])
}

// handleUnsubscribe processes GET /api/v1/unsubscribe?email=x&token=y
// This endpoint is unauthenticated — anyone with a valid HMAC token can opt out.
func (s *Server) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	emailAddr := strings.TrimSpace(r.URL.Query().Get("email"))
	token := r.URL.Query().Get("token")

	if emailAddr == "" || token == "" {
		renderUnsubPage(w, http.StatusBadRequest, unsubData{
			Title:   "Invalid Link",
			Message: "This unsubscribe link is missing required parameters.",
		})
		return
	}

	secret := s.unsubscribeSecret()
	if secret == "" || !email.ValidateUnsubscribeToken(emailAddr, token, secret) {
		renderUnsubPage(w, http.StatusBadRequest, unsubData{
			Title:   "Invalid Link",
			Message: "This unsubscribe link is invalid or has expired.",
		})
		return
	}

	if err := s.store.OptOutEmail(emailAddr); err != nil {
		renderUnsubPage(w, http.StatusInternalServerError, unsubData{
			Title:   "Error",
			Message: "Something went wrong. Please try again later.",
		})
		return
	}

	renderUnsubPage(w, http.StatusOK, unsubData{
		Title: "Unsubscribed",
		// The template autoescapes Email before it reaches the rendered
		// HTML, so even a malicious address like `"><script>alert(1)</script>`
		// is neutered. The <strong> wrapping is applied in the template,
		// not here — never again interpolate user input into a format
		// string that composes HTML.
		Message:          "You've been unsubscribed.",
		ShowEmailMessage: true,
		Email:            emailAddr,
	})
}

type unsubData struct {
	Title            string
	Message          string
	ShowEmailMessage bool
	Email            string
}

// unsubPageTemplate is compiled once at init time. html/template auto-
// escapes any value interpolated with {{.Field}}, so user-controlled
// fields cannot inject markup or scripts into the rendered page.
var unsubPageTemplate = template.Must(template.New("unsub").Parse(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} — Pad</title>
</head>
<body style="font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; color: #1a1a1a; max-width: 480px; margin: 80px auto; padding: 0 20px; text-align: center;">
  <div style="margin-bottom: 24px;">
    <strong style="font-size: 18px;">Pad</strong>
  </div>
  <h1 style="font-size: 22px; font-weight: 600; margin-bottom: 12px;">{{.Title}}</h1>
  <p style="font-size: 15px; line-height: 1.5; color: #444;">
    {{.Message}}{{if .ShowEmailMessage}} <strong>{{.Email}}</strong> will no longer receive non-transactional emails from this Pad instance.{{end}}
  </p>
</body>
</html>`))

// renderUnsubPage writes the unsubscribe status page with a strict CSP.
// The CSP denies scripts, inline event handlers, iframes, and remote
// resources outright — this page only needs its own inline styles, and
// the HTML-escaped output already covers most XSS. CSP is defense in
// depth for any future regression in the email validation / template.
func renderUnsubPage(w http.ResponseWriter, status int, data unsubData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; script-src 'none'; script-src-attr 'none'; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if err := unsubPageTemplate.Execute(w, data); err != nil {
		// Response already committed — best we can do is log.
		slog.Error("unsubscribe template render failed", "error", err)
	}
}
