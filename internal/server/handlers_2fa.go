package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/xarmian/pad/internal/models"
)

const (
	// totpIssuer is the issuer name shown in authenticator apps.
	totpIssuer = "Pad"

	// recoveryCodeCount is the number of recovery codes generated.
	recoveryCodeCount = 8
)

// handleTOTPSetup generates a TOTP secret and returns the provisioning URI
// for the user to scan with their authenticator app. The secret is stored
// but 2FA is not enabled until verified via /auth/2fa/verify.
func (s *Server) handleTOTPSetup(w http.ResponseWriter, r *http.Request) {
	if isAPITokenAuth(r) {
		writeError(w, http.StatusForbidden, "forbidden", "2FA management requires an interactive session, not an API token")
		return
	}

	user := currentUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Not logged in")
		return
	}

	if user.TOTPEnabled {
		writeError(w, http.StatusConflict, "conflict", "2FA is already enabled. Disable it first to reconfigure.")
		return
	}

	// Generate TOTP key
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      totpIssuer,
		AccountName: user.Email,
	})
	if err != nil {
		writeInternalError(w, fmt.Errorf("generate totp key: %w", err))
		return
	}

	// Store the secret (not yet enabled)
	if err := s.store.SetTOTPSecret(user.ID, key.Secret()); err != nil {
		writeInternalError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"secret": key.Secret(),
		"url":    key.URL(),
	})
}

// handleTOTPVerify verifies a TOTP code against the provided secret and
// enables 2FA if valid. The secret must match the one stored in the database
// (set during setup) to prevent TOCTOU races. Returns recovery codes.
func (s *Server) handleTOTPVerify(w http.ResponseWriter, r *http.Request) {
	if isAPITokenAuth(r) {
		writeError(w, http.StatusForbidden, "forbidden", "2FA management requires an interactive session, not an API token")
		return
	}

	user := currentUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Not logged in")
		return
	}

	if user.TOTPEnabled {
		writeError(w, http.StatusConflict, "conflict", "2FA is already enabled")
		return
	}

	var input struct {
		Code   string `json:"code"`
		Secret string `json:"secret"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	input.Code = strings.TrimSpace(input.Code)
	input.Secret = strings.TrimSpace(input.Secret)

	if input.Code == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "TOTP code is required")
		return
	}
	if input.Secret == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "Secret is required (from /auth/2fa/setup)")
		return
	}

	// Validate the code against the secret the client has
	if !totp.Validate(input.Code, input.Secret) {
		writeError(w, http.StatusUnauthorized, "invalid_code", "Invalid TOTP code. Please try again.")
		return
	}

	// Generate recovery codes (plaintext for the user, hashed for storage)
	codes, err := generateRecoveryCodes(recoveryCodeCount)
	if err != nil {
		writeInternalError(w, fmt.Errorf("generate recovery codes: %w", err))
		return
	}
	hashedCodes := hashRecoveryCodes(codes)

	// Atomically enable 2FA only if the DB secret still matches (prevents TOCTOU race)
	if err := s.store.EnableTOTP(user.ID, input.Secret, strings.Join(hashedCodes, "\n")); err != nil {
		writeError(w, http.StatusConflict, "conflict", "TOTP secret changed during verification. Please run setup again.")
		return
	}

	s.logAuditEvent(models.ActionTOTPEnabled, r, "")

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled":        true,
		"recovery_codes": codes,
	})
}

// handleTOTPDisable disables 2FA for the current user. Requires the
// current password for verification.
func (s *Server) handleTOTPDisable(w http.ResponseWriter, r *http.Request) {
	if isAPITokenAuth(r) {
		writeError(w, http.StatusForbidden, "forbidden", "2FA management requires an interactive session, not an API token")
		return
	}

	user := currentUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Not logged in")
		return
	}

	if !user.TOTPEnabled {
		writeError(w, http.StatusBadRequest, "bad_request", "2FA is not enabled")
		return
	}

	var input struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	if input.Password == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "Password is required to disable 2FA")
		return
	}

	// Verify password
	valid, err := s.store.ValidatePassword(user.Email, input.Password)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if valid == nil {
		time.Sleep(500 * time.Millisecond)
		writeError(w, http.StatusForbidden, "invalid_password", "Incorrect password")
		return
	}

	if err := s.store.DisableTOTP(user.ID); err != nil {
		writeInternalError(w, err)
		return
	}

	s.logAuditEvent(models.ActionTOTPDisabled, r, "")

	// Disabling 2FA weakens the account's auth surface — rotate every
	// other session the same way we do for password changes. Otherwise
	// a cookie captured while 2FA was on keeps its privileges after 2FA
	// comes off.
	token, ok := s.rotateSessionsAfterCredentialChange(w, r, user)
	if !ok {
		return
	}

	// Include the fresh token for Bearer-only callers (CLI/API).
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled": false,
		"token":   token,
	})
}

// handleTOTPLoginVerify completes a login that requires 2FA verification.
// Requires a valid challenge token (from the login response) plus a TOTP
// code or recovery code. The challenge token is HMAC-signed, IP-bound,
// and short-lived to prove the user already passed password verification.
func (s *Server) handleTOTPLoginVerify(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ChallengeToken string `json:"challenge_token"`
		Code           string `json:"code"`
		RecoveryCode   string `json:"recovery_code"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	if input.ChallengeToken == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "challenge_token is required")
		return
	}

	input.Code = strings.TrimSpace(input.Code)
	input.RecoveryCode = strings.TrimSpace(input.RecoveryCode)

	if input.Code == "" && input.RecoveryCode == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "A TOTP code or recovery code is required")
		return
	}

	// Validate the challenge token (proves password was verified, checks IP + expiry)
	userID, err := validateTwoFAChallenge(input.ChallengeToken, clientIP(r), s.twoFAChallengeSecret)
	if err != nil {
		time.Sleep(500 * time.Millisecond)
		writeError(w, http.StatusUnauthorized, "unauthorized", "Invalid or expired 2FA challenge")
		return
	}

	user, err := s.store.GetUser(userID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if user == nil || !user.TOTPEnabled {
		time.Sleep(500 * time.Millisecond)
		writeError(w, http.StatusUnauthorized, "unauthorized", "Invalid 2FA verification")
		return
	}

	verified := false

	// Try TOTP code first
	if input.Code != "" {
		if totp.Validate(input.Code, user.TOTPSecret) {
			verified = true
		}
	}

	// Try recovery code (hashed comparison) — rate-limited per challenge
	// token so a single captured challenge can't be used to grind through
	// the (small) recovery-code space before it expires. 6 tries is enough
	// for a legitimate user who mistypes a dash or two; anything more than
	// that is almost certainly automation.
	if !verified && input.RecoveryCode != "" {
		if s.rateLimiters != nil && s.rateLimiters.RecoveryCode != nil {
			// Key on a SHA-256 of the challenge token so the limiter map
			// never stores the raw HMAC token in-memory.
			h := sha256.Sum256([]byte(input.ChallengeToken))
			key := "rc:" + hex.EncodeToString(h[:])
			if !s.rateLimiters.RecoveryCode.getLimiter(key).Allow() {
				slog.Warn("rate limited", "user_id", user.ID, "limiter", "recovery_code")
				writeRateLimitResponse(w, s.rateLimiters.RecoveryCode.config)
				return
			}
		}
		// Normalize so users can type/paste codes however they were
		// displayed: generated codes are uppercase base32 [A-Z2-7] with
		// no separators, but mobile keyboards default to lowercase, and
		// some people paste codes wrapped with dashes or spaces. Strip
		// those and uppercase before hashing.
		normalized := normalizeRecoveryCode(input.RecoveryCode)
		consumed, err := s.store.ConsumeRecoveryCode(user.ID, normalized)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		verified = consumed
	}

	if !verified {
		time.Sleep(500 * time.Millisecond)
		writeError(w, http.StatusUnauthorized, "invalid_code", "Invalid verification code")
		return
	}

	// Create full session
	token, err := s.createAuthSession(w, r, user, webSessionTTL)
	if err != nil {
		return
	}

	s.logAuditEventForUser(models.ActionLogin, r, user.ID, auditMeta(map[string]string{"email": user.Email, "2fa": "verified"}))

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"user":  sessionUserPayload(user),
		"token": token,
	})
}

