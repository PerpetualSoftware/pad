package webhooks

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/xarmian/pad/internal/models"
)

// WebhookStore is the interface the dispatcher needs to fetch webhooks
// and record delivery outcomes.
type WebhookStore interface {
	ListWebhooks(workspaceID string) ([]models.Webhook, error)
	UpdateWebhookFailure(id string, failed bool) error
}

// WebhookPayload is the JSON body sent to each webhook endpoint.
type WebhookPayload struct {
	Event     string      `json:"event"`
	Workspace string      `json:"workspace"`
	Timestamp string      `json:"timestamp"`
	Data      interface{} `json:"data"`
}

// Dispatcher sends webhook HTTP POST notifications for workspace events.
type Dispatcher struct {
	store        WebhookStore
	client       *http.Client
	SkipSSRF     bool // Skip SSRF validation (for tests only)
}

// NewDispatcher creates a Dispatcher with the given store.
func NewDispatcher(store WebhookStore) *Dispatcher {
	return &Dispatcher{
		store: store,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// Dispatch sends the event payload to all matching active webhooks for the workspace.
// Each delivery runs in its own goroutine so the caller is never blocked.
func (d *Dispatcher) Dispatch(workspaceID, event string, data interface{}) {
	hooks, err := d.store.ListWebhooks(workspaceID)
	if err != nil {
		slog.Error("failed to list webhooks", "workspace", workspaceID, "error", err)
		return
	}

	payload := WebhookPayload{
		Event:     event,
		Workspace: workspaceID,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Data:      data,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		slog.Error("failed to marshal webhook payload", "error", err)
		return
	}

	for _, hook := range hooks {
		if !hook.Active {
			continue
		}
		if !matchesEvent(hook.Events, event) {
			continue
		}
		go d.deliver(hook, body)
	}
}

// deliver sends a single HTTP POST to the webhook URL.
func (d *Dispatcher) deliver(hook models.Webhook, body []byte) {
	// Defense in depth: re-validate URL before making the request
	if !d.SkipSSRF {
		if err := ValidateWebhookURL(hook.URL); err != nil {
			slog.Warn("blocked webhook delivery", "url", hook.URL, "error", err)
			d.store.UpdateWebhookFailure(hook.ID, true)
			return
		}
	}

	req, err := http.NewRequest(http.MethodPost, hook.URL, bytes.NewReader(body))
	if err != nil {
		slog.Error("failed to create webhook request", "url", hook.URL, "error", err)
		d.store.UpdateWebhookFailure(hook.ID, true)
		return
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Pad-Webhook/1.0")

	if hook.Secret != "" {
		sig := computeHMAC(body, []byte(hook.Secret))
		req.Header.Set("X-Pad-Signature", sig)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		slog.Error("webhook delivery failed", "url", hook.URL, "error", err)
		d.store.UpdateWebhookFailure(hook.ID, true)
		return
	}
	resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		d.store.UpdateWebhookFailure(hook.ID, false)
	} else {
		slog.Warn("webhook non-2xx response", "status", resp.StatusCode, "url", hook.URL)
		d.store.UpdateWebhookFailure(hook.ID, true)
	}
}

// matchesEvent checks whether a webhook's event filter (JSON array)
// includes the given event name, or the wildcard "*".
func matchesEvent(eventsJSON, event string) bool {
	var eventList []string
	if err := json.Unmarshal([]byte(eventsJSON), &eventList); err != nil {
		// Malformed JSON — treat as no match
		return false
	}
	for _, e := range eventList {
		if e == "*" || e == event {
			return true
		}
	}
	return false
}

// computeHMAC returns the hex-encoded HMAC-SHA256 of the body using the secret.
func computeHMAC(body, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
