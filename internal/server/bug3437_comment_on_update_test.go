package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// BUG-3437: the timeline's "commented on update" label keyed on the comment's
// activity_id, and EVERY comment has one: a comment or reply written on its own
// links to its own "commented" activity. The timeline now says which: an
// entry's comment_on_update is true only when an item update carried the
// comment. The web, the CLI (`pad item comment`) and MCP all write a comment
// through the same comments route, so the standalone leg covers all three.
func TestBug3437_CommentOnUpdateOnlyForAnUpdatesComment(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)
	item := timelineItemWithStructured(t, srv, ws, "", "")

	standalone := postComment(t, srv, ws, item.Slug, "", "a plain comment")
	reply := postReply(t, srv, ws, standalone.ID, "", "a plain reply")

	// An update that carries a comment, the way `pad item update --comment` sends it.
	rr := doAttributionRequest(t, srv, "", "PATCH", "/api/v1/workspaces/"+ws+"/items/"+item.Slug,
		map[string]any{"title": item.Title + " (renamed)", "comment": "why I renamed it"})
	if rr.Code != http.StatusOK {
		t.Fatalf("update with comment: %d %s", rr.Code, rr.Body.String())
	}
	var updated struct {
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &updated); err != nil || updated.Slug == "" {
		t.Fatalf("update response: %v %s", err, rr.Body.String())
	}
	item.Slug = updated.Slug // a rename moves the slug

	resp := fetchTimeline(t, srv, ws, item.Slug, "")
	var onUpdateID string
	for _, e := range resp.Entries {
		if e.Kind == "comment" && e.Comment != nil && e.Comment.Body == "why I renamed it" {
			onUpdateID = e.ID
			if !e.CommentOnUpdate {
				t.Errorf("the update's comment: comment_on_update = false, want true")
			}
		}
	}
	if onUpdateID == "" {
		t.Fatalf("the update's comment is not on the timeline: %+v", resp.Entries)
	}
	entry := commentEntryByID(resp.Entries, standalone.ID)
	if entry == nil {
		t.Fatalf("standalone comment not on the timeline")
	}
	if entry.CommentOnUpdate {
		t.Errorf("a standalone comment: comment_on_update = true, want false")
	}
	if entry.Comment.ActivityID == "" {
		t.Errorf("control: the standalone comment has no activity_id, so keying on it would not have failed")
	}
	// The reply is nested under its parent, and the label is a top-level
	// entry's: a reply never becomes an entry with the flag.
	for _, r := range entry.Comment.Replies {
		if r.ID == reply.ID && r.ActivityID == "" {
			t.Errorf("control: the reply has no activity_id")
		}
	}

	// The comments API and MCP keep their shape: the linked action is never serialized.
	rr = doAttributionRequest(t, srv, "", "GET", "/api/v1/workspaces/"+ws+"/items/"+item.Slug+"/comments", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list comments: %d", rr.Code)
	}
	body := rr.Body.String()
	for _, k := range []string{"linked_activity_action", "LinkedActivityAction", "comment_on_update"} {
		if strings.Contains(body, k) {
			t.Errorf("the comments API now carries %q", k)
		}
	}
	var list []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		var wrapped map[string][]map[string]any
		if err2 := json.Unmarshal(rr.Body.Bytes(), &wrapped); err2 != nil {
			t.Fatalf("decode comments: %v / %v", err, err2)
		}
	}
}
