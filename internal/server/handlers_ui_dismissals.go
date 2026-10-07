package server

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// The caller's dismissed one-time UI suggestions (TASK-3452): the first-time
// tutorial cards. Stored on the user so a dismissal holds on every device.
// Keys are a closed set (models.UIDismissalKeys). Web client only: no CLI or
// MCP surface.

// handleListUIDismissals: GET /me/ui-dismissals → {dismissed: [...]}.
func (s *Server) handleListUIDismissals(w http.ResponseWriter, r *http.Request) {
	userID := currentUserID(r)
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	}
	keys, err := s.store.ListUIDismissals(userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
			return
		}
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dismissed": keys})
}

// handleDismissUI: PUT /me/ui-dismissals/{key} → {dismissed: [...]}, the set
// as this write left it. Idempotent. A key outside the allow-list is 400.
func (s *Server) handleDismissUI(w http.ResponseWriter, r *http.Request) {
	userID := currentUserID(r)
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	}
	key := chi.URLParam(r, "key")
	if !models.IsUIDismissalKey(key) {
		writeError(w, http.StatusBadRequest, "bad_request", "Unknown dismissal key")
		return
	}
	keys, err := s.store.DismissUI(userID, key)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrUnknownUIDismissal):
			writeError(w, http.StatusBadRequest, "bad_request", "Unknown dismissal key")
		case errors.Is(err, sql.ErrNoRows):
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		default:
			writeInternalError(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dismissed": keys})
}
