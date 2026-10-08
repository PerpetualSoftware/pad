package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-1926: the /me workspace summary answered "all" for any platform admin,
// while the workspace middleware gives an admin no bypass over a bearer (an
// API token, a CLI session: BUG-1616's suppression). Over a bearer the summary
// advertised access the gates then denied. It now mirrors the middleware: a
// browser session gets the admin's "all", a bearer gets the admin's actual
// membership.
func TestBUG1926_MeSummaryMirrorsTheAdminBypass(t *testing.T) {
	t.Setenv("PAD_DISABLE_RATE_LIMITS", "1")
	srv := testServer(t)
	owner, err := srv.store.CreateUser(models.UserCreate{Email: "owner-1926@example.com", Name: "Owner", Password: "pw-test-12345"})
	if err != nil {
		t.Fatal(err)
	}
	admin, err := srv.store.CreateUser(models.UserCreate{Email: "admin-1926@example.com", Name: "Admin", Password: "correct-horse-battery-staple", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "WS 1926", OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	// The admin is ALSO a restricted member here: tasks only.
	if err := srv.store.AddWorkspaceMember(ws.ID, admin.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.SetMemberCollectionAccess(ws.ID, admin.ID, "specific", []string{mustCollectionID(t, srv, ws.ID, "tasks")}); err != nil {
		t.Fatal(err)
	}
	pat, err := srv.store.CreateAPIToken(admin.ID, models.APITokenCreate{Name: "admin-pat-1926", WorkspaceID: ws.ID}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	session := loginUser(t, srv, "admin-1926@example.com", "correct-horse-battery-staple")

	type me struct {
		Role             string `json:"role"`
		CollectionAccess string `json:"collection_access"`
	}
	read := func(name string, code int, body []byte) me {
		t.Helper()
		if code != http.StatusOK {
			t.Fatalf("%s: /me answered %d %s", name, code, body)
		}
		var m me
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return m
	}
	path := "/api/v1/workspaces/" + ws.Slug + "/me"

	// A browser session: the middleware grants the bypass, and /me says so.
	rr := doRequestWithCookie(srv, "GET", path, nil, session)
	if m := read("cookie", rr.Code, rr.Body.Bytes()); m.CollectionAccess != "all" || m.Role != "owner" {
		t.Errorf("admin browser session: %+v, want owner / all", m)
	}

	// A PAT and a CLI session (a bearer): no bypass, so the actual membership.
	for name, bearer := range map[string]string{"PAT": pat.Token, "CLI session": session} {
		rr := doRequestWithBearer(srv, "GET", path, bearer, nil)
		m := read(name, rr.Code, rr.Body.Bytes())
		if m.CollectionAccess == "all" {
			t.Errorf("admin %s: /me reports all access, which the workspace gates deny over a bearer: %+v", name, m)
		}
		if m.Role != "editor" {
			t.Errorf("admin %s: role %q, want the member role editor", name, m.Role)
		}
	}
}
