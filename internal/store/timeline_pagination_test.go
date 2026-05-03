package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Regression coverage for BUG-1086.
//
// The timeline handler previously seeded `beforeID` with the literal byte "\xff"
// to act as a sentinel that "sorts after any UUID". That worked on SQLite (which
// doesn't validate TEXT against UTF-8), but Postgres rejected it with
// SQLSTATE 22021 — `invalid byte sequence for encoding "UTF8": 0xff`, surfacing
// as a 500 every time the timeline tab loaded on pad cloud.
//
// The fix: when there is no cursor (first page), the store omits the id
// tie-breaker from the WHERE clause entirely. These tests exercise both the
// no-cursor path (the previously-broken case) and the real-cursor path on
// whichever backend `testStore` is configured for, so `make test-pg` covers
// the actual failure mode.

// timelineFixture creates a workspace, collection, item, and three comments
// spaced 1 second apart. Returns the item plus the comment slice
// (ordered oldest → newest).
func timelineFixture(t *testing.T, s *Store) (*models.Item, []models.Comment) {
	t.Helper()
	ws := createTestWorkspace(t, s, "Timeline")
	coll := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, coll.ID, "Test item", "")

	var comments []models.Comment
	for i, body := range []string{"first", "second", "third"} {
		c, err := s.CreateComment(ws.ID, item.ID, models.CommentCreate{
			Body:      body,
			Author:    "Tester",
			CreatedBy: "user",
			Source:    "web",
		})
		if err != nil {
			t.Fatalf("create comment %d: %v", i, err)
		}
		comments = append(comments, *c)
		// CreatedAt is RFC3339 (second precision) — sleep so each comment
		// gets a distinct timestamp and pagination is deterministic.
		time.Sleep(1100 * time.Millisecond)
	}
	return item, comments
}

func TestListCommentsBeforeTime_NoCursor(t *testing.T) {
	s := testStore(t)
	item, comments := timelineFixture(t, s)

	// First-page query: no cursor. Pre-fix this passed beforeID = "\xff" and
	// blew up on Postgres with SQLSTATE 22021. Now we pass "".
	got, err := s.ListCommentsBeforeTime(item.ID, time.Now().UTC().Add(time.Minute), "", 10)
	if err != nil {
		t.Fatalf("ListCommentsBeforeTime first page: %v", err)
	}
	if len(got) != len(comments) {
		t.Fatalf("expected %d comments, got %d", len(comments), len(got))
	}
	// Newest first.
	if got[0].Body != "third" || got[len(got)-1].Body != "first" {
		t.Errorf("unexpected order: %q .. %q", got[0].Body, got[len(got)-1].Body)
	}
}

func TestListCommentsBeforeTime_WithCursor(t *testing.T) {
	s := testStore(t)
	item, comments := timelineFixture(t, s)

	// Paginate "before" the newest comment using a real (timestamp, id) cursor.
	cursor := comments[2] // newest
	got, err := s.ListCommentsBeforeTime(item.ID, cursor.CreatedAt, cursor.ID, 10)
	if err != nil {
		t.Fatalf("ListCommentsBeforeTime cursor page: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 comments before cursor, got %d", len(got))
	}
	if got[0].Body != "second" || got[1].Body != "first" {
		t.Errorf("unexpected order on cursor page: %q, %q", got[0].Body, got[1].Body)
	}
}

// TestListCommentsBeforeTime_SameSecondCursor exercises the path where a
// caller passes a `before` timestamp together with an upper-bound id sentinel
// (e.g. "g") rather than an exact cursor id. The query must still include
// rows whose created_at equals that second — equivalent to the legacy
// "before=now,beforeID=\xff" semantics that BUG-1086 broke.
func TestListCommentsBeforeTime_SameSecondCursor(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "SameSecond")
	coll := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, coll.ID, "Same-second", "")

	// Three comments back-to-back with no sleep — they will share a second
	// at RFC3339 precision (which is what the store stores).
	for _, body := range []string{"a", "b", "c"} {
		if _, err := s.CreateComment(ws.ID, item.ID, models.CommentCreate{
			Body:      body,
			Author:    "Tester",
			CreatedBy: "user",
			Source:    "web",
		}); err != nil {
			t.Fatalf("create %s: %v", body, err)
		}
	}

	// Cursor at "now" with "g" sentinel — must include all 3 same-second rows.
	got, err := s.ListCommentsBeforeTime(item.ID, time.Now().UTC(), "g", 10)
	if err != nil {
		t.Fatalf("ListCommentsBeforeTime same-second cursor: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("same-second cursor should include all 3 rows, got %d", len(got))
	}
}

func TestListCommentsBeforeTime_LimitRespected(t *testing.T) {
	s := testStore(t)
	item, _ := timelineFixture(t, s)

	got, err := s.ListCommentsBeforeTime(item.ID, time.Now().UTC().Add(time.Minute), "", 2)
	if err != nil {
		t.Fatalf("ListCommentsBeforeTime: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("limit=2 expected 2 rows, got %d", len(got))
	}
}

func TestListDocumentActivityBeforeTime_NoCursor(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Activity")
	doc := createTestDoc(t, s, ws.ID, "Doc", "content")

	for _, action := range []string{"created", "viewed", "edited"} {
		if _, err := s.CreateActivity(models.Activity{
			WorkspaceID: ws.ID,
			DocumentID:  doc.ID,
			Action:      action,
			Actor:       "user",
			Source:      "web",
		}); err != nil {
			t.Fatalf("create activity %s: %v", action, err)
		}
	}

	got, err := s.ListDocumentActivityBeforeTime(doc.ID, time.Now().UTC().Add(time.Minute), "", 10)
	if err != nil {
		t.Fatalf("ListDocumentActivityBeforeTime first page: %v", err)
	}
	if len(got) < 3 {
		t.Fatalf("expected at least 3 activities, got %d", len(got))
	}
}

func TestListItemVersionsBeforeTime_NoCursor(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Versions")
	coll := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, coll.ID, "Versioned", "v1 content")

	// UpdateItem creates a version snapshot. Two updates → two extra versions.
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Content: strPtr("v2 content")}); err != nil {
		t.Fatalf("update v2: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Content: strPtr("v3 content")}); err != nil {
		t.Fatalf("update v3: %v", err)
	}

	got, err := s.ListItemVersionsBeforeTime(item.ID, time.Now().UTC().Add(time.Minute), "", 10)
	if err != nil {
		t.Fatalf("ListItemVersionsBeforeTime first page: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("expected at least 1 version, got 0")
	}
}

func strPtr(s string) *string { return &s }
