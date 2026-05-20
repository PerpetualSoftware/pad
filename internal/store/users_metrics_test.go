package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestGetUserMetrics covers the three engagement metrics from T1547:
// days_since_write, writes_7d, collections_touched_30d.
func TestGetUserMetrics(t *testing.T) {
	s := testStore(t)
	alice := createTestUser(t, s, "alice@example.com", "Alice", "password123")
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Acme", Slug: "acme", OwnerID: alice.ID})
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	tasks := createTestCollection(t, s, ws.ID, "Tasks")
	ideas := createTestCollection(t, s, ws.ID, "Ideas")

	// One item in each of two collections — needed for the
	// collections_touched_30d DISTINCT count.
	taskItem := createTestItem(t, s, ws.ID, tasks.ID, "T1", "")
	ideaItem := createTestItem(t, s, ws.ID, ideas.ID, "I1", "")

	// Touch last_write_at to now via the production code path.
	s.TouchUserWrite(t.Context(), alice.ID)

	// Seed write activities. Five "created"/"updated" within the 7d
	// window, one "commented" within the window, one "updated" 60 days
	// ago (outside both windows).
	mk := func(docID, action, when string) {
		// Bypass logActivity helper — write directly so we control timestamp.
		if _, err := s.db.Exec(s.q(`
			INSERT INTO activities (id, workspace_id, document_id, action, actor, source, metadata, user_id, ip_address, user_agent, created_at)
			VALUES (?, ?, ?, ?, 'user', 'web', '{}', ?, NULL, NULL, ?)
		`), newID(), ws.ID, docID, action, alice.ID, when); err != nil {
			t.Fatalf("seed activity: %v", err)
		}
	}
	recent := time.Now().UTC().Format(time.RFC3339)
	ancient := time.Now().UTC().Add(-60 * 24 * time.Hour).Format(time.RFC3339)

	mk(taskItem.ID, "created", recent)
	mk(taskItem.ID, "updated", recent)
	mk(taskItem.ID, "updated", recent)
	mk(ideaItem.ID, "created", recent)
	mk(ideaItem.ID, "commented", recent)
	mk(taskItem.ID, "updated", ancient) // outside both windows

	m, err := s.GetUserMetrics(alice.ID)
	if err != nil {
		t.Fatalf("GetUserMetrics: %v", err)
	}

	// days_since_write — TouchUserWrite ran moments ago, so this is 0.
	if m.DaysSinceWrite == nil || *m.DaysSinceWrite != 0 {
		t.Errorf("days_since_write: want 0, got %v", m.DaysSinceWrite)
	}
	// writes_7d — 5 within window (created/updated/updated/created/commented).
	// The ancient row is excluded by the 7-day cutoff.
	if m.Writes7d != 5 {
		t.Errorf("writes_7d: want 5, got %d", m.Writes7d)
	}
	// collections_touched_30d — distinct collection_id from items written
	// in last 30d. Two collections touched (tasks + ideas).
	if m.CollectionsTouched30d != 2 {
		t.Errorf("collections_touched_30d: want 2, got %d", m.CollectionsTouched30d)
	}
}

// TestGetUserMetricsEmptyUser ensures a user with no activity returns
// zero counts and a nil days_since_write rather than an error.
func TestGetUserMetricsEmptyUser(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "fresh@example.com", "Fresh", "password123")
	m, err := s.GetUserMetrics(u.ID)
	if err != nil {
		t.Fatalf("GetUserMetrics on empty user: %v", err)
	}
	if m.DaysSinceWrite != nil {
		t.Errorf("days_since_write: want nil, got %d", *m.DaysSinceWrite)
	}
	if m.Writes7d != 0 {
		t.Errorf("writes_7d: want 0, got %d", m.Writes7d)
	}
	if m.CollectionsTouched30d != 0 {
		t.Errorf("collections_touched_30d: want 0, got %d", m.CollectionsTouched30d)
	}
}
