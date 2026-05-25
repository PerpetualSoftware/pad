package server

import (
	"encoding/json"
	"fmt"
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
	// TASK-788: body now follows the standard {"error":{"code":...,"message":...,"details":{...}}} shape.
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details struct {
				Feature string  `json:"feature"`
				Limit   float64 `json:"limit"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 403 response: %v", err)
	}
	if resp.Error.Code != "plan_limit_exceeded" {
		t.Errorf("4th workspace: error.code=%q, want plan_limit_exceeded", resp.Error.Code)
	}
	if resp.Error.Message == "" {
		t.Errorf("4th workspace: error.message should be non-empty")
	}
	if resp.Error.Details.Feature != "workspaces" {
		t.Errorf("4th workspace: error.details.feature=%q, want workspaces", resp.Error.Details.Feature)
	}
	// Limit field must report 3 (the new cap), not 5.
	if int(resp.Error.Details.Limit) != 3 {
		t.Errorf("4th workspace: error.details.limit=%v, want 3", resp.Error.Details.Limit)
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

// TestPlanLimitError_ResponseShape verifies that the 403 body emitted by
// writePlanLimitError follows the standard {"error":{"code":...,"message":...,"details":{...}}}
// envelope shape (TASK-788). Uses the member-invite endpoint to exercise
// enforcePlanLimit("members_per_workspace") — a workspace-scoped limit distinct
// from the workspace-count limit tested by TestWorkspaceCap_FreeTierBlocksFourth.
func TestPlanLimitError_ResponseShape(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode("test-cloud-secret")

	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	// Create a free-tier user who will own the workspace.
	owner, err := srv.store.CreateUser(models.UserCreate{
		Email:    "owner@test.com",
		Name:     "Owner",
		Password: "correct-horse-battery-staple",
		Role:     "member",
	})
	if err != nil {
		t.Fatalf("CreateUser(owner): %v", err)
	}
	if err := srv.store.SetUserPlan(owner.ID, "free", ""); err != nil {
		t.Fatalf("SetUserPlan(free): %v", err)
	}
	ownerToken := loginUser(t, srv, "owner@test.com", "correct-horse-battery-staple")

	// Create a workspace for the owner.
	wsRR := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{
		"name": "Test Workspace",
	}, ownerToken)
	if wsRR.Code != http.StatusCreated {
		t.Fatalf("create workspace: expected 201, got %d: %s", wsRR.Code, wsRR.Body.String())
	}
	var wsResp struct {
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(wsRR.Body.Bytes(), &wsResp); err != nil || wsResp.Slug == "" {
		t.Fatalf("parse workspace response: %v — %s", err, wsRR.Body.String())
	}
	wsSlug := wsResp.Slug

	// Add existing users until we're at the member cap (3 for free tier).
	// The owner themselves count as 1, so invite 2 more real users.
	for i := 2; i <= 3; i++ {
		member, err := srv.store.CreateUser(models.UserCreate{
			Email:    fmt.Sprintf("member%d@test.com", i),
			Name:     fmt.Sprintf("Member %d", i),
			Password: "pass",
			Role:     "member",
		})
		if err != nil {
			t.Fatalf("CreateUser(member%d): %v", i, err)
		}
		invRR := doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/members/invite",
			map[string]string{"email": member.Email, "role": "editor"}, ownerToken)
		if invRR.Code != http.StatusCreated {
			t.Fatalf("invite member %d: expected 201, got %d: %s", i, invRR.Code, invRR.Body.String())
		}
	}

	// One more invite must be blocked — this is the limit-hit we're testing.
	extra, err := srv.store.CreateUser(models.UserCreate{
		Email:    "extra@test.com",
		Name:     "Extra",
		Password: "pass",
		Role:     "member",
	})
	if err != nil {
		t.Fatalf("CreateUser(extra): %v", err)
	}
	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/members/invite",
		map[string]string{"email": extra.Email, "role": "editor"}, ownerToken)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("4th member invite: expected 403, got %d: %s", rr.Code, rr.Body.String())
	}

	// Verify the standard error envelope shape (TASK-788).
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details struct {
				Feature    string  `json:"feature"`
				Limit      float64 `json:"limit"`
				Current    float64 `json:"current"`
				Plan       string  `json:"plan"`
				UpgradeURL string  `json:"upgrade_url"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 403 response: %v — body: %s", err, rr.Body.String())
	}

	// error.code must be the stable machine-readable token.
	if resp.Error.Code != "plan_limit_exceeded" {
		t.Errorf("error.code: got %q, want plan_limit_exceeded", resp.Error.Code)
	}
	// error.message must be a non-empty human sentence suitable for display.
	if resp.Error.Message == "" {
		t.Errorf("error.message: should be non-empty human-readable text")
	}
	// Details must carry the structured limit info.
	if resp.Error.Details.Feature != "members_per_workspace" {
		t.Errorf("details.feature: got %q, want members_per_workspace", resp.Error.Details.Feature)
	}
	if int(resp.Error.Details.Limit) != 3 {
		t.Errorf("details.limit: got %v, want 3", resp.Error.Details.Limit)
	}
	if resp.Error.Details.Plan != "free" {
		t.Errorf("details.plan: got %q, want free", resp.Error.Details.Plan)
	}
	if resp.Error.Details.UpgradeURL != "/console/billing" {
		t.Errorf("details.upgrade_url: got %q, want /console/billing", resp.Error.Details.UpgradeURL)
	}
}
