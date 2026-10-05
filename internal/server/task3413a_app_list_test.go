package server

import (
	"net/http"
	"testing"
)

// TASK-3413 (SPEC-6 U9a): GET /workspaces/{ws}/apps, the owner's Apps list.

func (e *appsEnv) listInstalls(t *testing.T, token string) (int, appInstallListResponse) {
	t.Helper()
	rr := doRequestWithCookie(e.srv, "GET", "/api/v1/workspaces/"+e.ws+"/apps", nil, token)
	var out appInstallListResponse
	if rr.Code == http.StatusOK {
		parseJSON(t, rr, &out)
	}
	return rr.Code, out
}

func TestTask3413a_TheListNamesEveryInstallWithItsStateAndHook(t *testing.T) {
	e := newProvisionEnv(t)
	code, out := e.listInstalls(t, e.token)
	if code != http.StatusOK || !out.Available || len(out.Installs) != 0 {
		t.Fatalf("empty workspace: %d %+v", code, out)
	}
	p := e.stagePreview(t)
	rr := e.confirm(t, p, p.ManifestSHA256)
	if rr.Code != http.StatusCreated {
		t.Fatalf("confirm: %d %s", rr.Code, rr.Body.String())
	}
	var conf appInstallConfirmResponse
	parseJSON(t, rr, &conf)

	_, out = e.listInstalls(t, e.token)
	if len(out.Installs) != 1 {
		t.Fatalf("installs = %+v", out.Installs)
	}
	got := out.Installs[0]
	if got.InstallID != conf.InstallID || got.State != "active" || got.Origin != e.origin() || got.Version != p.Version {
		t.Errorf("entry = %+v; want install %s active at %s v%s", got, conf.InstallID, e.origin(), p.Version)
	}
	if got.AppName != p.Title {
		t.Errorf("app_name = %q, want the app's title %q", got.AppName, p.Title)
	}
	if p.WebhookURL != "" && (got.Webhook == nil || got.Webhook.URL != p.WebhookURL) {
		t.Errorf("webhook = %+v, want the manifest's %s", got.Webhook, p.WebhookURL)
	}

	// An uninstall leaves the tombstone listed, as uninstalled.
	if rr := doRequestWithCookie(e.srv, "POST", "/api/v1/workspaces/"+e.ws+"/apps/"+conf.InstallID+"/uninstall", nil, e.token); rr.Code != http.StatusOK {
		t.Fatalf("uninstall: %d %s", rr.Code, rr.Body.String())
	}
	_, out = e.listInstalls(t, e.token)
	if len(out.Installs) != 1 || out.Installs[0].State != "uninstalled" || out.Installs[0].AppName != p.Title {
		t.Errorf("after uninstall: %+v", out.Installs)
	}
}

func TestTask3413a_OnlyAnOwnerListsInstalls(t *testing.T) {
	e := newProvisionEnv(t)
	for _, role := range []string{"editor", "viewer"} {
		user, token := loginTestUserAs(t, e.srv, role+"@test.com", role, "password123")
		if err := e.srv.store.AddWorkspaceMember(e.wsID, user.ID, role); err != nil {
			t.Fatal(err)
		}
		if code, _ := e.listInstalls(t, token); code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", role, code)
		}
	}
}

func TestTask3413a_AppsOffSaysSoInsteadOfAnEmptyList(t *testing.T) {
	e := newProvisionEnv(t)
	p := e.stagePreview(t)
	if rr := e.confirm(t, p, p.ManifestSHA256); rr.Code != http.StatusCreated {
		t.Fatalf("confirm: %d", rr.Code)
	}
	if rr := doRequestWithCookie(e.srv, "PUT", "/api/v1/admin/apps", map[string]any{"enabled": false}, e.token); rr.Code != http.StatusOK {
		t.Fatalf("disable apps: %d", rr.Code)
	}
	code, out := e.listInstalls(t, e.token)
	if code != http.StatusOK || out.Available || len(out.Installs) != 0 || out.Installs == nil {
		t.Errorf("apps off: %d %+v; want 200, available false, installs []", code, out)
	}
}
