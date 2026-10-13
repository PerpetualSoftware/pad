package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3768: installing an app adds its bot as a member with collection_access
// 'all', capped only by the install's read ceiling (the workspace's system
// collections plus the app's companions). So an install, and anything that
// hands out or restores the bot's credentials, gives the app system
// collections a restricted owner cannot see. The same whole-workspace
// capability class as BUG-1922 (export) and BUG-3340 (webhooks): a restricted
// owner is refused every door that installs, re-enables, upgrades or issues
// credentials. Reading an install, disabling it and uninstalling it stay open,
// as webhook delete did.

// bug3768CoOwner adds a second owner and returns their session cookie and id.
func bug3768CoOwner(t *testing.T, e *appsEnv) (string, string) {
	t.Helper()
	co, err := e.srv.store.CreateUser(models.UserCreate{Email: "coowner@test.com", Name: "CoOwner", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.srv.store.AddWorkspaceMember(e.wsID, co.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	cookie, err := e.srv.store.CreateSession(co.ID, "web-test", "192.0.2.1", "", webSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	return cookie, co.ID
}

// bug3768Restrict scopes the user to the tasks collection.
func bug3768Restrict(t *testing.T, e *appsEnv, userID string) {
	t.Helper()
	tasks, err := e.srv.store.GetCollectionBySlug(e.wsID, "tasks")
	if err != nil || tasks == nil {
		t.Fatalf("tasks collection: %v", err)
	}
	if err := e.srv.store.SetMemberCollectionAccess(e.wsID, userID, "specific", []string{tasks.ID}); err != nil {
		t.Fatal(err)
	}
}

func TestBUG3768_RestrictedOwnerCannotGrantAnApp(t *testing.T) {
	u := newUpgradeEnv(t)
	co, coID := bug3768CoOwner(t, u.appsEnv)
	bug3768Restrict(t, u.appsEnv, coID)
	ws := "/api/v1/workspaces/" + u.ws
	app := ws + "/apps/" + u.installID

	do := func(method, path string, body any, cookie string) (int, string) {
		t.Helper()
		rr := doRequestWithCookie(u.srv, method, path, body, cookie)
		return rr.Code, rr.Body.String()
	}
	want := func(label string, got int, body string, codes ...int) {
		t.Helper()
		for _, c := range codes {
			if got == c {
				return
			}
		}
		t.Errorf("%s: %d %s, want one of %v", label, got, body, codes)
	}

	// Install preview is pinned in TestBUG3768_RestrictedAfterStagingCannotConfirm,
	// on a workspace where the app is not installed yet: here it would answer
	// 409 app_already_installed whatever the gate did.
	code, body := do("POST", app+"/install-code", nil, co)
	want("restricted owner install code", code, body, http.StatusForbidden)
	code, body = do("POST", app+"/rotate", nil, co)
	want("restricted owner rotate", code, body, http.StatusForbidden)
	pendingBefore := u.pendingRows(t)
	code, body = do("POST", app+"/upgrade/preview", nil, co)
	want("restricted owner upgrade preview", code, body, http.StatusForbidden)
	if n := u.pendingRows(t); n != pendingBefore {
		t.Errorf("a refused upgrade preview staged a pending upgrade: %d rows, was %d", n, pendingBefore)
	}

	// Reads stay open.
	code, body = do("GET", ws+"/apps", nil, co)
	want("restricted owner list installs", code, body, http.StatusOK)
	code, body = do("GET", app, nil, co)
	want("restricted owner read install", code, body, http.StatusOK)

	// The refused rotate must not have torn the install down part way.
	if st, err := u.srv.store.InstallState(u.wsID, u.installID); err != nil || st != "active" {
		t.Fatalf("after the refused doors the install is %q (%v), want active", st, err)
	}

	// Disable is revocation and stays open; re-enabling is a grant.
	code, body = do("POST", app+"/disable", nil, co)
	want("restricted owner disable", code, body, http.StatusOK)
	code, body = do("POST", app+"/enable", nil, co)
	want("restricted owner enable", code, body, http.StatusForbidden)
	if st, _ := u.srv.store.InstallState(u.wsID, u.installID); st == "active" {
		t.Error("a refused enable re-activated the install")
	}
	// Control: the same enable from the unrestricted owner succeeds, so the
	// 403 above is the restriction and not the install's state.
	code, body = do("POST", app+"/enable", nil, u.token)
	want("control: unrestricted owner enable", code, body, http.StatusOK)
	code, body = do("POST", app+"/rotate", nil, u.token)
	want("control: unrestricted owner rotate", code, body, http.StatusOK)

	code, body = do("POST", app+"/uninstall", nil, co)
	want("restricted owner uninstall", code, body, http.StatusOK, http.StatusNoContent)
}

// The confirm doors check the restriction at confirm time, not only at
// preview: an owner restricted after staging cannot finish.
func TestBUG3768_RestrictedAfterStagingCannotConfirm(t *testing.T) {
	u := newUpgradeEnv(t)
	co, coID := bug3768CoOwner(t, u.appsEnv)

	// Install: the co-owner stages a second install of the same app while
	// unrestricted. A staged install of an already-installed origin may be
	// refused for that reason, so stage on a fresh env instead.
	e := newProvisionEnv(t)
	eco, ecoID := bug3768CoOwner(t, e)
	rr := doRequestWithCookie(e.srv, "POST", "/api/v1/workspaces/"+e.ws+"/apps/install/preview", map[string]any{"base_url": e.origin()}, eco)
	if rr.Code != http.StatusOK {
		t.Fatalf("unrestricted co-owner preview: %d %s", rr.Code, rr.Body.String())
	}
	var p appPreview
	parseJSON(t, rr, &p)
	bug3768Restrict(t, e, ecoID)
	pendingBefore := e.pendingRows(t)
	rr = doRequestWithCookie(e.srv, "POST", "/api/v1/workspaces/"+e.ws+"/apps/install/preview", map[string]any{"base_url": e.origin()}, eco)
	if rr.Code != http.StatusForbidden {
		t.Errorf("restricted owner install preview: %d %s, want 403", rr.Code, rr.Body.String())
	}
	if n := e.pendingRows(t); n != pendingBefore {
		t.Errorf("a refused preview staged a pending install: %d rows, was %d", n, pendingBefore)
	}
	before := provisionCensus(t, e)
	rr = doRequestWithCookie(e.srv, "POST", "/api/v1/workspaces/"+e.ws+"/apps/install/pending/"+p.PendingID+"/confirm",
		map[string]any{"manifest_sha256": p.ManifestSHA256}, eco)
	if rr.Code != http.StatusForbidden {
		t.Errorf("restricted owner install confirm: %d %s, want 403", rr.Code, rr.Body.String())
	}
	assertCensusUnchanged(t, before, provisionCensus(t, e))

	// Upgrade: the co-owner previews an upgrade while unrestricted.
	m := u.m
	m["version"] = "1.1.0"
	u.publish(t, m)
	rr = doRequestWithCookie(u.srv, "POST", "/api/v1/workspaces/"+u.ws+"/apps/"+u.installID+"/upgrade/preview", nil, co)
	if rr.Code != http.StatusOK {
		t.Fatalf("unrestricted co-owner upgrade preview: %d %s", rr.Code, rr.Body.String())
	}
	var up appPreview
	parseJSON(t, rr, &up)
	bug3768Restrict(t, u.appsEnv, coID)
	rr = doRequestWithCookie(u.srv, "POST", "/api/v1/workspaces/"+u.ws+"/apps/"+u.installID+"/upgrade/confirm",
		map[string]any{"pending_id": up.PendingID, "manifest_sha256": up.ManifestSHA256}, co)
	if rr.Code != http.StatusForbidden {
		t.Errorf("restricted owner upgrade confirm: %d %s, want 403", rr.Code, rr.Body.String())
	}
	if n := u.count(t, `SELECT COUNT(*) FROM app_installs WHERE id = ? AND manifest_version = ?`, u.installID, "1.0.0"); n != 1 {
		t.Error("after the refused upgrade confirm the install is no longer at version 1.0.0")
	}
}
