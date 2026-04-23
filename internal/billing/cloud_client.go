// Package billing holds the reverse pad → pad-cloud client used to cascade
// billing operations (e.g. Stripe subscription cancel) from the pad binary
// out to the pad-cloud sidecar during account deletion. Kept in its own
// package so the server package stays free of Stripe / HTTP-client
// dependencies, and so tests can inject a fake via the server.CloudSidecar
// interface.
package billing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// defaultTimeout bounds each request. 15s is deliberately longer than the
// pad-cloud 10s Stripe client timeout so a genuine upstream cancel that
// gets close to its own cap still returns to pad instead of being cut off
// locally — but short enough that a wedged sidecar doesn't block the
// account-delete handler (which also holds an open HTTP request to the
// end user) for more than a few seconds.
const defaultTimeout = 15 * time.Second

// maxResponseBody caps the size of the response body we will read when
// decoding pad-cloud's error shape. pad-cloud's cancel-customer endpoint
// returns tiny JSON objects; anything larger is almost certainly a misrouted
// HTML error page from a proxy, and we don't want to allocate MB of it.
const maxResponseBody = 64 * 1024

// ResolveOutboundSecret picks the secret to send on pad → pad-cloud calls
// given the inbound rotation list and an optional explicit override. Pulled
// out of main.go so the rotation-sensitive logic can be exercised directly
// by unit tests.
//
//   - explicit takes precedence when non-empty: operators pin this to the
//     exact value pad-cloud is currently validating against.
//   - Otherwise, scan the comma-separated inboundList from RIGHT to LEFT
//     and return the first non-empty entry. The older value is on the
//     right in the conventional "new,old" layout — so during a rotation
//     where pad-cloud is still running "old", pad's outbound call still
//     matches.
//   - Returns "" when neither source supplies a usable value. Callers
//     should treat that as a hard startup error — a misconfigured sidecar
//     URL is worse than no sidecar (every delete would 500 instead of
//     silently skipping cancel).
func ResolveOutboundSecret(explicit, inboundList string) string {
	if s := strings.TrimSpace(explicit); s != "" {
		return s
	}
	parts := strings.Split(inboundList, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(parts[i]); s != "" {
			return s
		}
	}
	return ""
}

// CloudClient calls the pad-cloud sidecar over HTTP. Stateless — safe to
// share across goroutines; the underlying http.Client has its own pool.
type CloudClient struct {
	baseURL     string
	cloudSecret string
	http        *http.Client
}

// NewCloudClient constructs a client pointed at the given pad-cloud base URL
// (e.g. "http://pad-cloud:7778") authenticated with cloudSecret. Both values
// must be non-empty — callers that can't provide them should skip wiring
// the client into the server rather than passing blanks, which would turn
// into silent 403s at request time.
func NewCloudClient(baseURL, cloudSecret string) *CloudClient {
	return &CloudClient{
		baseURL:     strings.TrimRight(baseURL, "/"),
		cloudSecret: cloudSecret,
		http: &http.Client{
			Timeout: defaultTimeout,
		},
	}
}

// SidecarError carries the structured details pad-cloud returns on a non-2xx
// response. Exposed so callers that want to log the status separately from
// the error message can extract it via errors.As. We deliberately do NOT
// expose a "this is a retryable/ignorable error" helper — pad-cloud
// normalizes Stripe's "already gone" cases to 200 internally, so every
// non-2xx we see is a real failure (ops misconfig, upstream breakage) and
// callers should treat the whole class uniformly as "abort".
type SidecarError struct {
	// Status is the HTTP status pad-cloud returned (e.g. 400, 403, 500).
	Status int
	// Body is the raw response body for log diagnostics only. Do not surface
	// this to end users — the sidecar's errors are internal and may leak
	// infra detail.
	Body string
}

func (e *SidecarError) Error() string {
	return fmt.Sprintf("pad-cloud sidecar returned %d: %s", e.Status, e.Body)
}

// CancelCustomer asks pad-cloud to cancel every active Stripe subscription
// for customerID and then delete the Stripe customer object. Idempotent at
// the pad-cloud side (it treats a 404/resource_missing from Stripe as
// success), so retries after a partial failure complete cleanly.
//
// Request: POST {baseURL}/billing/cancel-customer
// Body:    {"customer_id": "cus_xxx", "cloud_secret": "..."}
// 200 OK:  {"ok": true, "subscriptions_cancelled": N}
//
// Returns nil on 200. On any non-200, returns a *SidecarError so the caller
// can branch on Status. On transport failure (DNS, connect, timeout) returns
// a bare error — treated by callers as "retryable, abort the delete".
func (c *CloudClient) CancelCustomer(customerID string) error {
	if c == nil {
		return errors.New("billing: CancelCustomer called on nil CloudClient")
	}
	if customerID == "" {
		// Defensive — handleDeleteAccount is expected to skip the call when
		// StripeCustomerID is empty, but if somebody wires it differently
		// we refuse to POST an empty cus_ that would burn a sidecar call
		// and log-spam the 400 it would return.
		return errors.New("billing: customerID is empty")
	}
	if c.baseURL == "" || c.cloudSecret == "" {
		return errors.New("billing: CloudClient is not configured (missing baseURL or cloudSecret)")
	}

	payload, err := json.Marshal(map[string]string{
		"customer_id":  customerID,
		"cloud_secret": c.cloudSecret,
	})
	if err != nil {
		return fmt.Errorf("billing: marshal cancel-customer request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost,
		c.baseURL+"/billing/cancel-customer",
		bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("billing: build cancel-customer request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// Transport-level failure — DNS, connect refused, TLS, timeout. Caller
		// must abort the delete so a retry can try again with state intact.
		return fmt.Errorf("billing: cancel-customer request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		// We don't need the body — the happy-path envelope is advisory.
		// Drain-and-discard so the connection returns to the pool; the
		// limit keeps a broken sidecar from streaming us into memory pressure.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
		return nil
	}

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	return &SidecarError{
		Status: resp.StatusCode,
		Body:   string(bodyBytes),
	}
}
