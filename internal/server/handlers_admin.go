package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/xarmian/pad/internal/models"
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
)

// handleGetPlatformSettings returns all platform settings.
// Admin-only. Sensitive values (API keys) are masked.
func (s *Server) handleGetPlatformSettings(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user == nil || user.Role != "admin" {
		writeError(w, http.StatusForbidden, "forbidden", "Admin access required")
		return
	}

	settings, err := s.store.GetPlatformSettings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to load settings")
		return
	}

	// Mask sensitive values
	if key, ok := settings[settingMailerooAPIKey]; ok && len(key) > 8 {
		settings[settingMailerooAPIKey] = key[:4] + "..." + key[len(key)-4:]
	} else if ok && key != "" {
		settings[settingMailerooAPIKey] = "****"
	}

	writeJSON(w, http.StatusOK, settings)
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

	// Whitelist known settings
	allowed := map[string]bool{
		settingEmailProvider:          true,
		settingMailerooAPIKey:         true,
		settingEmailFrom:              true,
		settingEmailFromName:          true,
		settingPlatformName:           true,
		settingTokenDefaultExpiryDays: true,
		settingTokenMaxLifetimeDays:   true,
	}

	for key, value := range input {
		if !allowed[key] {
			continue
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
		if allowed[key] {
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
