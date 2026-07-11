package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"github.com/PerpetualSoftware/pad/internal/models"
)

const (
	// totpIssuer is the issuer name shown in authenticator apps.
	totpIssuer = "Pad"

	// recoveryCodeCount is the number of recovery codes generated.
	recoveryCodeCount = 8

	// totpPeriod is the TOTP time-step length in seconds. Matches the period
	// baked into totp.Validate (Google-Authenticator compatible) and is used
	// to derive the step a code matched for single-use enforcement (BUG-2054).
	totpPeriod = 30
)

// deriveTOTPStep returns the TOTP time-step whose generated code equals
// passcode, searching the same ±1 skew window totp.Validate accepts (in the
// library's search order: current, +1, -1). The bool is false if no step in
// the window matches — which shouldn't happen once totp.Validate has already
// returned true, but if it does the caller MUST refuse rather than risk
// accepting a replay it can't pin to a step. See BUG-2054.
func deriveTOTPStep(passcode, secret string, t time.Time) (int64, bool) {
	current := t.Unix() / totpPeriod
	for _, step := range []int64{current, current + 1, current - 1} {
		code, err := totp.GenerateCodeCustom(secret, time.Unix(step*totpPeriod, 0).UTC(), totp.ValidateOpts{
			Period:    totpPeriod,
			Digits:    otp.DigitsSix,
			Algorithm: otp.AlgorithmSHA1,
		})
		if err != nil {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(code), []byte(passcode)) == 1 {
			return step, true
		}
	}
	return 0, false
}

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
		// Per-challenge attempt cap on the TOTP branch, mirroring the
		// recovery-code branch below: a captured challenge token shouldn't be
		// a license to grind the 6-digit space before it expires. Reuses the
		// existing RecoveryCode limiter (keyed on a SHA-256 of the challenge
		// token, distinct "totp:" prefix) rather than adding a new limiter.
		if s.rateLimiters != nil && s.rateLimiters.RecoveryCode != nil {
			h := sha256.Sum256([]byte(input.ChallengeToken))
			key := "totp:" + hex.EncodeToString(h[:])
			if !s.rateLimiters.RecoveryCode.getLimiter(key).Allow() {
				slog.Warn("rate limited", "user_id", user.ID, "limiter", "totp")
				writeRateLimitResponse(w, s.rateLimiters.RecoveryCode.config)
				return
			}
		}
		// Capture the clock ONCE so validation and step derivation agree on the
		// window. totp.Validate reads its own time.Now(); if a boundary crosses
		// between it and deriveTOTPStep, a code valid under one clock could
		// derive to a step outside the other's ±1 window and be spuriously
		// rejected. ValidateCustom with the same `now` + the opts Validate bakes
		// in (period 30, skew 1, SHA1, 6 digits) closes that race.
		now := time.Now().UTC()
		valid, _ := totp.ValidateCustom(input.Code, user.TOTPSecret, now, totp.ValidateOpts{
			Period:    totpPeriod,
			Skew:      1,
			Digits:    otp.DigitsSix,
			Algorithm: otp.AlgorithmSHA1,
		})
		if valid {
			// Single-use enforcement (BUG-2054): a valid TOTP code is otherwise
			// replayable within its ~30s window (plus skew). Derive the step
			// the code actually matched and atomically claim it; a step already
			// consumed means this is a replay, so leave verified=false and let
			// it fall through to the same invalid-code response (no replay
			// signal is leaked to the caller).
			if step, ok := deriveTOTPStep(input.Code, user.TOTPSecret, now); ok {
				consumed, err := s.store.ConsumeTOTPStep(user.ID, user.TOTPSecret, step)
				if err != nil {
					writeInternalError(w, err)
					return
				}
				verified = consumed
			}
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
		// displayed: generated codes (post-TASK-658) are uppercase
		// base32 [A-Z2-7] with no separators, but mobile keyboards
		// default to lowercase, and some people paste codes wrapped
		// with dashes or spaces. Strip those and uppercase before
		// hashing.
		normalized := normalizeRecoveryCode(input.RecoveryCode)
		consumed, err := s.store.ConsumeRecoveryCode(user.ID, normalized)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if !consumed {
			// Legacy-code fallback: pre-TASK-658 codes were generated
			// with hex.EncodeToString (lowercase). Uppercasing them as
			// part of normalization changes the SHA-256 and locks out
			// users whose stored hash is of the lowercase form. Retry
			// with the whitespace-trimmed raw input (no case change)
			// so the original lowercase hex still validates. Doesn't
			// cost an extra rate-limit slot — the Allow() above has
			// already charged this attempt.
			trimmed := strings.TrimSpace(input.RecoveryCode)
			if trimmed != "" && trimmed != normalized {
				consumed, err = s.store.ConsumeRecoveryCode(user.ID, trimmed)
				if err != nil {
					writeInternalError(w, err)
					return
				}
			}
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
