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

	resp, err := http.Get(ts.URL + "/api/v1/events?workspace=" + slug)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Fatal("no Retry-After on a refusal the client is meant to retry")
	}
	body, _ := io.ReadAll(resp.Body)
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("body is not the JSON error shape: %v (%q)", err, body)
	}
	if payload.Error.Code != "subscription_failed" {
		t.Fatalf("error code = %q, want subscription_failed (body %q)", payload.Error.Code, body)
	}
	if held := srv.admission().heldTotal(); held != 0 {
		t.Fatalf("admission slots held after the refusal = %d, want 0", held)
	}
}
