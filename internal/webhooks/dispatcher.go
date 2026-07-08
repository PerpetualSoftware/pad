package webhooks

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"syscall"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// maxWebhookRedirects caps how many HTTP redirects a delivery will follow.
// Every hop is re-validated by checkRedirect, so this is a belt-and-braces
// bound against redirect loops rather than the primary SSRF control.
const maxWebhookRedirects = 5

// deliveryTimeout is the total per-delivery HTTP timeout.
const deliveryTimeout = 10 * time.Second

// maxDeliveryAttempts caps how many times a single delivery is attempted
// before giving up. Only transient failures (network errors, timeouts, 5xx)
// consume retries; permanent failures (4xx, SSRF block) stop immediately.
const maxDeliveryAttempts = 3

// defaultRetryBackoff is the base backoff between transient-failure retries;
// the Nth wait is defaultRetryBackoff * N (linear). Overridable via the
// Dispatcher.retryBackoff field (set to 0 in tests to avoid sleeping).
const defaultRetryBackoff = 500 * time.Millisecond

// deliveryResult classifies the outcome of a single delivery attempt so the
// retry loop knows whether to retry (transient), give up (permanent), or
// stop happily (success).
type deliveryResult int

const (
	deliverySuccess deliveryResult = iota
	deliveryTransient
	deliveryPermanent
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
	store  WebhookStore
	client *http.Client
	// spawn runs a delivery goroutine. When nil, deliveries run on a plain
	// `go f()`. The server injects Server.goAsync here (via SetSpawn) so
	// in-flight deliveries are tracked on s.bg and awaited by Server.Stop()
	// — closing the BUG-842 shutdown race where a detached delivery writes
	// to an already-closed store — and inherit goAsync's panic recovery.
	spawn func(func())
	// retryBackoff is the base wait between transient-failure retries.
	// Defaults to defaultRetryBackoff; tests set it to 0.
	retryBackoff time.Duration
	SkipSSRF     bool // Skip SSRF validation (for tests only)
}

// SetSpawn injects the goroutine spawner used for deliveries. Passing the
// server's tracked-goroutine helper (Server.goAsync) makes Server.Stop()
// wait for in-flight deliveries. A nil spawn (the default) falls back to a
// plain goroutine, keeping standalone Dispatcher usage working.
func (d *Dispatcher) SetSpawn(spawn func(func())) {
	d.spawn = spawn
}

// run executes fn on the injected spawner, or a plain goroutine if none is set.
func (d *Dispatcher) run(fn func()) {
	if d.spawn != nil {
		d.spawn(fn)
		return
	}
	go fn()
}

// NewDispatcher creates a Dispatcher with the given store.
//
// The delivery client enforces the SSRF guard at connect time, not just at
// parse time: its dialer's Control callback re-checks the ACTUAL resolved IP
// before the socket connects (closing the DNS-rebind TOCTOU where a hostname
// validates as public then resolves to an internal IP), and CheckRedirect
// re-runs ValidateWebhookURL on every hop so a 302 can't bounce the request
// to an internal target. Proxy is intentionally nil — honoring HTTP(S)_PROXY
// would connect to the proxy host and skip our dialer's IP check entirely.
func NewDispatcher(store WebhookStore) *Dispatcher {
	d := &Dispatcher{store: store, retryBackoff: defaultRetryBackoff}

	dialer := &net.Dialer{
		Timeout:   deliveryTimeout,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			if d.SkipSSRF {
				return nil
			}
			return screenDialAddr(address)
		},
	}
	d.client = &http.Client{
		Timeout: deliveryTimeout,
		Transport: &http.Transport{
			Proxy:                 nil, // never route through an env proxy — see NewDispatcher docstring
			DialContext:           dialer.DialContext,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
		CheckRedirect: d.checkRedirect,
	}
	return d
}

// checkRedirect re-validates every redirect hop against the SSRF guard and
// caps the redirect chain length. Without it, an allowed public endpoint
// could 302 the delivery to an internal address.
func (d *Dispatcher) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxWebhookRedirects {
		return fmt.Errorf("stopped after %d redirects", maxWebhookRedirects)
	}
	if d.SkipSSRF {
		return nil
	}
	if err := ValidateWebhookURL(req.URL.String()); err != nil {
		return fmt.Errorf("redirect to %s blocked: %w", req.URL.Redacted(), err)
	}
	return nil
}

// screenDialAddr rejects a dial to a private/reserved IP. The dialer calls
// this with the resolved connection address (ip:port), so it validates the
// exact target the socket is about to connect to — this is the dial-time
// check that defeats DNS rebinding.
func screenDialAddr(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("webhook dial blocked: %q is not a resolved IP", address)
	}
	if isPrivateIP(ip) {
		return fmt.Errorf("webhook dial blocked: private or reserved IP %s", ip)
	}
	return nil
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
		d.run(func() { d.deliver(hook, body) })
	}
}

// deliver sends a webhook, retrying transient failures up to
// maxDeliveryAttempts with linear backoff, and records the final outcome
// once via the store. Permanent failures (4xx, SSRF block, malformed URL)
// stop immediately without consuming retries.
func (d *Dispatcher) deliver(hook models.Webhook, body []byte) {
	result := deliveryPermanent
	for attempt := 1; attempt <= maxDeliveryAttempts; attempt++ {
		result = d.attemptDeliver(hook, body)
		if result != deliveryTransient {
			break // success or permanent failure — no point retrying
		}
		if attempt < maxDeliveryAttempts {
			if backoff := d.retryBackoff * time.Duration(attempt); backoff > 0 {
				time.Sleep(backoff)
			}
		}
	}
	d.store.UpdateWebhookFailure(hook.ID, result != deliverySuccess)
}

// attemptDeliver performs a single HTTP POST to the webhook URL and
// classifies the outcome. It does NOT record the result — deliver owns the
// single terminal store write so the retry loop doesn't churn the store.
func (d *Dispatcher) attemptDeliver(hook models.Webhook, body []byte) deliveryResult {
	// Defense in depth: re-validate URL before making the request. An
	// SSRF block is permanent — retrying won't make the target public.
	if !d.SkipSSRF {
		if err := ValidateWebhookURL(hook.URL); err != nil {
			slog.Warn("blocked webhook delivery", "url", hook.URL, "error", err)
			return deliveryPermanent
		}
	}

	req, err := http.NewRequest(http.MethodPost, hook.URL, bytes.NewReader(body))
	if err != nil {
		// A malformed URL/method won't fix itself on retry.
		slog.Error("failed to create webhook request", "url", hook.URL, "error", err)
		return deliveryPermanent
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Pad-Webhook/1.0")

	if hook.Secret != "" {
		sig := computeHMAC(body, []byte(hook.Secret))
		req.Header.Set("X-Pad-Signature", sig)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		// Network error / timeout — transient, worth retrying.
		slog.Error("webhook delivery failed", "url", hook.URL, "error", err)
		return deliveryTransient
	}
	resp.Body.Close()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return deliverySuccess
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		// Client error — permanent; the request itself is unacceptable.
		slog.Warn("webhook 4xx response", "status", resp.StatusCode, "url", hook.URL)
		return deliveryPermanent
	default:
		// 5xx (and any other non-2xx) — transient, worth retrying.
		slog.Warn("webhook non-2xx response", "status", resp.StatusCode, "url", hook.URL)
		return deliveryTransient
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
