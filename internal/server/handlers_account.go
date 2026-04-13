package server

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/xarmian/pad/internal/models"
	"golang.org/x/crypto/bcrypt"
)

// --- Account Deletion (GDPR Article 17 — Right to Erasure) ---

// handleDeleteAccount handles POST /api/v1/auth/delete-account.
// Requires password confirmation. Deletes the user and all owned data.
func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user == nil {
		user = s.validateSessionCookie(r)
	}
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Not logged in")
		return
	}

	var input struct {
		Password string `json:"password"`
		Confirm  bool   `json:"confirm"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	// Verify identity
	fullUser, err := s.store.GetUser(user.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	if input.Password != "" {
		// Password confirmation (normal flow)
		if err := bcrypt.CompareHashAndPassword([]byte(fullUser.PasswordHash), []byte(input.Password)); err != nil {
			writeError(w, http.StatusForbidden, "forbidden", "Incorrect password")
			return
		}
	} else if input.Confirm && s.cloudMode {
		// Cloud mode only: allow confirm-only deletion for OAuth-registered users
		// who never set a password. The session itself is the proof of identity.
		// In self-hosted mode, password is always required to prevent accidental
		// or coerced account deletion.
	} else {
		writeError(w, http.StatusBadRequest, "bad_request", "Password is required to delete your account")
		return
	}

	// Delete all owned workspaces
	workspaces, err := s.store.GetUserWorkspaces(user.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	for _, ws := range workspaces {
		if ws.OwnerID == user.ID {
			if err := s.store.DeleteWorkspace(ws.Slug); err != nil {
				slog.Error("delete account: failed to delete workspace", "workspace", ws.Slug, "error", err)
			}
		}
	}

	// Revoke all sessions
	_ = s.store.DeleteUserSessions(user.ID)

	// Delete the user
	if err := s.store.DeleteUser(user.ID); err != nil {
		writeInternalError(w, err)
		return
	}

	// Clear session cookie
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
	clearCSRFCookie(w)

	s.logAuditEventForUser(models.ActionAccountDeleted, r, user.ID, auditMeta(map[string]string{
		"email": user.Email,
	}))

	slog.Info("account deleted", "user_id", user.ID, "email", user.Email)
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// --- Data Export (GDPR Article 20 — Right to Portability) ---

// handleExportAccount handles GET /api/v1/auth/export.
// Returns all user data as a JSON object.
func (s *Server) handleExportAccount(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user == nil {
		user = s.validateSessionCookie(r)
	}
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Not logged in")
		return
	}

	// Collect all user data
	export := map[string]interface{}{
		"user": map[string]interface{}{
			"id":           user.ID,
			"email":        user.Email,
			"username":     user.Username,
			"name":         user.Name,
			"role":         user.Role,
			"plan":         user.Plan,
			"totp_enabled": user.TOTPEnabled,
			"created_at":   user.CreatedAt,
			"updated_at":   user.UpdatedAt,
		},
	}

	// Export owned workspaces with all items
	workspaces, err := s.store.GetUserWorkspaces(user.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	var wsExports []interface{}
	for _, ws := range workspaces {
		wsData := map[string]interface{}{
			"id":         ws.ID,
			"name":       ws.Name,
			"slug":       ws.Slug,
			"role":       "owner",
			"created_at": ws.CreatedAt,
		}

		// Only export full data for owned workspaces
		if ws.OwnerID == user.ID {
			// Get collections
			collections, _ := s.store.ListCollections(ws.ID)
			wsData["collections"] = collections

			// Get all items
			items, _ := s.store.ListItems(ws.ID, models.ItemListParams{IncludeArchived: true})
			wsData["items"] = items
		}

		wsExports = append(wsExports, wsData)
	}
	export["workspaces"] = wsExports

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", "attachment; filename=\"pad-export.json\"")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(export)
}
