package server

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/xarmian/pad/internal/models"
)

// dispatchWebhook fires a webhook event if a dispatcher is configured.
func (s *Server) dispatchWebhook(workspaceID, event string, data interface{}) {
	if s.webhooks == nil {
		return
	}
	s.webhooks.Dispatch(workspaceID, event, data)
}

// handleCreateWebhook registers a new webhook for a workspace.
func (s *Server) handleCreateWebhook(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	var input models.WebhookCreate
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if input.URL == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "url is required")
		return
	}

	hook, err := s.store.CreateWebhook(workspaceID, input)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, hook)
}

// handleListWebhooks returns all webhooks for a workspace.
func (s *Server) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	hooks, err := s.store.ListWebhooks(workspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if hooks == nil {
		hooks = []models.Webhook{}
	}

	writeJSON(w, http.StatusOK, hooks)
}

// handleDeleteWebhook removes a webhook by ID.
func (s *Server) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	_, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	webhookID := chi.URLParam(r, "webhookID")
	if err := s.store.DeleteWebhook(webhookID); err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "not_found", "Webhook not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleTestWebhook sends a test payload to the specified webhook.
func (s *Server) handleTestWebhook(w http.ResponseWriter, r *http.Request) {
	_, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	webhookID := chi.URLParam(r, "webhookID")
	hook, err := s.store.GetWebhook(webhookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if hook == nil {
		writeError(w, http.StatusNotFound, "not_found", "Webhook not found")
		return
	}

	if s.webhooks != nil {
		s.webhooks.Dispatch(hook.WorkspaceID, "webhook.test", map[string]interface{}{
			"message":    "This is a test webhook delivery from Pad",
			"webhook_id": hook.ID,
			"timestamp":  time.Now().UTC().Format(time.RFC3339),
		})
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "sent",
		"message": "Test payload dispatched",
	})
}
