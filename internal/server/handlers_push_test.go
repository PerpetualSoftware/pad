package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestPushToItem_ResponseCarriesWorkspaceSlug covers the P2 finding
// (dispatcher review round 2, codex): a successful push's JSON response
// must carry ref/workspace/pushed/message so `pad push --format json`
// has something real to print, and workspace must be the CANONICAL
// slug (not merely whatever the URL happened to contain) — same
// disambiguation rationale as the monitor line's workspace prefix.
func TestPushToItem_ResponseCarriesWorkspaceSlug(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)

	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tok.Token,
		map[string]interface{}{"message": "triage this"})
	if rr.Code != http.StatusOK {
		t.Fatalf("push: %d %s", rr.Code, rr.Body.String())
	}

	var resp pushResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.Ref != item.Ref {
		t.Fatalf("expected ref %q, got %q", item.Ref, resp.Ref)
	}
	if resp.Workspace != slug {
		t.Fatalf("expected workspace %q, got %q", slug, resp.Workspace)
	}
	if !resp.Pushed {
		t.Fatal("expected pushed: true")
	}
	if resp.Message != "triage this" {
		t.Fatalf("expected message %q, got %q", "triage this", resp.Message)
	}
}

// TestPushToItem_RejectsOverLongMessage covers the 400-over-cap
// validation (dispatcher review round 1): a message whose COLLAPSED
// length exceeds maxPushMessageLen is rejected rather than silently
// truncated — the message is the payload, not a preview, so truncation
// would corrupt what the user asked for. Built with extra internal
// whitespace so this also pins that the cap is measured AFTER
// whitespace collapse, not before (a message that's only over-length
// due to now-collapsed whitespace must NOT be rejected).
func TestPushToItem_RejectsOverLongMessage(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)

	overLong := strings.Repeat("a  \n  ", maxPushMessageLen) // collapses to > maxPushMessageLen chars
	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tok.Token,
		map[string]interface{}{"message": overLong})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an over-cap message, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestPushToItem_MessageAtCapSurvivesIntact proves a message whose
// COLLAPSED length is exactly at maxPushMessageLen is accepted and
// delivered byte-for-byte (not silently truncated at the boundary) —
// the counterpart to the over-cap rejection test above.
func TestPushToItem_MessageAtCapSurvivesIntact(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := connectWatchStream(ctx, t, ts.URL, tok.Token)
	waitForWatchEvent(t, ch, 3*time.Second) // connected

	atCap := strings.Repeat("a", maxPushMessageLen) // already whitespace-free: collapse is a no-op
	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/push", tok.Token,
		map[string]interface{}{"message": atCap})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for a message exactly at the cap, got %d (body: %s)", rr.Code, rr.Body.String())
	}

	ev := waitForWatchEvent(t, ch, 3*time.Second)
	var payload watchEventPayload
	if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if payload.Summary != atCap {
		t.Fatalf("expected the at-cap message to survive collapse+publish intact (len %d), got len %d", len(atCap), len(payload.Summary))
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
