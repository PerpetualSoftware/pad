package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/PerpetualSoftware/pad/internal/appfetch"
)

// App webhook delivery (SPEC-6 U10b, DOC-3371 §5; TASK-3408).
//
// Owner hooks keep DeliverEvent: the stored payload, the v1 signature,
// redirects re-screened and followed. App hooks differ on every axis that
// matters to an app's trust boundary, so they have their own path:
//   - the body is the app DTO built from the event-time projection, never
//     the stored snapshot;
//   - every attempt is ADMITTED (install active, hook released, event
//     visible) in a transaction that records it in flight, and the record
//     is removed when the attempt ends: disable waits for those records;
//   - https only, never a redirect (a 3xx is a failed attempt), dial-time
//     screening with the admin's webhook-flagged private origins;
//   - signature v2 over a timestamp, plus the install id.

// AppAdmission is what an admitted attempt sends with.
type AppAdmission struct {
	URL       string
	Secret    string
	InstallID string
}

// ErrAppDeliveryRefused wraps an admission refusal: nothing is owed.
var ErrAppDeliveryRefused = errors.New("app delivery refused")

// AppAdmitter admits and ends attempts (the store, through the server).
// AdmitAppDelivery returns an error wrapping ErrAppDeliveryRefused when the
// attempt must not be sent; any other error is the store's and the event is
// still owed.
type AppAdmitter interface {
	AdmitAppDelivery(webhookID, event, collectionID, occurredAt, deliveryID string) (*AppAdmission, error)
	EndAppDelivery(deliveryID string) error
}

// AppPoster sends one request under the app fetch policy (appfetch.Poster).
// An error wrapping appfetch.ErrRefused is the policy's and permanent.
type AppPoster interface {
	Post(ctx context.Context, rawURL string, body []byte, header http.Header) (int, error)
}

// AppDelivery is one app event for one app hook.
type AppDelivery struct {
	WebhookID    string
	Event        string
	CollectionID string
	// OccurredAt is the event's outbox occurred_at; admission refuses an
	// event older than the hook's deliver_from.
	OccurredAt string
	Body       []byte
	// RateGate, when set, is asked before EVERY attempt, retries included,
	// so a rate cap bounds requests rather than deliveries (codex r1 on
	// U10c). false stops the delivery with AppRateLimited, nothing sent.
	RateGate func() bool
}

// AppResult is what happened to one app delivery.
type AppResult int

const (
	// AppDelivered: a 2xx.
	AppDelivered AppResult = iota
	// AppTransient: retries exhausted on network errors, timeouts or 5xx,
	// or the dispatcher stopped mid-backoff. The event is still owed.
	AppTransient
	// AppPermanent: a 4xx, a 3xx, or a policy refusal. Not owed.
	AppPermanent
	// AppRefused: admission refused (install not active, hook held or
	// gone, event not visible). Nothing was sent and nothing is owed.
	AppRefused
	// AppDeferred: admission could not be decided (a store error). Nothing
	// was sent; the event is still owed.
	AppDeferred
	// AppRateLimited: the install's rate cap refused the attempt. Nothing
	// was sent; the event is still owed and no attempt is charged.
	AppRateLimited
)

// AppSignatureHeader carries `t=<unix seconds>,v1=<hex HMAC-SHA256 of
// "<t>.<body>">`. Receivers should refuse a t more than AppSignatureSkew from
// their clock.
const AppSignatureHeader = "X-Pad-Signature-256"

// AppInstallHeader names the install the delivery is for.
const AppInstallHeader = "X-Pad-Install"

// AppSignatureSkew is the documented receiver tolerance for t=.
const AppSignatureSkew = 5 * time.Minute

// SignAppPayload returns the signature v2 header value.
func SignAppPayload(secret string, t time.Time, body []byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// newDeliveryID names one attempt's in-flight record.
var newDeliveryID = func() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// DeliverAppEvent delivers one app event to one app hook, synchronously,
// with the owner path's retry schedule (maxDeliveryAttempts, linear backoff
// on context-aware timers). Each attempt is admitted separately: an install
// disabled between attempts gets no further attempt.
func (d *Dispatcher) DeliverAppEvent(adm AppAdmitter, poster AppPoster, dv AppDelivery) AppResult {
	parent := d.context()
	result := AppPermanent
	for attempt := 1; attempt <= maxDeliveryAttempts; attempt++ {
		result = d.attemptApp(parent, adm, poster, dv)
		if result != AppTransient {
			return result
		}
		if attempt < maxDeliveryAttempts {
			if backoff := d.retryBackoff * time.Duration(attempt); backoff > 0 && !d.wait(parent, backoff) {
				return AppTransient
			}
		}
	}
	return result
}

func (d *Dispatcher) attemptApp(parent context.Context, adm AppAdmitter, poster AppPoster, dv AppDelivery) AppResult {
	if parent.Err() != nil {
		return AppTransient
	}
	if dv.RateGate != nil && !dv.RateGate() {
		return AppRateLimited
	}
	// The deadline is RELATIVE and starts BEFORE admission (DOC-3371 §5):
	// any delay before the send only shortens the attempt, while the
	// in-flight record lasts 12 s of database time from a later instant, so
	// the attempt always ends before its record expires.
	ctx, cancel := context.WithTimeout(parent, deliveryTimeout)
	defer cancel()

	deliveryID := newDeliveryID()
	a, err := adm.AdmitAppDelivery(dv.WebhookID, dv.Event, dv.CollectionID, dv.OccurredAt, deliveryID)
	if err != nil {
		if errors.Is(err, ErrAppDeliveryRefused) {
			return AppRefused
		}
		slog.Error("app webhook admission failed", "webhook_id", dv.WebhookID, "error", err)
		return AppDeferred
	}
	defer func() {
		if err := adm.EndAppDelivery(deliveryID); err != nil {
			// The record expires on its own; the drain waits at most that.
			slog.Error("app webhook: ending the in-flight record failed", "webhook_id", dv.WebhookID, "error", err)
		}
	}()

	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", "Pad-Webhook/2.0")
	h.Set(AppSignatureHeader, SignAppPayload(a.Secret, d.now(), dv.Body))
	h.Set(AppInstallHeader, a.InstallID)

	status, err := poster.Post(ctx, a.URL, dv.Body, h)
	if err != nil {
		if errors.Is(err, appfetch.ErrRefused) {
			slog.Warn("app webhook refused by policy", "webhook_id", dv.WebhookID, "error", err)
			return AppPermanent
		}
		slog.Warn("app webhook delivery failed", "webhook_id", dv.WebhookID, "error", err)
		return AppTransient
	}
	switch {
	case status >= 200 && status < 300:
		return AppDelivered
	case status >= 500 && status < 600:
		return AppTransient
	default:
		// 3xx (never followed), 4xx, anything else.
		slog.Warn("app webhook non-2xx", "webhook_id", dv.WebhookID, "status", status)
		return AppPermanent
	}
}

// String names a result, for logs and tests.
func (r AppResult) String() string {
	switch r {
	case AppDelivered:
		return "delivered"
	case AppTransient:
		return "transient"
	case AppPermanent:
		return "permanent"
	case AppRefused:
		return "refused"
	case AppDeferred:
		return "deferred"
	case AppRateLimited:
		return "rate_limited"
	}
	return fmt.Sprintf("AppResult(%d)", int(r))
}
