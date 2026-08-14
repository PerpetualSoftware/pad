package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

func testServerWithWatchEvents(t *testing.T) *Server {
	t.Helper()
	srv := testServer(t)
	srv.SetWatchEventsBus(watchevents.New())
	return srv
}

// watchSSEEvent mirrors sseEvent (handlers_events_test.go) for the
// watch-events stream.
type watchSSEEvent struct {
	Type string
	Data string
}

// connectWatchStream connects to GET /api/v1/events/stream with a Bearer
// token and returns a channel of parsed SSE events.
func connectWatchStream(ctx context.Context, t *testing.T, baseURL, token string) <-chan watchSSEEvent {
	t.Helper()
	ch := make(chan watchSSEEvent, 32)

	req, err := http.NewRequestWithContext(ctx, "GET", baseURL+"/api/v1/events/stream", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}

	go func() {
		defer resp.Body.Close()
		defer close(ch)
		scanner := bufio.NewScanner(resp.Body)
		var eventType, data string
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				eventType = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			case line == "":
				if eventType != "" && data != "" {
					ch <- watchSSEEvent{Type: eventType, Data: data}
					eventType, data = "", ""
				}
			}
		}
	}()

	return ch
}

func waitForWatchEvent(t *testing.T, ch <-chan watchSSEEvent, timeout time.Duration) watchSSEEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("event channel closed unexpectedly")
		}
		return ev
	case <-time.After(timeout):
		t.Fatal("timed out waiting for watch stream event")
		return watchSSEEvent{}
	}
}

// assertNoWatchEvent drains ch for `d` and fails if a "notification"
// event with the given item_ref shows up — used to assert an unwatched
// item's mutation does NOT reach the stream.
func assertNoWatchEventForRef(t *testing.T, ch <-chan watchSSEEvent, ref string, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.Type != "notification" {
				continue
			}
			var payload watchEventPayload
			if err := json.Unmarshal([]byte(ev.Data), &payload); err == nil && payload.ItemRef == ref {
				t.Fatalf("unexpected notification for unwatched item %q: %+v", ref, payload)
			}
		case <-deadline:
			return
		}
	}
}

