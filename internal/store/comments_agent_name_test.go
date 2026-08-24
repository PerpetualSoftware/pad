package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-2760. A comment's agent name lives on the `commented` activity its
// activity_id points at, and nowhere else; the list queries LEFT JOIN it on so
// every comment list carries it. testStore runs these against SQLite by
// default and Postgres under `make test-pg` — the join and the jsonb scan
// must hold on both.

func agentNameFixture(t *testing.T) (*Store, *models.Item) {
	t.Helper()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "AgentName")
	coll := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, coll.ID, "Subject", "")
	return s, item
}

// commentWithActivity writes the pair the create handler writes: a
// `commented` activity carrying the given metadata, then the comment linked
// to it. An empty metadata leaves the comment unlinked (no activity at all).
func commentWithActivity(t *testing.T, s *Store, item *models.Item, metadata, body, parentID string) *models.Comment {
	t.Helper()
	input := models.CommentCreate{Body: body, Author: "Dave", CreatedBy: "agent", Source: "cli", ParentID: parentID}
	if metadata != "" {
		id, err := s.CreateActivity(models.Activity{
			WorkspaceID: item.WorkspaceID, DocumentID: item.ID,
			Action: "commented", Actor: "agent", Source: "cli", Metadata: metadata,
		})
		if err != nil {
			t.Fatalf("create activity: %v", err)
		}
		input.ActivityID = id
	}
	c, err := s.CreateComment(item.WorkspaceID, item.ID, "", input)
	if err != nil {
		t.Fatalf("create comment: %v", err)
	}
	return c
}

func agentNamesByID(comments []models.Comment) map[string]string {
	out := make(map[string]string, len(comments))
	for _, c := range comments {
		out[c.ID] = c.AgentName
	}
	return out
}

func TestListComments_AgentNameFromLinkedActivity(t *testing.T) {
	s, item := agentNameFixture(t)

	named := commentWithActivity(t, s, item, `{"agent":"wren"}`, "named", "")
	// A value the retired display filter would have swallowed — it must come
	// back verbatim, not blanked.
	generic := commentWithActivity(t, s, item, `{"agent":"claude-code"}`, "generic", "")
	stampless := commentWithActivity(t, s, item, `{"changes":"x"}`, "no stamp", "")
	emptyName := commentWithActivity(t, s, item, `{"agent":""}`, "empty", "")
	nonString := commentWithActivity(t, s, item, `{"agent":123}`, "non-string", "")
	unlinked := commentWithActivity(t, s, item, "", "unlinked", "")
	reply := commentWithActivity(t, s, item, `{"agent":"rook"}`, "reply", named.ID)

	want := map[string]string{
		named.ID: "wren", generic.ID: "claude-code", stampless.ID: "",
		emptyName.ID: "", nonString.ID: "", unlinked.ID: "", reply.ID: "rook",
	}

	all, err := s.ListComments(item.ID)
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if len(all) != len(want) {
		t.Fatalf("ListComments returned %d comments, want %d — the join must not drop or duplicate rows", len(all), len(want))
	}
	if got := agentNamesByID(all); !equalStringMaps(got, want) {
		t.Errorf("ListComments agent names = %v, want %v", got, want)
	}
	// The join must not disturb the query's own contract: chronological.
	for i := 1; i < len(all); i++ {
		if all[i].CreatedAt.Before(all[i-1].CreatedAt) {
			t.Errorf("ListComments order broken at %d: %v before %v", i, all[i].CreatedAt, all[i-1].CreatedAt)
		}
	}

	page, err := s.ListCommentsBeforeTime(item.ID, time.Now().Add(time.Hour), "", 50)
	if err != nil {
		t.Fatalf("ListCommentsBeforeTime: %v", err)
	}
	if len(page) != len(want) {
		t.Fatalf("ListCommentsBeforeTime returned %d comments, want %d", len(page), len(want))
	}
	if got := agentNamesByID(page); !equalStringMaps(got, want) {
		t.Errorf("ListCommentsBeforeTime agent names = %v, want %v", got, want)
	}
	// Newest-first, and the LIMIT still binds with the join in place.
	for i := 1; i < len(page); i++ {
		if page[i].CreatedAt.After(page[i-1].CreatedAt) {
			t.Errorf("ListCommentsBeforeTime order broken at %d: %v after %v", i, page[i].CreatedAt, page[i-1].CreatedAt)
		}
	}
	limited, err := s.ListCommentsBeforeTime(item.ID, time.Now().Add(time.Hour), "", 3)
	if err != nil {
		t.Fatalf("ListCommentsBeforeTime (limit 3): %v", err)
	}
	if len(limited) != 3 {
		t.Errorf("ListCommentsBeforeTime limit 3 returned %d rows", len(limited))
	}
	for _, c := range limited {
		if c.AgentName != want[c.ID] {
			t.Errorf("limited page: %s agent name = %q, want %q", c.ID, c.AgentName, want[c.ID])
		}
	}

	// The cursor branch is a separate query string; drive it too.
	cursor, err := s.ListCommentsBeforeTime(item.ID, time.Now().Add(time.Hour), "g", 50)
	if err != nil {
		t.Fatalf("ListCommentsBeforeTime (cursor): %v", err)
	}
	if got := agentNamesByID(cursor); !equalStringMaps(got, want) {
		t.Errorf("ListCommentsBeforeTime (cursor) agent names = %v, want %v", got, want)
	}

	// The single-row read is NOT enriched — Comment.AgentName documents the
	// list join as its only writer. This pins that so a future "why is it
	// empty here" is answered by the test, and so enriching it is a
	// deliberate change to this assertion rather than an accident.
	one, err := s.GetComment(named.ID)
	if err != nil || one == nil {
		t.Fatalf("GetComment: %v (%v)", err, one)
	}
	if one.AgentName != "" {
		t.Errorf("GetComment.AgentName = %q, want empty — only the list join populates it", one.AgentName)
	}
}

