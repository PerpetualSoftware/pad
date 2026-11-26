package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3691: on Pad Cloud, app install confirm and app upgrade confirm must
// enforce the workspace's items_per_workspace plan limit, answering 403
// plan_limit_exceeded with nothing written and the pending record kept
// confirmable. Every refused case is paired with a control one item under the
// cap.

func bug3691OwnerID(t *testing.T, srv *Server) string {
	t.Helper()
	u, err := srv.store.GetUserByEmail("owner@test.com")
	if err != nil || u == nil {
		t.Fatalf("GetUserByEmail(owner@test.com) = %v, %v", u, err)
	}
	return u.ID
}

func bug3691SetPlanCap(t *testing.T, srv *Server, cloud bool, limit int) {
	t.Helper()
	ownerID := bug3691OwnerID(t, srv)
	srv.cloudMode = cloud
	if _, err := srv.store.SetUserPlan(ownerID, store.PlanWrite{Plan: "free", Source: store.PlanSourceManual, Force: true}); err != nil {
		t.Fatalf("SetUserPlan: %v", err)
	}
	if err := srv.store.SetUserPlanOverrides(ownerID, fmt.Sprintf(`{"items_per_workspace":%d}`, limit)); err != nil {
		t.Fatalf("SetUserPlanOverrides: %v", err)
	}
}

func bug3691LiveItemCount(t *testing.T, e *appsEnv) int {
	t.Helper()
	return appRows(t, e, `SELECT COUNT(*) FROM items WHERE workspace_id = ? AND deleted_at IS NULL`, e.wsID)
}

