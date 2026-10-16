package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

// BUG-3773: a stream resolves each workspace's slug once per reval tick,
// not once per notification. The memo is observed through the payload: a
// workspace soft-deleted mid-tick still reads by its slug until reset(),
// and a failed resolve is retried rather than remembered.
func TestBUG3773WatchPayloadSlugIsMemoisedPerTick(t *testing.T) {
	srv := testServerWithWatchEvents(t)
	user, err := srv.store.CreateUser(models.UserCreate{Email: "bug3773@example.com", Name: "Watcher", Password: "pass-bug3773-xyz"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Slug Memo 3773"})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	cache := newWatchVisCache(srv, httptest.NewRequest(http.MethodGet, "/api/v1/watch/events", nil), user)
	n := watchevents.Notification{ID: 1, WorkspaceID: ws.ID, Kind: watchevents.KindStatusChange, ItemRef: "TASK-1"}

	if got := watchEventPayloadFor(cache, n).Workspace; got != ws.Slug {
		t.Fatalf("first payload workspace %q, want slug %q", got, ws.Slug)
	}
	if err := srv.store.DeleteWorkspace(ws.Slug); err != nil {
		t.Fatalf("delete workspace: %v", err)
	}
	// Same tick: served from the memo, with no store read that would see
	// the deletion.
	if got := watchEventPayloadFor(cache, n).Workspace; got != ws.Slug {
		t.Fatalf("payload in the same tick read the store again: workspace %q, want memoised slug %q", got, ws.Slug)
	}
	// Next tick: the memo is gone, and a deleted workspace degrades to its id.
	cache.reset()
	if got := watchEventPayloadFor(cache, n).Workspace; got != ws.ID {
		t.Fatalf("payload after reset: workspace %q, want the raw id %q", got, ws.ID)
	}
	// That failed resolve was not memoised: once the workspace is restored,
	// the very next payload in the same tick carries the slug again.
	if err := srv.store.RestoreWorkspace(ws.Slug); err != nil {
		t.Fatalf("restore workspace: %v", err)
	}
	if got := watchEventPayloadFor(cache, n).Workspace; got != ws.Slug {
		t.Fatalf("payload after restore: workspace %q, want slug %q (a failed resolve was memoised)", got, ws.Slug)
	}
}