func TestWatchEventsStream_RequiresAuth(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/events/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestWatchEventsStream_ConnectedEvent(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	_, _, tok, user := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := connectWatchStream(ctx, t, ts.URL, tok.Token)
	ev := waitForWatchEvent(t, ch, 3*time.Second)
	if ev.Type != "connected" {
		t.Fatalf("expected 'connected' event, got %q (data: %s)", ev.Type, ev.Data)
	}
	var data map[string]string
	if err := json.Unmarshal([]byte(ev.Data), &data); err != nil {
		t.Fatalf("parse connected data: %v", err)
	}
	if data["user_id"] != user.ID {
		t.Fatalf("expected user_id %q, got %q", user.ID, data["user_id"])
	}
}

func TestWatchEventsStream_DeliversWatchedStatusChange(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Unconditional watch.
	rr := bearerCall(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/watch", tok.Token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("create watch: %d %s", rr.Code, rr.Body.String())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := connectWatchStream(ctx, t, ts.URL, tok.Token)
	waitForWatchEvent(t, ch, 3*time.Second) // connected

	rr = bearerJSON(t, srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, tok.Token,
		map[string]interface{}{"fields": `{"status":"done"}`})
	if rr.Code != http.StatusOK {
		t.Fatalf("update item: %d %s", rr.Code, rr.Body.String())
	}

	ev := waitForWatchEvent(t, ch, 3*time.Second)
	if ev.Type != "notification" {
		t.Fatalf("expected 'notification', got %q (data: %s)", ev.Type, ev.Data)
	}
	var payload watchEventPayload
	if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if payload.ItemRef != item.Ref {
		t.Fatalf("expected item_ref %q, got %q", item.Ref, payload.ItemRef)
	}
	if payload.Kind != watchevents.KindStatusChange {
		t.Fatalf("expected kind %q, got %q", watchevents.KindStatusChange, payload.Kind)
	}
	if payload.Workspace != slug {
		t.Fatalf("expected workspace %q, got %q", slug, payload.Workspace)
	}
	if payload.Summary != "open → done" {
		t.Fatalf("expected summary 'open → done', got %q", payload.Summary)
	}
}

func TestWatchEventsStream_SuppressesUnwatchedItem(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	slug, watchedItem, tok, _ := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Seed a second, unwatched item in the same collection.
	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", tok.Token,
		map[string]interface{}{"title": "Not watched", "fields": `{"status":"open"}`})
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed second item: %d %s", rr.Code, rr.Body.String())
	}
	var otherItem models.Item
	parseJSON(t, rr, &otherItem)

	rr = bearerCall(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+watchedItem.Slug+"/watch", tok.Token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("create watch: %d %s", rr.Code, rr.Body.String())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := connectWatchStream(ctx, t, ts.URL, tok.Token)
	waitForWatchEvent(t, ch, 3*time.Second) // connected

	// Mutate the UNWATCHED item — must not surface.
	rr = bearerJSON(t, srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+otherItem.Slug, tok.Token,
		map[string]interface{}{"fields": `{"status":"done"}`})
	if rr.Code != http.StatusOK {
		t.Fatalf("update other item: %d %s", rr.Code, rr.Body.String())
	}
	// Then mutate the WATCHED item — must surface, proving the stream is
	// live and the absence above wasn't just "nothing happened yet".
	rr = bearerJSON(t, srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+watchedItem.Slug, tok.Token,
		map[string]interface{}{"fields": `{"status":"done"}`})
	if rr.Code != http.StatusOK {
		t.Fatalf("update watched item: %d %s", rr.Code, rr.Body.String())
	}

	ev := waitForWatchEvent(t, ch, 3*time.Second)
	var payload watchEventPayload
	if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if payload.ItemRef != watchedItem.Ref {
		t.Fatalf("expected the WATCHED item's ref %q first, got %q — unwatched item leaked", watchedItem.Ref, payload.ItemRef)
	}
}

// TestWatchEventsStream_AssignmentIsNotAddressedToYou is the inverted
// successor of TASK-2533's TestWatchEventsStream_AddressedToYou_Assignment,
// which asserted the opposite: that assigning an UNWATCHED item to the
// connected user surfaced on their stream. IDEA-2544 Phase 2 (TASK-2551)
// removed that delivery path outright — assignment is bookkeeping, push
// is dispatch — so the same setup must now stay silent.
//
// Structured so a silent stream can't pass it by accident (team CONVE-12:
// an end-state assertion is only evidence when no OTHER mechanism
// produces the same end state). Both legs are published on the SAME bus
// in order, so receiving the WATCHED item's assignment proves the
// unwatched one was filtered rather than merely slow — no sleep, no
// timing window. The control leg also pins the half of the rule that
// SURVIVED: an unconditional watch still delivers assignment
// notifications, exactly as `pad watch --help` promises.
func TestWatchEventsStream_AssignmentIsNotAddressedToYou(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	slug, item, tok, user := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// The control item, watched BEFORE connecting (the watches map is
	// loaded once at connect time — same rationale as the other
	// watch-before-connect setups in this file).
	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", tok.Token,
		map[string]interface{}{"title": "Watched control", "fields": `{"status":"open"}`})
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed watched item: %d %s", rr.Code, rr.Body.String())
	}
	var watched models.Item
	parseJSON(t, rr, &watched)
	rr = bearerCall(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+watched.Slug+"/watch", tok.Token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("create watch: %d %s", rr.Code, rr.Body.String())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := connectWatchStream(ctx, t, ts.URL, tok.Token)
	waitForWatchEvent(t, ch, 3*time.Second) // connected

	// Leg 1: assign the UNWATCHED item to the connected user. Pre-Phase-2
	// this fired via addressed-to-you; it must now be silent.
	rr = bearerJSON(t, srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, tok.Token,
		map[string]interface{}{"assigned_user_id": user.ID})
	if rr.Code != http.StatusOK {
		t.Fatalf("assign unwatched item: %d %s", rr.Code, rr.Body.String())
	}
	// Leg 2: assign the WATCHED item — must surface, proving the stream is
	// live and leg 1's absence isn't just "nothing happened yet".
	rr = bearerJSON(t, srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+watched.Slug, tok.Token,
		map[string]interface{}{"assigned_user_id": user.ID})
	if rr.Code != http.StatusOK {
		t.Fatalf("assign watched item: %d %s", rr.Code, rr.Body.String())
	}

	ev := waitForWatchEvent(t, ch, 3*time.Second)
	var payload watchEventPayload
	if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if payload.ItemRef != watched.Ref {
		t.Fatalf("expected the WATCHED item's ref %q first, got %q — assignment is still being delivered addressed-to-you", watched.Ref, payload.ItemRef)
	}
	if payload.Kind != watchevents.KindAssignment {
		t.Fatalf("expected kind %q, got %q", watchevents.KindAssignment, payload.Kind)
	}
}

