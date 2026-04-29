package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestAdminUpdateUser_StorageOverrideRoundTrip pins TASK-883's
// acceptance criterion: an admin PATCHes plan_overrides with a
// storage_bytes key, and a follow-up GET shows the new effective
// limit. The endpoint already accepted arbitrary keys in the
// plan_overrides JSON; this test guards against regressions in the
// admin user-detail page contract.
func TestAdminUpdateUser_StorageOverrideRoundTrip(t *testing.T) {
	srv := testServer(t)
	adminToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	// Seed a non-admin user so we have someone to set the override on.
	target, err := srv.store.CreateUser(models.UserCreate{
		Email:    "owner@test.com",
		Name:     "Owner",
		Password: "correct-horse-battery-staple",
		Role:     "member",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := srv.store.SetUserPlan(target.ID, "free", ""); err != nil {
		t.Fatalf("SetUserPlan: %v", err)
	}

	// PATCH with a storage_bytes override (1 GiB).
	const oneGiB = 1073741824
	body := map[string]any{
		"plan_overrides": `{"storage_bytes":` + itoa(oneGiB) + `}`,
	}
	rr := doRequestWithCookie(srv, "PATCH", "/api/v1/admin/users/"+target.ID, body, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch: status=%d body=%s", rr.Code, rr.Body.String())
	}

	// Round-trip: GET the user back and confirm the override stuck.
	rr = doRequestWithCookie(srv, "GET", "/api/v1/admin/users/"+target.ID, nil, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("get: status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		PlanOverrides string `json:"plan_overrides"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	if !strings.Contains(got.PlanOverrides, `"storage_bytes":1073741824`) {
		t.Errorf("plan_overrides=%q, want storage_bytes:1073741824", got.PlanOverrides)
	}

	// Audit log should have an entry. Read the admin audit feed and
	// look for the new ActionPlanOverridesChanged constant.
	rr = doRequestWithCookie(srv, "GET", "/api/v1/audit-log?limit=20", nil, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("audit-log: status=%d body=%s", rr.Code, rr.Body.String())
	}
	var entries []struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &entries); err != nil {
		t.Fatalf("decode audit-log: %v body=%s", err, rr.Body.String())
	}
	found := false
	for _, e := range entries {
		if e.Action == models.ActionPlanOverridesChanged {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("audit feed missing %q after override change; entries=%+v",
			models.ActionPlanOverridesChanged, entries)
	}

	// Clearing the override (empty string) should remove the
	// storage_bytes key — TASK-883 acceptance: empty input clears
	// the override. We send an empty plan_overrides JSON.
	body = map[string]any{"plan_overrides": ""}
	rr = doRequestWithCookie(srv, "PATCH", "/api/v1/admin/users/"+target.ID, body, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("clear patch: status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithCookie(srv, "GET", "/api/v1/admin/users/"+target.ID, nil, adminToken)
	got.PlanOverrides = ""
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode after clear: %v", err)
	}
	if got.PlanOverrides != "" && got.PlanOverrides != "{}" {
		t.Errorf("after clear: plan_overrides=%q, want empty or {}", got.PlanOverrides)
	}
}

// TestAdminUpdateUser_OmittedOverridesPreserved pins the contract
// the admin UI relies on: PATCH with no plan_overrides field at
// all (e.g. when only the role is being changed) MUST NOT clear
// existing overrides — the handler's nil-pointer check ensures
// that. Empty string ("" → SetUserPlanOverrides("")) is the
// explicit "clear" signal used by the Reset-to-default button.
//
// Codex caught the related bug on PR #304 round 1: the UI was
// sending null when all override fields were blank, which JSON-
// decoded to a nil pointer and the handler skipped the update.
// This test pins the OTHER half of the contract — that omitting
// the field is genuinely a no-op.
func TestAdminUpdateUser_OmittedOverridesPreserved(t *testing.T) {
	srv := testServer(t)
	adminToken := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	target, err := srv.store.CreateUser(models.UserCreate{
		Email:    "owner@test.com",
		Name:     "Owner",
		Password: "correct-horse-battery-staple",
		Role:     "member",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	const overrides = `{"storage_bytes":1073741824}`
	if err := srv.store.SetUserPlanOverrides(target.ID, overrides); err != nil {
		t.Fatalf("SetUserPlanOverrides: %v", err)
	}

	// PATCH with only the role field. plan_overrides is intentionally
	// absent from the body — the handler must leave the column alone.
	rr := doRequestWithCookie(srv, "PATCH", "/api/v1/admin/users/"+target.ID,
		map[string]any{"role": "member"}, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch role-only: status=%d body=%s", rr.Code, rr.Body.String())
	}

	got, err := srv.store.GetUser(target.ID)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if got.PlanOverrides != overrides {
		t.Errorf("after role-only patch: PlanOverrides=%q, want %q", got.PlanOverrides, overrides)
	}

	// PATCH with explicit empty string → clears the column.
	rr = doRequestWithCookie(srv, "PATCH", "/api/v1/admin/users/"+target.ID,
		map[string]any{"plan_overrides": ""}, adminToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch clear: status=%d body=%s", rr.Code, rr.Body.String())
	}
	got, err = srv.store.GetUser(target.ID)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if got.PlanOverrides != "" {
		t.Errorf("after empty-string patch: PlanOverrides=%q, want empty", got.PlanOverrides)
	}
}

// TestAdminUpdateUser_NonAdminForbidden pins the existing admin gate:
// a member-role user can't PATCH another user's plan_overrides.
// Already covered indirectly elsewhere — re-asserted here so the
// new audit-log path doesn't accidentally accept unauthorized
// requests through some other branch.
func TestAdminUpdateUser_NonAdminForbidden(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	memberToken := registerNonAdmin(t, srv, "member@test.com", "Member")

	target, err := srv.store.CreateUser(models.UserCreate{
		Email:    "victim@test.com",
		Name:     "Victim",
		Password: "correct-horse-battery-staple",
		Role:     "member",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	rr := doRequestWithCookie(srv, "PATCH", "/api/v1/admin/users/"+target.ID,
		map[string]any{"plan_overrides": `{"storage_bytes":1}`}, memberToken)
	if rr.Code != http.StatusForbidden {
		t.Errorf("non-admin patch: status=%d, want 403", rr.Code)
	}
}

// itoa is a tiny strconv.Itoa wrapper kept local to avoid an extra
// import in this single-file test.
func itoa(n int) string {
	buf := make([]byte, 0, 12)
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		buf = append(buf, byte('0'+n%10))
		n /= 10
	}
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	if neg {
		return "-" + string(buf)
	}
	return string(buf)
}
