package server

import (
	"crypto/rand"
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

// handleTOTPVerify verifies a TOTP code against the stored secret and
// enables 2FA if valid. Returns recovery codes.
func (s *Server) handleTOTPVerify(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Not logged in")
		return
	}

	if user.TOTPEnabled {
		writeError(w, http.StatusConflict, "conflict", "2FA is already enabled")
		return
	}

	// Re-fetch user to get the latest secret (may have been set in setup)
	user, err := s.store.GetUser(user.ID)
	if err != nil || user == nil {
		writeInternalError(w, fmt.Errorf("fetch user: %w", err))
		return
	}

	if user.TOTPSecret == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Call /auth/2fa/setup first to generate a secret")
		return
	}

	var input struct {
		Code string `json:"code"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	input.Code = strings.TrimSpace(input.Code)
	if input.Code == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "TOTP code is required")
		return
	}

	// Validate the code
	if !totp.Validate(input.Code, user.TOTPSecret) {
		writeError(w, http.StatusUnauthorized, "invalid_code", "Invalid TOTP code. Please try again.")
		return
	}

	// Generate recovery codes
	codes, err := generateRecoveryCodes(recoveryCodeCount)
	if err != nil {
		writeInternalError(w, fmt.Errorf("generate recovery codes: %w", err))
		return
	}

	// Enable 2FA
	if err := s.store.EnableTOTP(user.ID, strings.Join(codes, "\n")); err != nil {
		writeInternalError(w, err)
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
// Accepts a TOTP code or a recovery code and returns a full session.
func (s *Server) handleTOTPLoginVerify(w http.ResponseWriter, r *http.Request) {
	var input struct {
		UserID       string `json:"user_id"`
		Code         string `json:"code"`
		RecoveryCode string `json:"recovery_code"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	if input.UserID == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "user_id is required")
		return
	}

	input.Code = strings.TrimSpace(input.Code)
	input.RecoveryCode = strings.TrimSpace(input.RecoveryCode)

	if input.Code == "" && input.RecoveryCode == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "A TOTP code or recovery code is required")
		return
	}

	user, err := s.store.GetUser(input.UserID)
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

	// Try recovery code
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
