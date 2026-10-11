package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3595: a first Cloud signup lands inside the workspace auto-created for
// it, not on the console's list. The signup doors return where that is as an
// additive `workspace_path`; the client (register page) and the sidecar
// (OAuth) decide whether an explicit destination wins.

func task3595OnlyWorkspacePath(t *testing.T, srv *Server, email string) (string, *models.User) {
	t.Helper()
	u, err := srv.store.GetUserByEmail(email)
	if err != nil || u == nil {
		t.Fatalf("lookup %s: %v", email, err)
	}
	wss, err := srv.store.GetUserWorkspaces(u.ID)
	if err != nil {
		t.Fatalf("GetUserWorkspaces: %v", err)
	}
	if len(wss) != 1 {
		t.Fatalf("PREMISE: want exactly the auto-created workspace, got %d", len(wss))
	}
	return "/" + u.Username + "/" + wss[0].Slug, u
}

func TestTask3595_CloudRegisterReturnsTheNewWorkspacePath(t *testing.T) {
	srv, _ := newCloudEmailServer(t)
	rr := doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email":    "first3595@example.com",
		"name":     "First Signup",
		"password": "correct-horse-battery-staple",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rr.Code, rr.Body.String())
	}
	var body struct {
		WorkspacePath string `json:"workspace_path"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want, _ := task3595OnlyWorkspacePath(t, srv, "first3595@example.com")
	if body.WorkspacePath != want {
		t.Errorf("workspace_path = %q, want %q", body.WorkspacePath, want)
	}
}

func TestTask3595_OAuthSignupReturnsTheWorkspacePathOnlyForANewUser(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode(oauthProviderTestSecret)
	login := func() map[string]any {
		t.Helper()
		body := oauthLoginBody("oauth3595@example.com")
		body["cloud_secret"] = oauthProviderTestSecret
		body["subject"] = "gh-3595"
		rr := doRequestWithHeadersFromAddr(srv, "POST", "/api/v1/auth/oauth-login", body, map[string]string{
			"X-Cloud-Secret": oauthProviderTestSecret,
		}, "127.0.0.1:1")
		if rr.Code != http.StatusOK {
			t.Fatalf("oauth-login: %d %s", rr.Code, rr.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	first := login()
	if first["new_user"] != true {
		t.Fatalf("PREMISE: the first sign-in did not create the user: %v", first)
	}
	want, _ := task3595OnlyWorkspacePath(t, srv, "oauth3595@example.com")
	if first["workspace_path"] != want {
		t.Errorf("new user: workspace_path = %v, want %q", first["workspace_path"], want)
	}

	again := login()
	if _, ok := again["workspace_path"]; ok {
		t.Errorf("a returning user was given a workspace_path: %v", again["workspace_path"])
	}
}

// A workspace that could not be made usable is no destination: every failure
// arm returns nil, so the doors omit workspace_path and the user lands where
// they always did.
func TestTask3595_AutoCreateReturnsNilWhenItFailsAndOffCloud(t *testing.T) {
	srv := testServer(t)
	ghost := &models.User{ID: "ghost-3595", Name: "Ghost", Username: "ghost3595", Email: "ghost3595@example.com"}
	if ws := srv.autoCreateWorkspace(ghost); ws != nil {
		t.Errorf("self-host auto-create returned %+v, want nil", ws)
	}
	srv.cloudMode = true
	// Not in the users table: the owner-member insert fails and the workspace
	// is removed (TASK-1932's arm).
	if ws := srv.autoCreateWorkspace(ghost); ws != nil {
		t.Errorf("a failed auto-create returned %+v, want nil", ws)
	}
	if got := createdWorkspacePath(ghost, nil); got != "" {
		t.Errorf("createdWorkspacePath(nil) = %q, want empty", got)
	}
}