func bug3691AssertPlanLimit403(t *testing.T, status int, body string, limit int, wantNote string) {
	t.Helper()
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", status, body)
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details struct {
				Feature string `json:"feature"`
				Limit   int    `json:"limit"`
				Current int    `json:"current"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode error envelope: %v (body=%s)", err, body)
	}
	if env.Error.Code != "plan_limit_exceeded" {
		t.Errorf("error.code = %q, want plan_limit_exceeded (body=%s)", env.Error.Code, body)
	}
	if !strings.Contains(env.Error.Message, wantNote) {
		t.Errorf("error.message = %q, want substring %q", env.Error.Message, wantNote)
	}
	if d := env.Error.Details; d.Feature != "items_per_workspace" || d.Limit != limit || d.Current != limit {
		t.Errorf("error.details = %+v, want items_per_workspace %d/%d (body=%s)", d, limit, limit, body)
	}
}

func TestBUG3691_Server_InstallConfirm_AtCap_RefusedThenRetryAfterRaiseSucceeds(t *testing.T) {
	t.Parallel()
	e := newProvisionEnv(t)
	// Stage preview before enabling cloud mode (cloud mode disallows loopback private origins).
	p := e.stagePreview(t)

	itemsBefore := bug3691LiveItemCount(t, e)
	bug3691SetPlanCap(t, e.srv, true, itemsBefore)
	censusBefore := provisionCensus(t, e)

	rr := e.confirm(t, p, p.ManifestSHA256)
	bug3691AssertPlanLimit403(t, rr.Code, rr.Body.String(), itemsBefore, "The install was not applied, and nothing was written.")
	if got := bug3691LiveItemCount(t, e); got != itemsBefore {
		t.Errorf("live item count = %d, want unchanged %d", got, itemsBefore)
	}
	assertCensusUnchanged(t, censusBefore, provisionCensus(t, e))

	// Raising the cap lets a second confirm of the SAME pending install succeed
	// (the pending record was not consumed on the 403 refusal).
	bug3691SetPlanCap(t, e.srv, true, itemsBefore+1)
	rr2 := e.confirm(t, p, p.ManifestSHA256)
	if rr2.Code != http.StatusCreated {
		t.Fatalf("retry confirm after raising cap: status = %d, want 201 (body=%s)", rr2.Code, rr2.Body.String())
	}
	if got := bug3691LiveItemCount(t, e); got != itemsBefore+1 {
		t.Errorf("live item count after retry = %d, want %d", got, itemsBefore+1)
	}
	if got := e.pendingRows(t); got != 0 {
		t.Errorf("pending rows after successful retry = %d, want 0", got)
	}
}

func TestBUG3691_Server_InstallConfirm_UnderCap_ControlSucceeds(t *testing.T) {
	t.Parallel()
	e := newProvisionEnv(t)
	p := e.stagePreview(t)

	itemsBefore := bug3691LiveItemCount(t, e)
	bug3691SetPlanCap(t, e.srv, true, itemsBefore+1)

	rr := e.confirm(t, p, p.ManifestSHA256)
	if rr.Code != http.StatusCreated {
		t.Fatalf("control confirm one under cap: status = %d, want 201 (body=%s)", rr.Code, rr.Body.String())
	}
	if got := bug3691LiveItemCount(t, e); got != itemsBefore+1 {
		t.Errorf("live item count = %d, want %d", got, itemsBefore+1)
	}
}

func TestBUG3691_Server_InstallConfirm_SelfHosted_AtCap_Succeeds(t *testing.T) {
	t.Parallel()
	e := newProvisionEnv(t)
	p := e.stagePreview(t)

	itemsBefore := bug3691LiveItemCount(t, e)
	bug3691SetPlanCap(t, e.srv, false, itemsBefore)

	rr := e.confirm(t, p, p.ManifestSHA256)
	if rr.Code != http.StatusCreated {
		t.Fatalf("self-hosted confirm at cap: status = %d, want 201 (body=%s)", rr.Code, rr.Body.String())
	}
	if got := bug3691LiveItemCount(t, e); got != itemsBefore+1 {
		t.Errorf("live item count = %d, want %d", got, itemsBefore+1)
	}
}

func bug3691StageArtifactUpgrade(t *testing.T, u *upgradeEnv) appPreview {
	t.Helper()
	body2 := []byte(strings.Replace(string(u.files["/pack/ship.md"]), "title: Ship a change", "title: Ship a change v2", 1))
	u.files["/pack/ship.md"] = body2
	u.m["version"] = "1.1.0"
	u.m["companion_pack"].(map[string]any)["artifacts"] = []any{
		map[string]any{"key": "ship", "url": u.origin() + "/pack/ship.md", "sha256": appSHA(body2)},
	}
	u.publish(t, u.m)
	code, p, body := u.previewUpgrade(t)
	if code != http.StatusOK {
		t.Fatalf("preview upgrade: %d %s", code, body)
	}
	return p
}

func TestBUG3691_Server_UpgradeConfirm_AtCapWithArtifact_RefusedAndNothingWritten(t *testing.T) {
	t.Parallel()
	u := newUpgradeEnv(t)
	p := bug3691StageArtifactUpgrade(t, u)

	itemsBefore := bug3691LiveItemCount(t, u.appsEnv)
	bug3691SetPlanCap(t, u.srv, true, itemsBefore)
	censusBefore := provisionCensus(t, u.appsEnv)

	code, body := u.confirmUpgrade(t, p)
	bug3691AssertPlanLimit403(t, code, body, itemsBefore, "The upgrade was not applied, and nothing was written.")
	if got := bug3691LiveItemCount(t, u.appsEnv); got != itemsBefore {
		t.Errorf("live item count = %d, want unchanged %d", got, itemsBefore)
	}
	assertCensusUnchanged(t, censusBefore, provisionCensus(t, u.appsEnv))
	var version string
	if err := u.srv.store.DB().QueryRow(`SELECT manifest_version FROM app_installs WHERE id = ?`, u.installID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != "1.0.0" {
		t.Errorf("manifest_version = %q, want unchanged 1.0.0", version)
	}

	// Raising the cap allows the same pending upgrade to confirm cleanly.
	bug3691SetPlanCap(t, u.srv, true, itemsBefore+1)
	code2, body2 := u.confirmUpgrade(t, p)
	if code2 != http.StatusOK {
		t.Fatalf("retry upgrade confirm after raising cap: status = %d, want 200 (body=%s)", code2, body2)
	}
	if got := bug3691LiveItemCount(t, u.appsEnv); got != itemsBefore+1 {
		t.Errorf("live item count after retry = %d, want %d", got, itemsBefore+1)
	}
}

func TestBUG3691_Server_UpgradeConfirm_UnderCapWithArtifact_ControlSucceeds(t *testing.T) {
	t.Parallel()
	u := newUpgradeEnv(t)
	p := bug3691StageArtifactUpgrade(t, u)

	itemsBefore := bug3691LiveItemCount(t, u.appsEnv)
	bug3691SetPlanCap(t, u.srv, true, itemsBefore+1)

	code, body := u.confirmUpgrade(t, p)
	if code != http.StatusOK {
		t.Fatalf("control upgrade confirm one under cap: status = %d, want 200 (body=%s)", code, body)
	}
	if got := bug3691LiveItemCount(t, u.appsEnv); got != itemsBefore+1 {
		t.Errorf("live item count = %d, want %d", got, itemsBefore+1)
	}
}

func TestBUG3691_Server_UpgradeConfirm_AtCapNoArtifact_Succeeds(t *testing.T) {
	t.Parallel()
	u := newUpgradeEnv(t)
	u.m["version"] = "1.0.1"
	u.publish(t, u.m)
	code, p, body := u.previewUpgrade(t)
	if code != http.StatusOK {
		t.Fatalf("preview upgrade: %d %s", code, body)
	}

	itemsBefore := bug3691LiveItemCount(t, u.appsEnv)
	bug3691SetPlanCap(t, u.srv, true, itemsBefore)

	code, body = u.confirmUpgrade(t, p)
	if code != http.StatusOK {
		t.Fatalf("no-artifact upgrade confirm at cap: status = %d, want 200 (body=%s)", code, body)
	}
	if got := bug3691LiveItemCount(t, u.appsEnv); got != itemsBefore {
		t.Errorf("live item count = %d, want unchanged %d", got, itemsBefore)
	}
}
