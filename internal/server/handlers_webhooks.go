package server

import (
	"database/sql"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/webhooks"
)

// maskWebhookSecret blanks the plaintext HMAC secret before a webhook is
// returned in a list/get response, so the raw signing secret is never echoed
// after creation (BUG-2057). The has_secret flag still signals whether one is
// configured. Returns a copy — the caller's slice/model is left untouched.
func maskWebhookSecret(hook models.Webhook) models.Webhook {
	hook.Secret = ""
	return hook
}

// dispatchWebhook fires a webhook event if a dispatcher is configured.
func (s *Server) dispatchWebhook(workspaceID, event string, data interface{}) {
	if s.webhooks == nil {
		return
	}
	s.webhooks.Dispatch(workspaceID, event, data)
}

// handleCreateWebhook registers a new webhook for a workspace.
func (s *Server) handleCreateWebhook(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "owner") {
		return
	}
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	// Enforce webhook count limit (workspace-scoped)
	if !s.enforcePlanLimit(w, workspaceID, "webhooks") {
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

	// "enc:" is the reserved marker the store uses to tag encrypted secrets at
	// rest (see internal/store/encryption.go::encryptedPrefix). Reject a raw
	// secret that starts with it so a user-supplied plaintext can never be
	// mistaken for ciphertext on read (which would break signing/dispatch,
	// including on keyless self-host instances).
	if strings.HasPrefix(input.Secret, "enc:") {
		writeError(w, http.StatusBadRequest, "bad_request", "secret must not start with the reserved prefix \"enc:\"")
		return
	}

	// Validate URL to prevent SSRF attacks
	if err := webhooks.ValidateWebhookURL(input.URL); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid webhook URL: "+err.Error())
		return
	}

	hook, err := s.store.CreateWebhook(workspaceID, input)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// The creation response is the ONLY place the raw secret is returned, so
	// the caller can record it for HMAC verification. It is masked everywhere
	// else (BUG-2057).
	writeJSON(w, http.StatusCreated, hook)
}

// handleListWebhooks returns all webhooks for a workspace.
// Restricted to owners since webhook URLs may contain secret tokens.
func (s *Server) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "owner") {
		return
	}
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	hooks, err := s.store.ListWebhooks(workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if hooks == nil {
		hooks = []models.Webhook{}
	}

	// Never echo the raw signing secret in a list response (BUG-2057).
	masked := make([]models.Webhook, len(hooks))
	for i, hook := range hooks {
		masked[i] = maskWebhookSecret(hook)
	}

	writeJSON(w, http.StatusOK, masked)
}

// handleDeleteWebhook removes a webhook by ID.
func (s *Server) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "owner") {
		return
	}
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
		writeInternalError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleTestWebhook sends a test payload to the specified webhook.
func (s *Server) handleTestWebhook(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "owner") {
		return
	}
	_, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	webhookID := chi.URLParam(r, "webhookID")
	hook, err := s.store.GetWebhook(webhookID)
	if err != nil {
		writeInternalError(w, err)
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