func TestWatchEventsStream_PredicateOnlyFiresOnMatch(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	rr := bearerCall(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/watch", tok.Token,
		[]byte(`{"predicate":"status=done"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("create watch: %d %s", rr.Code, rr.Body.String())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := connectWatchStream(ctx, t, ts.URL, tok.Token)
	waitForWatchEvent(t, ch, 3*time.Second) // connected

	// Transition to a non-matching status first — must NOT fire.
	rr = bearerJSON(t, srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, tok.Token,
		map[string]interface{}{"fields": `{"status":"in-progress"}`})
	if rr.Code != http.StatusOK {
		t.Fatalf("update to in-progress: %d %s", rr.Code, rr.Body.String())
	}
	assertNoWatchEventForRef(t, ch, item.Ref, 300*time.Millisecond)

	// Now transition to the matching status — must fire.
	rr = bearerJSON(t, srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, tok.Token,
		map[string]interface{}{"fields": `{"status":"done"}`})
	if rr.Code != http.StatusOK {
		t.Fatalf("update to done: %d %s", rr.Code, rr.Body.String())
	}

	ev := waitForWatchEvent(t, ch, 3*time.Second)
	var payload watchEventPayload
	if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if payload.Summary != "in-progress → done" {
		t.Fatalf("expected summary 'in-progress → done', got %q", payload.Summary)
	}
}

func TestWatchEventsStream_CommentNotification(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	rr := bearerCall(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/watch", tok.Token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("create watch: %d %s", rr.Code, rr.Body.String())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := connectWatchStream(ctx, t, ts.URL, tok.Token)
	waitForWatchEvent(t, ch, 3*time.Second) // connected

	rr = bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/comments", tok.Token,
		map[string]interface{}{"body": "fix verified, looks good"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create comment: %d %s", rr.Code, rr.Body.String())
	}

	ev := waitForWatchEvent(t, ch, 3*time.Second)
	var payload watchEventPayload
	if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if payload.Kind != watchevents.KindComment {
		t.Fatalf("expected kind %q, got %q", watchevents.KindComment, payload.Kind)
	}
	if payload.Summary != "fix verified, looks good" {
		t.Fatalf("expected summary to be the comment body, got %q", payload.Summary)
	}
}

// TestWatchEventsStream_ReplyNotification covers codex round-1 finding 2:
// handleCreateReply is a SEPARATE code path from handleCreateComment (it
// calls store.CreateComment directly via POST .../comments/{id}/replies,
// not POST .../comments) and was missing the watch-notification hook
// entirely — a reply to a comment on a watched item produced zero
// notification. Verifies the fix.
func TestWatchEventsStream_ReplyNotification(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	rr := bearerCall(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/watch", tok.Token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("create watch: %d %s", rr.Code, rr.Body.String())
	}

	rr = bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/comments", tok.Token,
		map[string]interface{}{"body": "top-level comment"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create comment: %d %s", rr.Code, rr.Body.String())
	}
	var parent models.Comment
	parseJSON(t, rr, &parent)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := connectWatchStream(ctx, t, ts.URL, tok.Token)
	waitForWatchEvent(t, ch, 3*time.Second) // connected

	rr = bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/comments/"+parent.ID+"/replies", tok.Token,
		map[string]interface{}{"body": "a reply worth nudging about"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create reply: %d %s", rr.Code, rr.Body.String())
	}

	ev := waitForWatchEvent(t, ch, 3*time.Second)
	var payload watchEventPayload
	if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if payload.Kind != watchevents.KindComment {
		t.Fatalf("expected kind %q, got %q", watchevents.KindComment, payload.Kind)
	}
	if payload.Summary != "a reply worth nudging about" {
		t.Fatalf("expected summary to be the reply body, got %q", payload.Summary)
	}
	if payload.ItemRef != item.Ref {
		t.Fatalf("expected item_ref %q, got %q", item.Ref, payload.ItemRef)
	}
}

// TestWatchEventsStream_BulkAssignFires exercises the producer-coverage
// claim in publishWatchNotifications' doc comment (TASK-2533 audit): a
// bulk "assign" op (which calls store.UpdateItem directly, bypassing
// handleUpdateItem entirely) must still surface an assignment
// notification — proving the fix belongs at the store layer
// (LastMutation), not in each individual HTTP handler.
//
// The delivery vehicle is an explicit WATCH on the item, not the
// caller's own assignment: IDEA-2544 Phase 2 (TASK-2551) dropped
// addressed-to-you assignment delivery, which is what this test used to
// ride on. The producer claim under test is unchanged — only the reason
// the notification reaches this stream is.
func TestWatchEventsStream_BulkAssignFires(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	slug, item, tok, user := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	rr0 := bearerCall(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/watch", tok.Token, nil)
	if rr0.Code != http.StatusOK {
		t.Fatalf("create watch: %d %s", rr0.Code, rr0.Body.String())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := connectWatchStream(ctx, t, ts.URL, tok.Token)
	waitForWatchEvent(t, ch, 3*time.Second) // connected

	rr := bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/bulk", tok.Token,
		map[string]interface{}{
			"ids":              []string{item.ID},
			"op":               "assign",
			"assigned_user_id": user.ID,
		})
	if rr.Code != http.StatusOK {
		t.Fatalf("bulk assign: %d %s", rr.Code, rr.Body.String())
	}

	ev := waitForWatchEvent(t, ch, 3*time.Second)
	var payload watchEventPayload
	if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if payload.Kind != watchevents.KindAssignment {
		t.Fatalf("expected kind %q from a bulk assign, got %q", watchevents.KindAssignment, payload.Kind)
	}
	if payload.ItemRef != item.Ref {
		t.Fatalf("expected item_ref %q, got %q", item.Ref, payload.ItemRef)
	}
}

// TestWatchEventsStream_UpdateWithAttachedComment covers the bypass
// named in publishWatchNotifications' audit: `PATCH .../items/{slug}`
// with a `comment` field creates that comment via store.CreateComment
// directly, not through handleCreateComment. Both the status-change and
// the comment notification must surface (as two separate notifications
// — TASK-2533 plan note: this system does not combine a field change and
// its attached comment into one nudge line, unlike DOC-2479's single
// combined example).
func TestWatchEventsStream_UpdateWithAttachedComment(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	slug, item, tok, _ := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	rr := bearerCall(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/watch", tok.Token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("create watch: %d %s", rr.Code, rr.Body.String())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := connectWatchStream(ctx, t, ts.URL, tok.Token)
	waitForWatchEvent(t, ch, 3*time.Second) // connected

	rr = bearerJSON(t, srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, tok.Token,
		map[string]interface{}{
			"fields":  `{"status":"done"}`,
			"comment": "fix verified",
		})
	if rr.Code != http.StatusOK {
		t.Fatalf("update with comment: %d %s", rr.Code, rr.Body.String())
	}

	kinds := map[string]watchEventPayload{}
	for i := 0; i < 2; i++ {
		ev := waitForWatchEvent(t, ch, 3*time.Second)
		var payload watchEventPayload
		if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
			t.Fatalf("parse payload: %v", err)
		}
		kinds[payload.Kind] = payload
	}
	sc, ok := kinds[watchevents.KindStatusChange]
	if !ok {
		t.Fatalf("expected a status-change notification, got %+v", kinds)
	}
	if sc.Summary != "open → done" {
		t.Fatalf("expected status summary 'open → done', got %q", sc.Summary)
	}
	c, ok := kinds[watchevents.KindComment]
	if !ok {
		t.Fatalf("expected a comment notification, got %+v", kinds)
	}
	if c.Summary != "fix verified" {
		t.Fatalf("expected comment summary 'fix verified', got %q", c.Summary)
	}
}

// --- Pure unit tests for the visibility/predicate logic (no HTTP) ---

func TestWatchNotificationVisible_UnconditionalWatch(t *testing.T) {
	t.Parallel()
	watches := map[string]string{"item-1": ""}
	n := watchevents.Notification{ItemID: "item-1", Kind: watchevents.KindComment}
	if !watchNotificationVisible(watches, watchAccessVisibility{fullAccess: true}, "user-1", n) {
		t.Fatal("expected an unconditional watch to match any notification on the item")
	}
}

func TestWatchNotificationVisible_UnwatchedItemDenied(t *testing.T) {
	t.Parallel()
	watches := map[string]string{"item-1": ""}
	n := watchevents.Notification{ItemID: "item-2", Kind: watchevents.KindComment}
	if watchNotificationVisible(watches, watchAccessVisibility{fullAccess: true}, "user-1", n) {
		t.Fatal("expected an unwatched item's notification to be denied")
	}
}

func TestWatchNotificationVisible_PredicateGatesNonStatusKinds(t *testing.T) {
	t.Parallel()
	watches := map[string]string{"item-1": "status=done"}
	n := watchevents.Notification{ItemID: "item-1", Kind: watchevents.KindComment}
	if watchNotificationVisible(watches, watchAccessVisibility{fullAccess: true}, "user-1", n) {
		t.Fatal("expected a predicated watch to suppress a comment notification")
	}
}

func TestWatchNotificationVisible_PredicateMatchesOnlyTargetValue(t *testing.T) {
	t.Parallel()
	watches := map[string]string{"item-1": "status=done"}
	wrong := watchevents.Notification{ItemID: "item-1", Kind: watchevents.KindStatusChange, StatusFieldKey: "status", ToStatus: "in-progress"}
	right := watchevents.Notification{ItemID: "item-1", Kind: watchevents.KindStatusChange, StatusFieldKey: "status", ToStatus: "done"}
	if watchNotificationVisible(watches, watchAccessVisibility{fullAccess: true}, "user-1", wrong) {
		t.Fatal("expected the non-matching status transition to be denied")
	}
	if !watchNotificationVisible(watches, watchAccessVisibility{fullAccess: true}, "user-1", right) {
		t.Fatal("expected the matching status transition to be visible")
	}
}

// TestWatchNotificationVisible_AssignmentToYouNeedsAWatch replaces
// TASK-2533's TestWatchNotificationVisible_AddressedToYouIgnoresWatchList,
// which asserted the exact opposite. IDEA-2544 Phase 2 (TASK-2551):
// assignment is no longer addressed traffic, so being the new assignee
// buys nothing — a watch is now the ONLY way a KindAssignment
// notification reaches a caller. Both legs run against the same
// notification so the watch map is provably the only variable.
func TestWatchNotificationVisible_AssignmentToYouNeedsAWatch(t *testing.T) {
	t.Parallel()
	n := watchevents.Notification{ItemID: "item-1", Kind: watchevents.KindAssignment, AssignedUserID: "user-1"}
	if watchNotificationVisible(map[string]string{}, watchAccessVisibility{fullAccess: true}, "user-1", n) {
		t.Fatal("expected an assignment-to-you notification to be denied with no watch on the item")
	}
	if !watchNotificationVisible(map[string]string{"item-1": ""}, watchAccessVisibility{fullAccess: true}, "user-1", n) {
		t.Fatal("expected the same assignment to be visible to a caller holding an unconditional watch")
	}
}

func TestWatchNotificationVisible_AssignmentToSomeoneElseDenied(t *testing.T) {
	t.Parallel()
	watches := map[string]string{}
	n := watchevents.Notification{ItemID: "item-1", Kind: watchevents.KindAssignment, AssignedUserID: "user-2"}
	if watchNotificationVisible(watches, watchAccessVisibility{fullAccess: true}, "user-1", n) {
		t.Fatal("expected an assignment to someone else to be denied")
	}
}

// TestWatchNotificationVisible_AssignmentToSomeoneElseVisibleToWatcher
// pins the deliberate asymmetry between assignment and push after
// IDEA-2544 Phase 2. Push is private dispatch and is EXCLUSIVE of
// watch-matched delivery (see the Push* tests below): an unconditional
// watch must not leak a push addressed to someone else. An assignment is
// an ordinary item-level fact, so the opposite is true — a watcher is
// entitled to see who the item got assigned to, whoever that is. Without
// this test, deleting the KindAssignment fall-through in favour of a
// push-style `return n.AssignedUserID == userID` would look correct: the
// Phase 2 tests above would all still pass.
func TestWatchNotificationVisible_AssignmentToSomeoneElseVisibleToWatcher(t *testing.T) {
	t.Parallel()
	watches := map[string]string{"item-1": ""} // unconditional watch
	n := watchevents.Notification{ItemID: "item-1", Kind: watchevents.KindAssignment, AssignedUserID: "user-2"}
	if !watchNotificationVisible(watches, watchAccessVisibility{fullAccess: true}, "user-1", n) {
		t.Fatal("expected an unconditional watcher to see an assignment to someone else — an assignment is an item-level fact, not private dispatch")
	}
}

// TestWatchNotificationVisible_PushToYou mirrors
// TestWatchNotificationVisible_AddressedToYouIgnoresWatchList for
// KindPush (IDEA-2544 Phase 1): a push fires purely off TargetUserID,
// independent of any watch.
func TestWatchNotificationVisible_PushToYou(t *testing.T) {
	t.Parallel()
	watches := map[string]string{} // no watches at all
	n := watchevents.Notification{ItemID: "item-1", Kind: watchevents.KindPush, TargetUserID: "user-1"}
	if !watchNotificationVisible(watches, watchAccessVisibility{fullAccess: true}, "user-1", n) {
		t.Fatal("expected a push-to-you notification to be visible with no watch")
	}
}

// TestWatchNotificationVisible_PushToSomeoneElseDenied mirrors
// TestWatchNotificationVisible_AssignmentToSomeoneElseDenied for
// KindPush: Phase 1 only ever publishes self-addressed pushes, but the
// delivery rule itself must still deny a push addressed to a different
// user, exactly like the assignment branch.
func TestWatchNotificationVisible_PushToSomeoneElseDenied(t *testing.T) {
	t.Parallel()
	watches := map[string]string{}
	n := watchevents.Notification{ItemID: "item-1", Kind: watchevents.KindPush, TargetUserID: "user-2"}
	if watchNotificationVisible(watches, watchAccessVisibility{fullAccess: true}, "user-1", n) {
		t.Fatal("expected a push addressed to someone else to be denied")
	}
}

// TestWatchNotificationVisible_PushToSomeoneElseDeniedEvenWithUnconditionalWatch
// covers codex round 1's P1 finding: the original push branch returned
// true only on a match and otherwise FELL THROUGH to the watch-map
// check below it — so a non-target caller holding an unconditional
// watch on the item still received the push (instruction text
// included), because an unconditional watch matches "any notification
// on this item". TestWatchNotificationVisible_PushToSomeoneElseDenied
// used an EMPTY watch map, which never exercised that fall-through path
// at all. Push must be exclusive of watch-matched delivery: watching an
// item grants no claim on private dispatch someone else addressed to
// their own session — see the doc comment on the push branch in
// watchNotificationVisible.
func TestWatchNotificationVisible_PushToSomeoneElseDeniedEvenWithUnconditionalWatch(t *testing.T) {
	t.Parallel()
	watches := map[string]string{"item-1": ""} // unconditional watch on the item
	n := watchevents.Notification{ItemID: "item-1", Kind: watchevents.KindPush, TargetUserID: "user-2"}
	if watchNotificationVisible(watches, watchAccessVisibility{fullAccess: true}, "user-1", n) {
		t.Fatal("expected a push addressed to someone else to be denied even when the caller holds an unconditional watch on the item")
	}
}

// TestWatchNotificationVisible_PushToSomeoneElseDeniedEvenWithPredicateWatch
// is the predicated-watch variant of the same fall-through blind spot —
// same shape, a non-empty predicate instead of an unconditional watch.
func TestWatchNotificationVisible_PushToSomeoneElseDeniedEvenWithPredicateWatch(t *testing.T) {
	t.Parallel()
	watches := map[string]string{"item-1": "status=done"}
	n := watchevents.Notification{ItemID: "item-1", Kind: watchevents.KindPush, TargetUserID: "user-2"}
	if watchNotificationVisible(watches, watchAccessVisibility{fullAccess: true}, "user-1", n) {
		t.Fatal("expected a push addressed to someone else to be denied even when the caller holds a predicated watch on the item")
	}
}

// TestWatchNotificationVisible_PushStillGatedByAccess mirrors
// TestWatchNotificationVisible_AddressedToYouStillGatedByAccess: the
// SAME current-access check that gates every other kind must also gate
// a push-to-you notification, not just the assignment branch.
func TestWatchNotificationVisible_PushStillGatedByAccess(t *testing.T) {
	t.Parallel()
	watches := map[string]string{}
	deny := watchAccessVisibility{} // zero value: nothing visible
	n := watchevents.Notification{
		ItemID: "item-1", CollectionID: "coll-1",
		Kind: watchevents.KindPush, TargetUserID: "user-1",
	}
	if watchNotificationVisible(watches, deny, "user-1", n) {
		t.Fatal("expected a push-to-you notification to be denied when the caller has no current access to the item's collection")
	}

	allow := watchAccessVisibility{visibleCollIDs: map[string]bool{"coll-1": true}}
	if !watchNotificationVisible(watches, allow, "user-1", n) {
		t.Fatal("expected a push-to-you notification to be visible once the caller has current access to the item's collection")
	}
}

// TestWatchNotificationVisible_AssignmentStillGatedByAccess covers
// TASK-2533 codex round 2 finding 2: the addressed-to-you branch used to
// return true unconditionally, with NO access check — an item assigned
// to a "specific"-access member whose granted collections don't include
// it (validateAssignmentScope only checks workspace membership, never
// collection access — a completely ordinary, no-revocation-needed
// scenario) would still leak. A zero-value watchAccessVisibility (no
// full access, no granted collections/items) must deny an assignment
// notification just as it would any other kind.
//
// IDEA-2544 Phase 2 (TASK-2551) deleted that branch, so the specific
// leak is now structurally impossible — but the ordering rule it taught
// (access is checked FIRST, uniformly, before any per-kind logic) is
// still live and still worth pinning. The caller therefore holds an
// unconditional WATCH here: without it the denial leg would pass for the
// uninteresting reason that nothing matches at all (team CONVE-12), and
// the access check would never be the thing under test.
func TestWatchNotificationVisible_AssignmentStillGatedByAccess(t *testing.T) {
	t.Parallel()
	watches := map[string]string{"item-1": ""}
	deny := watchAccessVisibility{} // zero value: nothing visible
	n := watchevents.Notification{
		ItemID: "item-1", CollectionID: "coll-1",
		Kind: watchevents.KindAssignment, AssignedUserID: "user-1",
	}
	if watchNotificationVisible(watches, deny, "user-1", n) {
		t.Fatal("expected a watch-matched assignment notification to be denied when the caller has no current access to the item's collection")
	}

	allow := watchAccessVisibility{visibleCollIDs: map[string]bool{"coll-1": true}}
	if !watchNotificationVisible(watches, allow, "user-1", n) {
		t.Fatal("expected the same notification to be visible once the collection is in the caller's current access")
	}
}

// TestWatchEventsStream_AssignmentOutsideMemberCollectionAccessDenied is
// the HTTP/SSE-level regression for TASK-2533 codex round 2 finding 2:
// an item can be assigned to a "specific"-access member whose granted
// collections do NOT include it — validateAssignmentScope
// (internal/store/items.go) only checks WORKSPACE membership, never
// collection access, so this needs no revocation timing at all, just an
// ordinary assignment. The addressed-to-you path used to deliver it
// anyway with zero access check.
//
// Honest limitation after IDEA-2544 Phase 2 (TASK-2551), stated rather
// than left for a reader to assume otherwise (team CONVE-12): the
// restricted member now has TWO independent reasons not to receive this
// assignment — no access to "tasks" AND no watch on the item — and this
// test cannot separate them, because loadWatchPredicates drops watches
// on inaccessible items before the visibility check ever runs, so the
// setup that would isolate the access gate is unreachable over HTTP. It
// is kept as an end-to-end guard that the leak stays closed, not as
// evidence of WHICH gate closes it; the discriminating coverage for that
// is TestWatchNotificationVisible_AssignmentStillGatedByAccess (unit,
// watch present) and the mid-stream revocation tests in
// handlers_watch_vis_cache_test.go.
func TestWatchEventsStream_AssignmentOutsideMemberCollectionAccessDenied(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	slug := createWSWithCollections(t, srv)

	// A second collection the restricted member WILL be granted, distinct
	// from "tasks" where the assigned item actually lives.
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{
		"name": "Docs Only", "icon": "doc",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create second collection: %d %s", rr.Code, rr.Body.String())
	}
	var otherColl models.Collection
	parseJSON(t, rr, &otherColl)

	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items",
		map[string]interface{}{"title": "Assign me", "fields": `{"status":"open"}`})
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed item: %d %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)

	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil {
		t.Fatalf("GetWorkspaceBySlug: %v", err)
	}
	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "assign-owner@example.com", Name: "Owner", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser (owner): %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember (owner): %v", err)
	}
	ownerTok, err := srv.store.CreateAPIToken(owner.ID, models.APITokenCreate{Name: "assign-owner-test"}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken (owner): %v", err)
	}

	restricted, err := srv.store.CreateUser(models.UserCreate{
		Email: "assign-restricted@example.com", Name: "Restricted", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser (restricted): %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, restricted.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember (restricted): %v", err)
	}
	// Granted ONLY the "Docs Only" collection — NOT "tasks", where the
	// item being assigned actually lives.
	if err := srv.store.SetMemberCollectionAccess(ws.ID, restricted.ID, "specific", []string{otherColl.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}
	restrictedTok, err := srv.store.CreateAPIToken(restricted.ID, models.APITokenCreate{Name: "assign-restricted-test"}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken (restricted): %v", err)
	}

	// An item in the GRANTED collection, watched BEFORE connecting (the
	// watches map is loaded once at connect time and only reloaded on
	// the 60s reval tick — creating this after connecting would make the
	// "prove liveness" step below flaky/slow rather than deterministic).
	rr = bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/collections/"+otherColl.Slug+"/items", ownerTok.Token,
		map[string]interface{}{"title": "Visible item", "fields": "{}"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed visible item: %d %s", rr.Code, rr.Body.String())
	}
	var visibleItem models.Item
	parseJSON(t, rr, &visibleItem)
	if _, err := srv.store.CreateWatch(ws.ID, restricted.ID, visibleItem.ID, ""); err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := connectWatchStream(ctx, t, ts.URL, restrictedTok.Token)
	waitForWatchEvent(t, ch, 3*time.Second) // connected

	// Owner assigns the item (in "tasks") to the restricted member.
	// validateAssignmentScope only checks workspace membership, so this
	// succeeds even though the assignee can't see "tasks" at all.
	rr = bearerJSON(t, srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, ownerTok.Token,
		map[string]interface{}{"assigned_user_id": restricted.ID})
	if rr.Code != http.StatusOK {
		t.Fatalf("assign item: %d %s", rr.Code, rr.Body.String())
	}

	// Prove the stream is live and would have surfaced something: comment
	// on the watched, GRANTED item, then assert THAT arrives instead of
	// the assignment leaking first.
	rr = bearerJSON(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+visibleItem.Slug+"/comments", ownerTok.Token,
		map[string]interface{}{"body": "visible nudge"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("comment on visible item: %d %s", rr.Code, rr.Body.String())
	}

	ev := waitForWatchEvent(t, ch, 3*time.Second)
	var payload watchEventPayload
	if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if payload.ItemRef != visibleItem.Ref {
		t.Fatalf("expected the VISIBLE item's ref %q first, got %q (kind %q) — the out-of-scope assignment leaked",
			visibleItem.Ref, payload.ItemRef, payload.Kind)
	}
	if payload.Kind == watchevents.KindAssignment {
		t.Fatalf("assignment notification leaked to a member with no access to the item's collection: %+v", payload)
	}
}

// TestWatchEventsStream_ItemGrantGuestDoesNotSeeSiblingItem is the
// HTTP/SSE-level regression for TASK-2533 codex round 2 finding 1: a
// guest granted only item A must not receive stream notifications for
// an ungranted sibling item B in the SAME collection, even if a watch
// row exists on B (simulating access narrowed after the watch was
// created — the exact scenario the delivery-time filter, not the
// creation-time gate, has to catch).
func TestWatchEventsStream_ItemGrantGuestDoesNotSeeSiblingItem(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	slug := createWSWithCollections(t, srv)

	rrA := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items",
		map[string]interface{}{"title": "Granted item", "fields": `{"status":"open"}`})
	if rrA.Code != http.StatusCreated {
		t.Fatalf("seed item A: %d %s", rrA.Code, rrA.Body.String())
	}
	var itemA models.Item
	parseJSON(t, rrA, &itemA)

	rrB := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items",
		map[string]interface{}{"title": "Sibling item", "fields": `{"status":"open"}`})
	if rrB.Code != http.StatusCreated {
		t.Fatalf("seed item B: %d %s", rrB.Code, rrB.Body.String())
	}
	var itemB models.Item
	parseJSON(t, rrB, &itemB)
	if itemA.CollectionID != itemB.CollectionID {
		t.Fatalf("test setup bug: items must share a collection")
	}

	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil {
		t.Fatalf("GetWorkspaceBySlug: %v", err)
	}

	// Owner performs the mutations below — the guest below is deliberately
	// NOT granted edit access to item B, so it must not be the guest's
	// own token making that PATCH (it would be correctly rejected before
	// ever reaching the notification pipeline, which is not what this
	// test is exercising).
	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "sibling-stream-owner@example.com", Name: "Owner", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser (owner): %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember (owner): %v", err)
	}
	ownerTok, err := srv.store.CreateAPIToken(owner.ID, models.APITokenCreate{Name: "sibling-owner-test"}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken (owner): %v", err)
	}

	guest, err := srv.store.CreateUser(models.UserCreate{
		Email: "sibling-stream-guest@example.com", Name: "Sibling Stream Guest", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser (guest): %v", err)
	}
	if _, err := srv.store.CreateItemGrant(ws.ID, itemA.ID, guest.ID, "view", guest.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}
	guestTok, err := srv.store.CreateAPIToken(guest.ID, models.APITokenCreate{Name: "sibling-guest-test"}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken (guest): %v", err)
	}

	// Watches on BOTH items, created directly at the store layer —
	// simulating a watch on B that predates a since-narrowed grant
	// (the API's creation-time requireItemVisible gate would correctly
	// refuse to let the guest create this watch on B today; the
	// delivery-time filter is what has to catch a STALE one).
	if _, err := srv.store.CreateWatch(ws.ID, guest.ID, itemA.ID, ""); err != nil {
		t.Fatalf("CreateWatch(A): %v", err)
	}
	if _, err := srv.store.CreateWatch(ws.ID, guest.ID, itemB.ID, ""); err != nil {
		t.Fatalf("CreateWatch(B): %v", err)
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := connectWatchStream(ctx, t, ts.URL, guestTok.Token)
	waitForWatchEvent(t, ch, 3*time.Second) // connected

	// Mutate the UNGRANTED sibling B first — must not surface.
	rr := bearerJSON(t, srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+itemB.Slug, ownerTok.Token,
		map[string]interface{}{"fields": `{"status":"done"}`})
	if rr.Code != http.StatusOK {
		t.Fatalf("update item B: %d %s", rr.Code, rr.Body.String())
	}
	// Then mutate the GRANTED item A — must surface, proving the stream
	// is live and B's absence above wasn't just "nothing happened yet".
	rr = bearerJSON(t, srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+itemA.Slug, ownerTok.Token,
		map[string]interface{}{"fields": `{"status":"done"}`})
	if rr.Code != http.StatusOK {
		t.Fatalf("update item A: %d %s", rr.Code, rr.Body.String())
	}

	ev := waitForWatchEvent(t, ch, 3*time.Second)
	var payload watchEventPayload
	if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if payload.ItemRef != itemA.Ref {
		t.Fatalf("expected the GRANTED item A's ref %q first, got %q — ungranted sibling B leaked", itemA.Ref, payload.ItemRef)
	}
}

func TestParseWatchPredicate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw        string
		field, val string
		ok         bool
	}{
		{"status=done", "status", "done", true},
		{"status=", "status", "", true},
		{"=done", "", "", false},
		{"nopair", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		field, val, ok := parseWatchPredicate(c.raw)
		if ok != c.ok || field != c.field || val != c.val {
			t.Errorf("parseWatchPredicate(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.raw, field, val, ok, c.field, c.val, c.ok)
		}
	}
}