// The design's load-bearing claim: the name reaches the comment regardless of
// which activities a paginated activity read would return. Six later
// activities push the `commented` row out of a three-row activity window —
// the shape the timeline handler reads with perSource = limit*3 — while the
// comment query still carries the name, because it does not depend on that
// window at all. A handler-side join of the two lists would have lost it here.
func TestListCommentsBeforeTime_AgentNameSurvivesActivityWindow(t *testing.T) {
	s, item := agentNameFixture(t)

	c := commentWithActivity(t, s, item, `{"agent":"wren"}`, "early", "")
	// Timestamps are RFC3339 (second precision), so everything this test
	// writes lands in ONE second and the window's ORDER BY breaks the tie on
	// random ids — the commented row would then sit inside the window by
	// luck (it did, under Postgres, on the first full run). Backdate it so
	// the order is strict and the premise below is a fact, not a coin flip.
	if _, err := s.db.Exec(s.q(`UPDATE activities SET created_at = ? WHERE id = ?`),
		time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), c.ActivityID); err != nil {
		t.Fatalf("backdate activity: %v", err)
	}
	for i := 0; i < 6; i++ {
		if _, err := s.CreateActivity(models.Activity{
			WorkspaceID: item.WorkspaceID, DocumentID: item.ID,
			Action: "updated", Actor: "user", Source: "web", Metadata: `{"changes":"status"}`,
		}); err != nil {
			t.Fatalf("create activity %d: %v", i, err)
		}
	}

	future := time.Now().Add(time.Hour)
	window, err := s.ListDocumentActivityBeforeTime(item.ID, future, "", 3)
	if err != nil {
		t.Fatalf("ListDocumentActivityBeforeTime: %v", err)
	}
	for _, a := range window {
		if a.ID == c.ActivityID {
			t.Fatalf("premise broken: the commented activity is still inside the 3-row window; the test proves nothing")
		}
	}

	comments, err := s.ListCommentsBeforeTime(item.ID, future, "", 3)
	if err != nil {
		t.Fatalf("ListCommentsBeforeTime: %v", err)
	}
	if len(comments) != 1 || comments[0].AgentName != "wren" {
		t.Errorf("comments = %+v, want the one comment with AgentName %q", comments, "wren")
	}
}

func equalStringMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
