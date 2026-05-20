package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestListUserActivity seeds a small mix of activities for two users and
// verifies the per-user filter + ordering + pagination behavior backing
// the /admin/users/{id}/activity endpoint. PLAN-1542 / TASK-1546.
func TestListUserActivity(t *testing.T) {
	s := testStore(t)
	alice := createTestUser(t, s, "alice@example.com", "Alice", "password123")
	bob := createTestUser(t, s, "bob@example.com", "Bob", "password123")
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Acme", Slug: "acme", OwnerID: alice.ID})
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}

	// Seed: 3 activities for alice (one of each action), 1 for bob.
	mk := func(uid, action string) {
		if _, err := s.CreateActivity(models.Activity{
			WorkspaceID: ws.ID,
			Action:      action,
			Actor:       "user",
			Source:      "web",
			Metadata:    "{}",
			UserID:      uid,
		}); err != nil {
			t.Fatalf("create activity: %v", err)
		}
	}
	mk(alice.ID, "created")
	mk(alice.ID, "updated")
	mk(alice.ID, "commented")
	mk(bob.ID, "created")

	// All-alice
	got, err := s.ListUserActivity(alice.ID, models.ActivityListParams{Limit: 50})
	if err != nil {
		t.Fatalf("ListUserActivity: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("alice should have 3 events; got %d", len(got))
	}
	for _, a := range got {
		if a.UserID != alice.ID {
			t.Fatalf("cross-user leak: %+v", a)
		}
	}

	// Action filter
	got, err = s.ListUserActivity(alice.ID, models.ActivityListParams{Action: "commented"})
	if err != nil {
		t.Fatalf("ListUserActivity action: %v", err)
	}
	if len(got) != 1 || got[0].Action != "commented" {
		t.Fatalf("action filter: want 1 commented; got %d %+v", len(got), got)
	}

	// Pagination
	page1, err := s.ListUserActivity(alice.ID, models.ActivityListParams{Limit: 2})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("page1 size: want 2 got %d", len(page1))
	}
	page2, err := s.ListUserActivity(alice.ID, models.ActivityListParams{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != 1 {
		t.Fatalf("page2 size: want 1 got %d", len(page2))
	}
	// No overlap between pages.
	if page1[0].ID == page2[0].ID || page1[1].ID == page2[0].ID {
		t.Fatalf("pagination overlap: page1=%v page2=%v", page1, page2)
	}

	// Inner safety cap at 100: ask for 10000, get at most 100 (we seeded
	// 3, so 3 here). The handler caps per-page at 50 + 1 probe; this
	// inner cap is just protection against pathological internal callers.
	got, err = s.ListUserActivity(alice.ID, models.ActivityListParams{Limit: 10000})
	if err != nil {
		t.Fatalf("cap: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("cap: want 3 got %d", len(got))
	}

	// Regression for Codex finding: handler's limit+1 probe at the public
	// max (50) must reach the store untruncated. Seed 51 activities,
	// request 51 from the store, expect 51 rows.
	for i := 0; i < 48; i++ {
		mk(alice.ID, "updated")
	}
	got, err = s.ListUserActivity(alice.ID, models.ActivityListParams{Limit: 51})
	if err != nil {
		t.Fatalf("51-probe: %v", err)
	}
	if len(got) != 51 {
		t.Fatalf("51-probe: want 51 (alice has 51 activities); got %d", len(got))
	}
}
