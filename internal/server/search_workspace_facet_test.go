package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestSearch_WorkspaceLabelHasName(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	ownerCookie := bootstrapFirstUser(t, srv, "owner@test.com", "Owner")
	owner, err := srv.store.GetUserByEmail("owner@test.com")
	if err != nil || owner == nil {
		t.Fatalf("owner lookup: %v", err)
	}

	mkWS := func(name string) *models.Workspace {
		rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{"name": name}, ownerCookie)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create workspace %s: %d %s", name, rr.Code, rr.Body.String())
		}
		var ws models.Workspace
		parseJSON(t, rr, &ws)
		return &ws
	}
	wsB := mkWS("Bravo Workspace")
	wsC := mkWS("Charlie Secret")

	mkItem := func(wsSlug, coll, title string) *models.Item {
		rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/collections/"+coll+"/items",
			map[string]any{"title": title, "content": "labelnameprobe body"}, ownerCookie)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", title, rr.Code, rr.Body.String())
		}
		var it models.Item
		parseJSON(t, rr, &it)
		return &it
	}
	mkItem(wsB.Slug, "tasks", "Bravo visible hit")
	mkItem(wsC.Slug, "tasks", "Charlie hidden hit")

	guest, err := srv.store.CreateUser(models.UserCreate{
		Email:    "guestb@test.com",
		Name:     "Guest B",
		Password: "correct-horse-battery-staple",
	})
	if err != nil {
		t.Fatal(err)
	}
	tasksB, err := srv.store.GetCollectionBySlug(wsB.ID, "tasks")
	if err != nil || tasksB == nil {
		t.Fatalf("tasks in B: %v", err)
	}
	if _, err := srv.store.CreateCollectionGrant(wsB.ID, tasksB.ID, guest.ID, "view", owner.ID); err != nil {
		t.Fatal(err)
	}
	guestSess, err := srv.store.CreateSession(guest.ID, "web-test", "192.0.2.1", "", webSessionTTL)
	if err != nil {
		t.Fatal(err)
	}

	rr := doRequestWithCookie(srv, "GET", "/api/v1/search?"+url.Values{"q": {"labelnameprobe"}}.Encode(), nil, guestSess)
	if rr.Code != http.StatusOK {
		t.Fatalf("search: %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	var resp struct {
		Results []struct {
			Item struct {
				Title string `json:"title"`
			} `json:"item"`
			Workspace *struct {
				Slug          string `json:"slug"`
				Name          string `json:"name"`
				OwnerUsername string `json:"owner_username"`
			} `json:"workspace"`
		} `json:"results"`
	}
	if err := jsonUnmarshalString(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("got %d results, want 1 (%s)", len(resp.Results), body)
	}
	ws := resp.Results[0].Workspace
	if ws == nil || ws.Slug != wsB.Slug || ws.Name != "Bravo Workspace" {
		t.Errorf("workspace label = %+v, want slug=%q name=%q", ws, wsB.Slug, "Bravo Workspace")
	}
	if strings.Contains(body, wsC.Slug) || strings.Contains(body, "Charlie Secret") {
		t.Errorf("response leaked inaccessible workspace C: %s", body)
	}
}

