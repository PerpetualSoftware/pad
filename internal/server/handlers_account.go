package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

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

	// Delete all owned workspaces, sessions, and the user atomically.
	// If any workspace deletion fails, the entire operation is aborted.
	workspaces, err := s.store.GetUserWorkspaces(user.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	var ownedSlugs []string
	for _, ws := range workspaces {
		if ws.OwnerID == user.ID {
			ownedSlugs = append(ownedSlugs, ws.Slug)
		}
	}

	if err := s.store.DeleteAccountAtomic(user.ID, ownedSlugs); err != nil {
		slog.Error("delete account: atomic deletion failed", "user_id", user.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"Account deletion failed. No data was removed. Please try again or contact support.")
		return
	}

	// Clear session cookie
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName(s.secureCookies),
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
// Streams user data as JSON, processing one workspace at a time to avoid
// loading everything into memory. Enforces a 60-second timeout.
func (s *Server) handleExportAccount(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user == nil {
		user = s.validateSessionCookie(r)
	}
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Not logged in")
		return
	}

	// Enforce a 60-second timeout for the entire export
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	// Prefetch workspace list (small) before starting the streaming response
	workspaces, err := s.store.GetUserWorkspaces(user.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// Stream the response — once we start writing, we can't send error status codes
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", "attachment; filename=\"pad-export.json\"")
	w.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(w)

	// Write opening structure
	w.Write([]byte("{\n  \"user\": "))
	enc.Encode(map[string]interface{}{
		"id":           user.ID,
		"email":        user.Email,
		"username":     user.Username,
		"name":         user.Name,
		"role":         user.Role,
		"plan":         user.Plan,
		"totp_enabled": user.TOTPEnabled,
		"created_at":   user.CreatedAt,
		"updated_at":   user.UpdatedAt,
	})

	w.Write([]byte(",\n  \"workspaces\": [\n"))

	for i, ws := range workspaces {
		// Check timeout between workspaces
		if ctx.Err() != nil {
			slog.Warn("export timeout", "user_id", user.ID, "workspaces_exported", i)
			break
		}

		if i > 0 {
			w.Write([]byte(",\n"))
		}

		wsData := map[string]interface{}{
			"id":         ws.ID,
			"name":       ws.Name,
			"slug":       ws.Slug,
			"role":       "owner",
			"created_at": ws.CreatedAt,
		}

		// Only export full data for owned workspaces
		if ws.OwnerID == user.ID {
			collections, _ := s.store.ListCollections(ws.ID)
			wsData["collections"] = collections

			// Stream items per workspace (each workspace loaded individually, then GC'd)
			items, err := s.store.ListItems(ws.ID, models.ItemListParams{IncludeArchived: true})
			if err != nil {
				slog.Error("export: failed to list items", "workspace", ws.Slug, "error", err)
				wsData["items"] = []interface{}{}
				wsData["export_error"] = "failed to export items"
			} else {
				wsData["items"] = items
			}
		}

		w.Write([]byte("    "))
		enc.Encode(wsData)

		// Flush after each workspace to free memory and show progress
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	w.Write([]byte("\n  ]\n}\n"))

	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
