package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3343: an item event whose collection is empty is not a
// workspace-level event. The comment and reaction publishers resolved the
// collection AFTER the write with a lookup that skips archived items, so a
// concurrent archive left it empty, and the SSE filter delivered "no
// collection" events to every member, collection-restricted ones included.

func TestBUG3343_ItemEventWithoutCollectionIsNotWorkspaceLevel(t *testing.T) {
	restricted := sseVisibility{
		visibleSlugSet:   map[string]bool{"tasks": true},
		visibleCollIDSet: map[string]bool{"tasks-id": true},
	}
	itemEvent := events.Event{Type: sseCommentCreated, WorkspaceID: "w", ItemID: "hidden-item", Title: "Hidden title"}
	if sseEventVisibleFor(restricted, "u", itemEvent) {
		t.Error("a restricted member received an item event that carries no collection")
	}
	guest := restricted
	guest.isGuest = true
	if sseEventVisibleFor(guest, "u", itemEvent) {
		t.Error("a guest received an item event that carries no collection")
	}
	// Controls: unrestricted members still get it, and a genuinely
	// workspace-level event (no item) still reaches restricted members.
	if !sseEventVisibleFor(sseVisibility{}, "u", itemEvent) {
		t.Error("control: an unrestricted member should receive the event")
	}
	if !sseEventVisibleFor(restricted, "u", events.Event{Type: "member_added", WorkspaceID: "w"}) {
		t.Error("control: a workspace-level event should still reach a restricted member")
	}
}

// The reaction publisher resolves the collection with archived items
// included, so an archive racing the write cannot blank it.
func TestBUG3343_ReactionEventKeepsCollectionOfArchivedItem(t *testing.T) {
	srv := testServerWithEvents(t)
	cookie := bootstrapFirstUser(t, srv, "owner@test.com", "Owner")
	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "Events"}, cookie)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)
	rr = doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/collections/tasks/items", map[string]any{"title": "Task"}, cookie)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: %d %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)
	rr = doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/items/"+item.Slug+"/comments", map[string]any{"body": "hi"}, cookie)
	if rr.Code != http.StatusCreated {
		t.Fatalf("comment: %d %s", rr.Code, rr.Body.String())
	}
	var comment models.Comment
	parseJSON(t, rr, &comment)

	// The archive lands between the reaction write and the publish.
	if err := srv.store.DeleteItem(item.ID); err != nil {
		t.Fatal(err)
	}
	ch, _, _ := srv.events.Subscribe(context.Background(), ws.ID)
	defer srv.events.Unsubscribe(ch)
	srv.publishReactionEvent(events.ReactionAdded, &comment)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type != events.ReactionAdded {
				continue
			}
			if ev.Collection != "tasks" {
				t.Fatalf("reaction event collection = %q, want tasks", ev.Collection)
			}
			return
		case <-deadline:
			t.Fatal("no reaction event published")
		}
	}
}
