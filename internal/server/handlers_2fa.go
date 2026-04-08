package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled": false,
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

	// Try recovery code (hashed comparison)
	if !verified && input.RecoveryCode != "" {
		consumed, err := s.store.ConsumeRecoveryCode(user.ID, input.RecoveryCode)
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

// generateRecoveryCodes produces n random recovery codes (8-char hex each).
func generateRecoveryCodes(n int) ([]string, error) {
	codes := make([]string, n)
	for i := 0; i < n; i++ {
		b := make([]byte, 4)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		codes[i] = hex.EncodeToString(b)
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
