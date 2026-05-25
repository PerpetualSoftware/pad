package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestCheckUserLimit_WorkspacesFreeTier exercises the workspace-count limit
// for a free-tier user. The cap must be 3 (DefaultFreeLimits.Workspaces).
//
//   - With 0 workspaces: Allowed=true, Current=0, Limit=3
//   - With 2 workspaces (below cap): Allowed=true, Current=2
//   - After inserting a 3rd workspace (at cap): Allowed=false, Current=3
func TestCheckUserLimit_WorkspacesFreeTier(t *testing.T) {
	s := testStore(t)

	owner := createTestUser(t, s, "owner@example.com", "Owner", "s3cret")
	if err := s.SetUserPlan(owner.ID, "free", ""); err != nil {
		t.Fatalf("SetUserPlan(free): %v", err)
	}

	// 0 workspaces — allowed, limit reported as 3.
	res, err := s.CheckUserLimit(owner.ID, "workspaces")
	if err != nil {
		t.Fatalf("CheckUserLimit (0 workspaces): %v", err)
	}
	if !res.Allowed {
		t.Errorf("0 workspaces: Allowed=false, want true")
	}
	if res.Limit != 3 {
		t.Errorf("0 workspaces: Limit=%d, want 3", res.Limit)
	}
	if res.Current != 0 {
		t.Errorf("0 workspaces: Current=%d, want 0", res.Current)
	}

	// Create 2 workspaces — still allowed, Current==2.
	for i := 0; i < 2; i++ {
		if _, err := s.CreateWorkspace(models.WorkspaceCreate{
			Name:    "ws-below-cap",
			OwnerID: owner.ID,
		}); err != nil {
			t.Fatalf("CreateWorkspace(%d): %v", i, err)
		}
	}
	res, err = s.CheckUserLimit(owner.ID, "workspaces")
	if err != nil {
		t.Fatalf("CheckUserLimit (2 workspaces): %v", err)
	}
	if !res.Allowed {
		t.Errorf("2 workspaces: Allowed=false, want true")
	}
	if res.Current != 2 {
		t.Errorf("2 workspaces: Current=%d, want 2", res.Current)
	}

	// Create a 3rd workspace — now at cap; next create should be denied.
	if _, err := s.CreateWorkspace(models.WorkspaceCreate{
		Name:    "ws-at-cap",
		OwnerID: owner.ID,
	}); err != nil {
		t.Fatalf("CreateWorkspace(3rd): %v", err)
	}
	res, err = s.CheckUserLimit(owner.ID, "workspaces")
	if err != nil {
		t.Fatalf("CheckUserLimit (3 workspaces): %v", err)
	}
	if res.Allowed {
		t.Errorf("3 workspaces: Allowed=true, want false (at cap)")
	}
	if res.Current != 3 {
		t.Errorf("3 workspaces: Current=%d, want 3", res.Current)
	}
	if res.Limit != 3 {
		t.Errorf("3 workspaces: Limit=%d, want 3", res.Limit)
	}
}

// TestCheckUserLimit_WorkspacesFreeWithOverride confirms that a per-user
// plan_overrides JSON entry raises the cap above the default 3.
func TestCheckUserLimit_WorkspacesFreeWithOverride(t *testing.T) {
	s := testStore(t)

	owner := createTestUser(t, s, "override@example.com", "Override", "s3cret")
	if err := s.SetUserPlan(owner.ID, "free", ""); err != nil {
		t.Fatalf("SetUserPlan(free): %v", err)
	}
	// Override raises cap to 10.
	if err := s.SetUserPlanOverrides(owner.ID, `{"workspaces":10}`); err != nil {
		t.Fatalf("SetUserPlanOverrides: %v", err)
	}

	// Create 4 workspaces (would exceed the 3-default but not the override).
	for i := 0; i < 4; i++ {
		if _, err := s.CreateWorkspace(models.WorkspaceCreate{
			Name:    "ws-override",
			OwnerID: owner.ID,
		}); err != nil {
			t.Fatalf("CreateWorkspace(%d) with override: %v", i, err)
		}
	}
	res, err := s.CheckUserLimit(owner.ID, "workspaces")
	if err != nil {
		t.Fatalf("CheckUserLimit with override: %v", err)
	}
	if !res.Allowed {
		t.Errorf("4 workspaces with override=10: Allowed=false, want true")
	}
	if res.Limit != 10 {
		t.Errorf("override limit: Limit=%d, want 10", res.Limit)
	}
}

// TestCheckUserLimit_WorkspacesProUnlimited confirms that pro-plan users
// are always allowed regardless of workspace count.
func TestCheckUserLimit_WorkspacesProUnlimited(t *testing.T) {
	s := testStore(t)

	owner := createTestUser(t, s, "pro@example.com", "Pro", "s3cret")
	if err := s.SetUserPlan(owner.ID, "pro", ""); err != nil {
		t.Fatalf("SetUserPlan(pro): %v", err)
	}

	// Create 5 workspaces — should all be allowed on pro.
	for i := 0; i < 5; i++ {
		if _, err := s.CreateWorkspace(models.WorkspaceCreate{
			Name:    "ws-pro",
			OwnerID: owner.ID,
		}); err != nil {
			t.Fatalf("CreateWorkspace(%d) on pro: %v", i, err)
		}
	}
	res, err := s.CheckUserLimit(owner.ID, "workspaces")
	if err != nil {
		t.Fatalf("CheckUserLimit (pro): %v", err)
	}
	if !res.Allowed {
		t.Errorf("pro plan (5 workspaces): Allowed=false, want true (unlimited)")
	}
	if res.Limit != -1 {
		t.Errorf("pro plan: Limit=%d, want -1 (unlimited)", res.Limit)
	}
}
