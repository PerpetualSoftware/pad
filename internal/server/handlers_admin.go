package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Known platform setting keys. Values are stored in the platform_settings table.
const (
	settingEmailProvider  = "email_provider" // "maileroo" or empty
	settingMailerooAPIKey = "maileroo_api_key"
	settingEmailFrom      = "email_from"      // Sender address
	settingEmailFromName  = "email_from_name" // Sender display name
	settingPlatformName   = "platform_name"   // Instance name (default: "Pad")

	// Token policy settings
	settingTokenDefaultExpiryDays = "token_default_expiry_days" // Default: 90
	settingTokenMaxLifetimeDays   = "token_max_lifetime_days"   // Default: 0 (no limit)

	// WebMCP opt-in. "true" enables the browser-side WebMCP surface; any other
	// value (including unset) means disabled. Default off (PLAN-1888 DR-6).
	settingWebMCPEnabled = "webmcp_enabled"
)

// adminManagedSettings is the canonical allowlist of admin-editable platform
// settings. It gates BOTH the read (handleGetPlatformSettings) and the write
// (handleUpdatePlatformSettings) so the two can't drift.
//
// Deny-by-default is load-bearing for security: the platform_settings table also
// holds server secrets (2fa_challenge_secret — the 2FA challenge HMAC key) and
// plan-limit rows (plan_limits_*). Returning the whole table leaked the 2FA
// secret to any admin client (BUG-1909). GET emits only these keys; a value that
// is settable but sensitive (maileroo_api_key) is additionally masked. Any key
// not listed here is neither exposed nor writable through this endpoint.
var adminManagedSettings = map[string]bool{
	settingEmailProvider:          true,
	settingMailerooAPIKey:         true,
	settingEmailFrom:              true,
	settingEmailFromName:          true,
	settingPlatformName:           true,
	settingTokenDefaultExpiryDays: true,
	settingTokenMaxLifetimeDays:   true,
	settingWebMCPEnabled:          true,
}

// handleGetPlatformSettings returns the admin-managed platform settings.
// Admin-only. Only allowlisted keys are returned (deny-by-default — see
// adminManagedSettings); sensitive values (API keys) are masked.
func (s *Server) handleGetPlatformSettings(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user == nil || user.Role != "admin" {
		writeError(w, http.StatusForbidden, "forbidden", "Admin access required")
		return
	}

	stored, err := s.store.GetPlatformSettings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to load settings")
		return
	}

	// Project only the admin-managed keys — never the 2FA secret or plan-limit
	// rows that also live in platform_settings (BUG-1909).
	settings := make(map[string]string, len(adminManagedSettings))
	for key := range adminManagedSettings {
		if v, ok := stored[key]; ok {
			settings[key] = v
		}
	}

	// Surface webmcp_enabled with an explicit default so the admin UI toggle
	// always has a value to bind to on a fresh instance (PLAN-1888 DR-6).
	if _, ok := settings[settingWebMCPEnabled]; !ok {
		settings[settingWebMCPEnabled] = "false"
	}

	// Mask sensitive values
	if key, ok := settings[settingMailerooAPIKey]; ok && key != "" {
		settings[settingMailerooAPIKey] = maskAPIKey(key)
	}

	writeJSON(w, http.StatusOK, settings)
}

// maskAPIKey returns the display form of a sensitive setting value. Keys longer
// than 8 chars show their first and last 4 chars (abcd...wxyz); shorter non-empty
// keys collapse to "****". Empty stays empty. This is the single source of truth
// for the mask format — handleGetPlatformSettings emits it and
// handleUpdatePlatformSettings uses it to detect an unchanged (echoed-back) key.
func maskAPIKey(key string) string {
	if len(key) > 8 {
		return key[:4] + "..." + key[len(key)-4:]
	}
	if key != "" {
		return "****"
	}
	return ""
}

// handleUpdatePlatformSettings updates one or more platform settings.
// Admin-only. Accepts a JSON object of key-value pairs.
func (s *Server) handleUpdatePlatformSettings(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user == nil || user.Role != "admin" {
		writeError(w, http.StatusForbidden, "forbidden", "Admin access required")
		return
	}

	var input map[string]string
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	// Whitelist known settings — same canonical set the GET handler exposes, so a
	// caller can neither read nor write server secrets (2fa_challenge_secret) or
	// plan-limit rows through this endpoint (BUG-1909 / BUG-1890).
	for key, value := range input {
		if !adminManagedSettings[key] {
			continue
		}
		// Defensive guard against BUG-1890: the GET handler returns the API key
		// masked (abcd...wxyz / ****). If a client echoes that mask back unchanged
		// (e.g. an admin saving the email form without re-typing the key), persisting
		// it would corrupt the real key and silently break email. Treat an incoming
		// value that equals the mask of the stored key as "unchanged" and skip it.
		//
		// This is a best-effort backstop; the authoritative fix is client-side (the
		// web form omits the key unless the admin actually edits it). It recognizes
		// the mask of the *current* key, so a mask captured before a concurrent key
		// change by another admin wouldn't be caught — an accepted, negligible gap
		// for the backstop given the client never sends the mask on a no-op save.
		if key == settingMailerooAPIKey && value != "" {
			current, err := s.store.GetPlatformSetting(settingMailerooAPIKey)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal_error", "Failed to load setting: "+key)
				return
			}
			if current != "" && value == maskAPIKey(current) {
				continue
			}
		}
		if err := s.store.SetPlatformSetting(key, value); err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to save setting: "+key)
			return
		}
	}

	// Reconfigure email sender if email settings changed
	s.reconfigureEmail()

	// Log which settings were changed (keys only, not values for security)
	var keys []string
	for key := range input {
		if adminManagedSettings[key] {
			keys = append(keys, key)
		}
	}
	keysJSON, _ := json.Marshal(keys)
	s.logAuditEvent(models.ActionSettingsChanged, r, fmt.Sprintf(`{"keys":%s}`, keysJSON))

	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// handleTestEmail sends a test email to verify the email configuration.
// Admin-only.
func (s *Server) handleTestEmail(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user == nil || user.Role != "admin" {
		writeError(w, http.StatusForbidden, "forbidden", "Admin access required")
		return
	}

	if s.email == nil {
		writeError(w, http.StatusBadRequest, "email_not_configured", "Email is not configured. Set the API key and sender address first.")
		return
	}

	var input struct {
		To string `json:"to"`
	}
	if err := decodeJSON(r, &input); err != nil || input.To == "" {
		// Default to the admin's own email
		input.To = user.Email
	}

	if err := s.email.SendTest(r.Context(), input.To); err != nil {
		writeError(w, http.StatusInternalServerError, "email_failed", "Failed to send test email: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":      true,
		"sent_to": input.To,
	})
}