// normalizeRecoveryCode prepares a user-entered recovery code for
// hashed comparison against stored codes: strip ASCII whitespace and
// dashes, then uppercase the result. Generated codes are already
// uppercase base32, so normalization is a no-op for a correctly-typed
// code but rescues common formatting mistakes (mobile lowercase,
// pasted dashes, surrounding whitespace) that would otherwise burn a
// rate-limit slot.
func normalizeRecoveryCode(code string) string {
	var b strings.Builder
	b.Grow(len(code))
	for _, r := range code {
		switch {
		case r == '-' || r == ' ' || r == '\t' || r == '\n' || r == '\r':
			continue
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - ('a' - 'A'))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// generateRecoveryCodes produces n random recovery codes. Each code is 10
// bytes (80 bits) of cryptographic randomness encoded as unpadded base32
// — 16 visible characters of [A-Z2-7]. That's well above NIST SP 800-63B's
// 6-character minimum for backup authenticators and well above the ~32
// bits of the old 8-char hex codes, which a modern attacker could grind
// through online at a few thousand attempts per second.
func generateRecoveryCodes(n int) ([]string, error) {
	codes := make([]string, n)
	for i := 0; i < n; i++ {
		b := make([]byte, 10)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		// StdEncoding without padding gives a compact, user-readable code.
		// Base32 avoids ambiguity between 0/O and 1/I/l that would bite
		// if a user has to type the code from a printed backup.
		codes[i] = base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	}
	return codes, nil
}

// hashRecoveryCodes returns SHA-256 hex hashes of the given plaintext codes.
func hashRecoveryCodes(codes []string) []string {
	hashed := make([]string, len(codes))
	for i, c := range codes {
		h := sha256.Sum256([]byte(c))
		hashed[i] = hex.EncodeToString(h[:])
	}
	return hashed
}