func TestSearch_WorkspaceFacetAdmittedOnly(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	ownerCookie := bootstrapFirstUser(t, srv, "owner@test.com", "Owner")
	owner, err := srv.store.GetUserByEmail("owner@test.com")
	if err != nil || owner == nil {
		t.Fatalf("owner lookup: %v", err)
	}

	mkWS := func(name string) *models.Workspace {
		rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{"name": name}, ownerCookie)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create workspace %s: %d %s", name, rr.Code, rr.Body.String())
		}
		var ws models.Workspace
		parseJSON(t, rr, &ws)
		return &ws
	}
	wsA := mkWS("Facet Alpha")
	wsB := mkWS("Facet Bravo")
	wsC := mkWS("Facet Charlie")
	wsD := mkWS("Facet Delta Empty")

	mkItem := func(wsSlug, coll, title, content string) *models.Item {
		rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/collections/"+coll+"/items",
			map[string]any{"title": title, "content": content}, ownerCookie)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", title, rr.Code, rr.Body.String())
		}
		var it models.Item
		parseJSON(t, rr, &it)
		return &it
	}
	mkItem(wsA.Slug, "tasks", "A hit 1", "facetneedle body")
	mkItem(wsA.Slug, "docs", "A hit 2", "facetneedle body")
	mkItem(wsB.Slug, "tasks", "B granted hit", "facetneedle body")
	mkItem(wsB.Slug, "docs", "B ungranted hit", "facetneedle body")
	mkItem(wsC.Slug, "tasks", "C hidden hit", "facetneedle body")
	mkItem(wsD.Slug, "tasks", "D unrelated", "nothing matching here")

	caller, err := srv.store.CreateUser(models.UserCreate{
		Email:    "caller@test.com",
		Name:     "Caller",
		Password: "correct-horse-battery-staple",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AddWorkspaceMember(wsA.ID, caller.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AddWorkspaceMember(wsD.ID, caller.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	tasksB, err := srv.store.GetCollectionBySlug(wsB.ID, "tasks")
	if err != nil || tasksB == nil {
		t.Fatalf("tasks in B: %v", err)
	}
	if _, err := srv.store.CreateCollectionGrant(wsB.ID, tasksB.ID, caller.ID, "view", owner.ID); err != nil {
		t.Fatal(err)
	}
	sess, err := srv.store.CreateSession(caller.ID, "web-test", "192.0.2.1", "", webSessionTTL)
	if err != nil {
		t.Fatal(err)
	}

	rr := doRequestWithCookie(srv, "GET", "/api/v1/search?"+url.Values{"q": {"facetneedle"}, "limit": {"200"}}.Encode(), nil, sess)
	if rr.Code != http.StatusOK {
		t.Fatalf("search: %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	var resp struct {
		Results []struct {
			Item struct {
				Title string `json:"title"`
			} `json:"item"`
			Workspace *struct {
				Slug string `json:"slug"`
			} `json:"workspace"`
		} `json:"results"`
		Facets *struct {
			Collections map[string]int `json:"collections"`
			Statuses    map[string]int `json:"statuses"`
			Workspaces  map[string]int `json:"workspaces"`
		} `json:"facets"`
	}
	if err := jsonUnmarshalString(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Facets == nil {
		t.Fatalf("missing facets: %s", body)
	}
	bVisible := 0
	for _, r := range resp.Results {
		if r.Item.Title == "B ungranted hit" || r.Item.Title == "C hidden hit" {
			t.Errorf("inaccessible item returned: %q", r.Item.Title)
		}
		if r.Workspace != nil && r.Workspace.Slug == wsB.Slug {
			bVisible++
		}
	}
	if bVisible != 1 {
		t.Errorf("visible B results = %d, want 1", bVisible)
	}
	got := resp.Facets.Workspaces
	if len(got) != 2 {
		t.Errorf("facets.workspaces = %v, want exactly 2 keys (%s and %s)", got, wsA.Slug, wsB.Slug)
	}
	if got[wsA.Slug] != 2 {
		t.Errorf("facets.workspaces[%s] = %d, want 2", wsA.Slug, got[wsA.Slug])
	}
	if got[wsB.Slug] != bVisible {
		t.Errorf("facets.workspaces[%s] = %d, want %d (matching visible B results)", wsB.Slug, got[wsB.Slug], bVisible)
	}
	if _, hasC := got[wsC.Slug]; hasC {
		t.Errorf("facets.workspaces leaked inaccessible workspace C: %v", got)
	}
	if _, hasD := got[wsD.Slug]; hasD {
		t.Errorf("facets.workspaces included zero-hit workspace D: %v", got)
	}
	if strings.Contains(body, `"`+wsC.Slug+`"`) {
		t.Errorf("response body mentions workspace C slug %q: %s", wsC.Slug, body)
	}

	// Named-workspace search must omit facets.workspaces.
	scoped := doRequestWithCookie(srv, "GET", "/api/v1/search?"+url.Values{"q": {"facetneedle"}, "workspace": {wsA.Slug}}.Encode(), nil, sess)
	if scoped.Code != http.StatusOK {
		t.Fatalf("scoped search: %d %s", scoped.Code, scoped.Body.String())
	}
	if strings.Contains(scoped.Body.String(), `"workspaces"`) {
		t.Errorf("scoped search should omit workspaces facet: %s", scoped.Body.String())
	}
}
