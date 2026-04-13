package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, unsubPage("Invalid Link", "This unsubscribe link is missing required parameters."))
		return
	}

	secret := s.unsubscribeSecret()
	if secret == "" || !email.ValidateUnsubscribeToken(emailAddr, token, secret) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, unsubPage("Invalid Link", "This unsubscribe link is invalid or has expired."))
		return
	}

	if err := s.store.OptOutEmail(emailAddr); err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, unsubPage("Error", "Something went wrong. Please try again later."))
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, unsubPage("Unsubscribed",
		fmt.Sprintf("You've been unsubscribed. <strong>%s</strong> will no longer receive non-transactional emails from this Pad instance.", emailAddr)))
}

func unsubPage(title, message string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s — Pad</title>
</head>
<body style="font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; color: #1a1a1a; max-width: 480px; margin: 80px auto; padding: 0 20px; text-align: center;">
  <div style="margin-bottom: 24px;">
    <strong style="font-size: 18px;">Pad</strong>
  </div>
  <h1 style="font-size: 22px; font-weight: 600; margin-bottom: 12px;">%s</h1>
  <p style="font-size: 15px; line-height: 1.5; color: #444;">%s</p>
</body>
</html>`, title, title, message)
}
