package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/events"
)

// failingEstablishBus stands in for a bus whose Redis SUBSCRIBE cannot be
// issued (BUG-2764): every subscribe returns SubscribeFailed and hands back no
// channel.
type failingEstablishBus struct {
	events.EventBus
}

func (b *failingEstablishBus) SubscribeIfAllowed(context.Context, string, int) (chan events.Event, <-chan struct{}, events.SubscribeOutcome) {
	return nil, nil, events.SubscribeFailed
}

func (b *failingEstablishBus) SubscribeAndReplaySince(context.Context, string, int64, int) (chan events.Event, []events.Event, <-chan struct{}, events.SubscribeOutcome) {
	return nil, nil, nil, events.SubscribeFailed
}

// TestAFailedSubscriptionIsRefusedWithARetryableStatus is the BINDING
// assertion for BUG-2764 (CONVE-19): internal/events vouches for the bus
// returning SubscribeFailed, and says nothing about what the SSE handler does
// with it. Through the router, not by direct call.
//
// The wrong behaviours it is written against: the pre-fix handler had no case
// for the outcome and fell to the default arm — a 500 "internal_error" that
// reads as a server bug rather than a transient the client should retry —
// and a handler that admitted the caller anyway would answer 200 and hold a
// stream that carries nothing. Both are distinguishable from a 503 with a
// Retry-After and the named code. The admission slot must also be released:
// a refusal that kept it would let a Redis outage exhaust the per-instance
// bound with connections that were never served.
func TestAFailedSubscriptionIsRefusedWithARetryableStatus(t *testing.T) {
	srv := testServerWithEvents(t)
	srv.SetEventBus(&failingEstablishBus{EventBus: events.New()})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	slug := createTestWorkspace(t, ts.URL, "Test")

	// BOTH BRANCHES of the handler's subscribe (codex round 5 on BUG-2764):
	// a fresh connection goes through SubscribeIfAllowed, a resume through
	// SubscribeAndReplaySince, and each has its own call site that could
	// have missed the mapping.
	for name, lastID := range map[string]string{"fresh": "", "resume": "7"} {
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/events?workspace="+slug, nil)
		if err != nil {
			t.Fatalf("%s: new request: %v", name, err)
		}
		if lastID != "" {
			req.Header.Set("Last-Event-ID", lastID)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: GET: %v", name, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503 (body %q)", name, resp.StatusCode, body)
		}
		if got := resp.Header.Get("Retry-After"); got == "" {
			t.Fatalf("%s: no Retry-After on a refusal the client is meant to retry", name)
		}
		var payload struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("%s: body is not the JSON error shape: %v (%q)", name, err, body)
		}
		if payload.Error.Code != "subscription_failed" {
			t.Fatalf("%s: error code = %q, want subscription_failed (body %q)", name, payload.Error.Code, body)
		}
		if held := srv.admission().heldTotal(); held != 0 {
			t.Fatalf("%s: admission slots held after the refusal = %d, want 0", name, held)
		}
	}
}
