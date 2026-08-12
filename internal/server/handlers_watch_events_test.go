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

func TestWatchEventsStream_AddressedToYou_Assignment(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	slug, item, tok, user := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// No explicit watch — this must fire purely via addressed-to-you.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := connectWatchStream(ctx, t, ts.URL, tok.Token)
	waitForWatchEvent(t, ch, 3*time.Second) // connected

	rr := bearerJSON(t, srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, tok.Token,
		map[string]interface{}{"assigned_user_id": user.ID})
	if rr.Code != http.StatusOK {
		t.Fatalf("assign item: %d %s", rr.Code, rr.Body.String())
	}

	ev := waitForWatchEvent(t, ch, 3*time.Second)
	var payload watchEventPayload
	if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if payload.Kind != watchevents.KindAssignment {
		t.Fatalf("expected kind %q, got %q", watchevents.KindAssignment, payload.Kind)
	}
	if payload.ItemRef != item.Ref {
		t.Fatalf("expected item_ref %q, got %q", item.Ref, payload.ItemRef)
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
// handleUpdateItem entirely) must still surface an addressed-to-you
// assignment notification — proving the fix belongs at the store layer
// (LastMutation), not in each individual HTTP handler.
func TestWatchEventsStream_BulkAssignFires(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	slug, item, tok, user := setupWatchTestUser(t, srv)
	ts := httptest.NewServer(srv)
	defer ts.Close()

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
	if !watchNotificationVisible(watches, "user-1", n) {
		t.Fatal("expected an unconditional watch to match any notification on the item")
	}
}

func TestWatchNotificationVisible_UnwatchedItemDenied(t *testing.T) {
	t.Parallel()
	watches := map[string]string{"item-1": ""}
	n := watchevents.Notification{ItemID: "item-2", Kind: watchevents.KindComment}
	if watchNotificationVisible(watches, "user-1", n) {
		t.Fatal("expected an unwatched item's notification to be denied")
	}
}

func TestWatchNotificationVisible_PredicateGatesNonStatusKinds(t *testing.T) {
	t.Parallel()
	watches := map[string]string{"item-1": "status=done"}
	n := watchevents.Notification{ItemID: "item-1", Kind: watchevents.KindComment}
	if watchNotificationVisible(watches, "user-1", n) {
		t.Fatal("expected a predicated watch to suppress a comment notification")
	}
}

func TestWatchNotificationVisible_PredicateMatchesOnlyTargetValue(t *testing.T) {
	t.Parallel()
	watches := map[string]string{"item-1": "status=done"}
	wrong := watchevents.Notification{ItemID: "item-1", Kind: watchevents.KindStatusChange, StatusFieldKey: "status", ToStatus: "in-progress"}
	right := watchevents.Notification{ItemID: "item-1", Kind: watchevents.KindStatusChange, StatusFieldKey: "status", ToStatus: "done"}
	if watchNotificationVisible(watches, "user-1", wrong) {
		t.Fatal("expected the non-matching status transition to be denied")
	}
	if !watchNotificationVisible(watches, "user-1", right) {
		t.Fatal("expected the matching status transition to be visible")
	}
}

func TestWatchNotificationVisible_AddressedToYouIgnoresWatchList(t *testing.T) {
	t.Parallel()
	watches := map[string]string{} // no watches at all
	n := watchevents.Notification{ItemID: "item-1", Kind: watchevents.KindAssignment, AssignedUserID: "user-1"}
	if !watchNotificationVisible(watches, "user-1", n) {
		t.Fatal("expected an assignment-to-you notification to be visible with no watch")
	}
}

func TestWatchNotificationVisible_AssignmentToSomeoneElseDenied(t *testing.T) {
	t.Parallel()
	watches := map[string]string{}
	n := watchevents.Notification{ItemID: "item-1", Kind: watchevents.KindAssignment, AssignedUserID: "user-2"}
	if watchNotificationVisible(watches, "user-1", n) {
		t.Fatal("expected an assignment to someone else to be denied")
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
