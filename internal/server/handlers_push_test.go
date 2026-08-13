package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

// TestPushToItem_RequiresMessage covers the 400-on-empty(-after-trim)
// validation: an absent message and a whitespace-only one are both
// rejected, so a blank push instruction can never reach the bus.
func TestPushToItem_RequiresMessage(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)

	for _, body := range [][]byte{
		nil,
		[]byte(`{}`),
		[]byte(`{"message":""}`),
		[]byte(`{"message":"   "}`),
		[]byte(`{"message":"\n\t "}`),
	} {
		rr := bearerCall(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tok.Token, body)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("body %q: expected 400, got %d (body: %s)", body, rr.Code, rr.Body.String())
		}
	}
}

// TestPushToItem_UnavailableWithoutBus covers the 503-on-nil-bus guard:
// unlike a durable watch, a push has no persistence to fall back on, so
// a missing bus must fail loudly rather than silently losing the
// message.
func TestPushToItem_UnavailableWithoutBus(t *testing.T) {
	t.Parallel()
	srv := testServer(t) // NOT testServerWithWatchEvents — bus is nil
	slug, item, tok, _ := setupWatchTestUser(t, srv)

	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tok.Token,
		map[string]interface{}{"message": "triage this"})
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestPushToItem_DeliversSelfAddressed mirrors
// TestWatchEventsStream_AddressedToYou_Assignment: a push must reach the
// SAME user's own connected stream with no explicit watch required, and
// carry the collapsed message as Summary.
func TestPushToItem_DeliversSelfAddressed(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := connectWatchStream(ctx, t, ts.URL, tok.Token)
	waitForWatchEvent(t, ch, 3*time.Second) // connected

	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tok.Token,
		map[string]interface{}{"message": "triage this\nwith the triage playbook"})
	if rr.Code != http.StatusOK {
		t.Fatalf("push: %d %s", rr.Code, rr.Body.String())
	}

	ev := waitForWatchEvent(t, ch, 3*time.Second)
	var payload watchEventPayload
	if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if payload.Kind != watchevents.KindPush {
		t.Fatalf("expected kind %q, got %q", watchevents.KindPush, payload.Kind)
	}
	if payload.ItemRef != item.Ref {
		t.Fatalf("expected item_ref %q, got %q", item.Ref, payload.ItemRef)
	}
	if payload.Summary != "triage this with the triage playbook" {
		t.Fatalf("expected newline-collapsed summary, got %q", payload.Summary)
	}
}

// TestPushToItem_InvisibleItemDenied covers the same resolve/visibility
// gate every other item-scoped handler enforces: pushing a nonexistent
// ref 404s.
func TestPushToItem_InvisibleItemDenied(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	slug, _, tok, _ := setupWatchTestUser(t, srv)

	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/NOPE-999/push", tok.Token,
		map[string]interface{}{"message": "triage this"})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}
