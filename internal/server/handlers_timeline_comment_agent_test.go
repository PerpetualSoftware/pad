package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-2760. An agent's comment rendered under the human's name because the
// timeline payload carried the comment and dropped the one row holding the
// agent's name (its linked `commented` activity). These drive the real create
// path — X-Pad-Agent header in, timeline and comments endpoints out — so they
// assert the BINDING from header to payload, not the store query alone
// (CONVE-19). Each has a control leg: the same request without the header
// must yield no name, or a hardcoded value would pass.

func postComment(t *testing.T, srv *Server, ws, itemSlug, agent, body, parentID string) models.Comment {
	t.Helper()
	payload := map[string]any{"body": body}
	if parentID != "" {
		payload["parent_id"] = parentID
	}
	rr := doAttributionRequest(t, srv, agent, "POST",
		"/api/v1/workspaces/"+ws+"/items/"+itemSlug+"/comments", payload)
	var c models.Comment
	decodeAttributionBody(t, rr, &c)
	return c
}

func commentEntryByID(entries []models.TimelineEntry, id string) *models.TimelineEntry {
	for i := range entries {
		if entries[i].Kind == "comment" && entries[i].ID == id {
			return &entries[i]
		}
	}
	return nil
}

func TestTimeline_CommentEntryCarriesAgentName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		agent string
		want  string
	}{
		{name: "agent comment", agent: "wren", want: "wren"},
		{name: "generic client id verbatim", agent: "claude-code", want: "claude-code"},
		{name: "human comment (control)", agent: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := testServer(t)
			ws := createTestWorkspaceViaAPI(t, srv)
			item := timelineItemWithStructured(t, srv, ws, "", "")

			c := postComment(t, srv, ws, item.Slug, tc.agent, "hello", "")

			resp := fetchTimeline(t, srv, ws, item.Slug, "")
			entry := commentEntryByID(resp.Entries, c.ID)
			if entry == nil {
				t.Fatalf("comment %s not in timeline: %+v", c.ID, resp.Entries)
			}
			if entry.AgentName != tc.want {
				t.Errorf("entry.agent_name = %q, want %q", entry.AgentName, tc.want)
			}
			if entry.Comment == nil || entry.Comment.AgentName != tc.want {
				t.Errorf("entry.comment.agent_name = %v, want %q", entry.Comment, tc.want)
			}
			// The two are one value: the entry field is derived from the
			// nested comment's, never set independently.
			if entry.Comment != nil && entry.AgentName != entry.Comment.AgentName {
				t.Errorf("entry.agent_name %q != entry.comment.agent_name %q", entry.AgentName, entry.Comment.AgentName)
			}
			// The human account the write rode on stays its own fact.
			if entry.ActorName != c.Author {
				t.Errorf("entry.actor_name = %q, want the comment author %q", entry.ActorName, c.Author)
			}
			// The linked activity is still suppressed — the comment card
			// stands in for it. Carrying the name must not have re-opened
			// the double-card problem the suppression exists to solve.
			for _, e := range resp.Entries {
				if e.Kind == "activity" && e.ID == c.ActivityID {
					t.Errorf("the comment's linked activity %s leaked into the payload", c.ActivityID)
				}
			}
		})
	}
}

// Replies are nested Comment objects under the parent's entry, not entries of
// their own — the reason the field lives on models.Comment and not only on
// TimelineEntry. A reply's name has to arrive through the nested object.
func TestTimeline_ReplyCarriesAgentName(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)
	item := timelineItemWithStructured(t, srv, ws, "", "")

	parent := postComment(t, srv, ws, item.Slug, "", "question", "")
	reply := postComment(t, srv, ws, item.Slug, "rook", "answer", parent.ID)

	resp := fetchTimeline(t, srv, ws, item.Slug, "")
	entry := commentEntryByID(resp.Entries, parent.ID)
	if entry == nil || entry.Comment == nil {
		t.Fatalf("parent comment %s not in timeline: %+v", parent.ID, resp.Entries)
	}
	if entry.AgentName != "" {
		t.Errorf("human parent got agent_name %q", entry.AgentName)
	}
	if len(entry.Comment.Replies) != 1 {
		t.Fatalf("replies = %+v, want the one reply nested", entry.Comment.Replies)
	}
	if got := entry.Comment.Replies[0]; got.ID != reply.ID || got.AgentName != "rook" {
		t.Errorf("nested reply = %+v, want id %s with agent_name %q", got, reply.ID, "rook")
	}
}

// The comments endpoint shares the store read path, so it carries the same
// field — one mechanism, every surface (`pad item comments` reads this).
func TestListComments_CarriesAgentName(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	ws := createTestWorkspaceViaAPI(t, srv)
	item := timelineItemWithStructured(t, srv, ws, "", "")

	named := postComment(t, srv, ws, item.Slug, "wren", "by agent", "")
	human := postComment(t, srv, ws, item.Slug, "", "by human", "")

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items/"+item.Slug+"/comments", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET comments = %d: %s", rr.Code, rr.Body.String())
	}
	var comments []models.Comment
	parseJSON(t, rr, &comments)

	got := map[string]string{}
	for _, c := range comments {
		got[c.ID] = c.AgentName
	}
	if got[named.ID] != "wren" || got[human.ID] != "" || len(got) != 2 {
		t.Errorf("agent names = %v, want %s→wren, %s→\"\"", got, named.ID, human.ID)
	}
}
