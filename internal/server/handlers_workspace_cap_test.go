package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestWorkspaceCap_FreeTierAllowsUpToThree verifies that a free-tier user
// in cloud mode can create exactly 3 workspaces — all three must return 201.
// TASK-1609: cap reduced from 5 to 3.
func TestWorkspaceCap_FreeTierAllowsUpToThree(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode("test-cloud-secret")

	// Bootstrap admin so auth gates open.
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	// Create a free-tier user.
	u, err := srv.store.CreateUser(models.UserCreate{
		Email:    "free@test.com",
		Name:     "Free User",
		Password: "correct-horse-battery-staple",
		Role:     "member",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := srv.store.SetUserPlan(u.ID, "free", ""); err != nil {
		t.Fatalf("SetUserPlan(free): %v", err)
	}
	token := loginUser(t, srv, "free@test.com", "correct-horse-battery-staple")

	for i := 1; i <= 3; i++ {
		rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{
			"name": "Workspace",
		}, token)
		if rr.Code != http.StatusCreated {
			t.Fatalf("workspace %d of 3: expected 201, got %d: %s", i, rr.Code, rr.Body.String())
		}
	}
}

// TestWorkspaceCap_FreeTierBlocksFourth verifies that the 4th workspace
// creation attempt for a free-tier user in cloud mode returns 403 with
// error code plan_limit_exceeded. TASK-1609: cap is 3, not 5.
func TestWorkspaceCap_FreeTierBlocksFourth(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode("test-cloud-secret")

	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	u, err := srv.store.CreateUser(models.UserCreate{
		Email:    "free@test.com",
		Name:     "Free User",
		Password: "correct-horse-battery-staple",
		Role:     "member",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := srv.store.SetUserPlan(u.ID, "free", ""); err != nil {
		t.Fatalf("SetUserPlan(free): %v", err)
	}
	token := loginUser(t, srv, "free@test.com", "correct-horse-battery-staple")

	// Create 3 workspaces (all allowed).
	for i := 1; i <= 3; i++ {
		rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{
			"name": "Workspace",
		}, token)
		if rr.Code != http.StatusCreated {
			t.Fatalf("setup workspace %d: expected 201, got %d: %s", i, rr.Code, rr.Body.String())
		}
	}

	// 4th attempt must be blocked.
	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{
		"name": "Fourth Workspace",
	}, token)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("4th workspace: expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 403 response: %v", err)
	}
	if got, _ := resp["error"].(string); got != "plan_limit_exceeded" {
		t.Errorf("4th workspace: error=%q, want plan_limit_exceeded", got)
	}
	if got, _ := resp["feature"].(string); got != "workspaces" {
		t.Errorf("4th workspace: feature=%q, want workspaces", got)
	}
	// Limit field must report 3 (the new cap), not 5.
	if got, ok := resp["limit"].(float64); !ok || int(got) != 3 {
		t.Errorf("4th workspace: limit=%v, want 3", resp["limit"])
	}
}

// TestWorkspaceCap_ProTierUnlimited verifies that a pro-tier user in cloud
// mode can create more than 3 workspaces without being blocked.
func TestWorkspaceCap_ProTierUnlimited(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode("test-cloud-secret")

	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	u, err := srv.store.CreateUser(models.UserCreate{
		Email:    "pro@test.com",
		Name:     "Pro User",
		Password: "correct-horse-battery-staple",
		Role:     "member",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := srv.store.SetUserPlan(u.ID, "pro", ""); err != nil {
		t.Fatalf("SetUserPlan(pro): %v", err)
	}
	token := loginUser(t, srv, "pro@test.com", "correct-horse-battery-staple")

	// Create 5 workspaces — all must succeed on pro.
	for i := 1; i <= 5; i++ {
		rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{
			"name": "Pro Workspace",
		}, token)
		if rr.Code != http.StatusCreated {
			t.Fatalf("pro workspace %d: expected 201, got %d: %s", i, rr.Code, rr.Body.String())
		}
	}
}

// TestWorkspaceCap_SelfHostedNoLimit confirms that without SetCloudMode the
// workspace cap is not enforced — any number of workspaces can be created.
func TestWorkspaceCap_SelfHostedNoLimit(t *testing.T) {
	srv := testServer(t) // no SetCloudMode → self-hosted mode
	// No auth required when no users exist yet.
	for i := 1; i <= 5; i++ {
		rr := doRequest(srv, "POST", "/api/v1/workspaces", map[string]string{
			"name": "Self-hosted Workspace",
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("self-hosted workspace %d: expected 201, got %d: %s", i, rr.Code, rr.Body.String())
		}
	}
}

// TestWorkspaceCap_OverrideUnblocks verifies that a per-user plan_overrides
// entry raising the workspace cap beyond 3 allows a free-tier user to create
// a 4th workspace in cloud mode.
func TestWorkspaceCap_OverrideUnblocks(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode("test-cloud-secret")

	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	u, err := srv.store.CreateUser(models.UserCreate{
		Email:    "override@test.com",
		Name:     "Override User",
		Password: "correct-horse-battery-staple",
		Role:     "member",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := srv.store.SetUserPlan(u.ID, "free", ""); err != nil {
		t.Fatalf("SetUserPlan(free): %v", err)
	}
	// Override raises the workspace cap to 10.
	if err := srv.store.SetUserPlanOverrides(u.ID, `{"workspaces":10}`); err != nil {
		t.Fatalf("SetUserPlanOverrides: %v", err)
	}
	token := loginUser(t, srv, "override@test.com", "correct-horse-battery-staple")

	// Create 4 workspaces — all must succeed because override is 10.
	for i := 1; i <= 4; i++ {
		rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{
			"name": "Override Workspace",
		}, token)
		if rr.Code != http.StatusCreated {
			t.Fatalf("override workspace %d: expected 201, got %d: %s", i, rr.Code, rr.Body.String())
		}
	}
}
