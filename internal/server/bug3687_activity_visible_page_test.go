package server

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3687: the activity feed applied collection and item-grant visibility in
// Go, after the SQL LIMIT, so a restricted member whose newest events were all
// hidden got a short or empty page, and the web feed reads a page shorter
// than its size as the end. The dashboard's recent activity read 30 rows and
// filtered them the same way. Visibility now rides into the query.
//
// Each fixture puts a run of 40 hidden rows NEWER than the visible ones, so a
// filter applied after any limit of 30 returns nothing.

const bug3687Action = "bug3687_seed"

type bug3687Fixture struct {
	srv      *Server
	ws       *models.Workspace
	task     models.Item // in "tasks"
	idea1    models.Item // in "ideas"
	idea2    models.Item // in "ideas"
	ownerTok string
}

func newBUG3687Fixture(t *testing.T, srv *Server) bug3687Fixture {
	t.Helper()
	slug := createWSWithCollections(t, srv)
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspaceBySlug: %v", err)
	}
	f := bug3687Fixture{srv: srv, ws: ws}
	f.task = createItem(t, srv, slug, "tasks", map[string]any{"title": "Visible task"})
	f.idea1 = createItem(t, srv, slug, "ideas", map[string]any{"title": "Granted idea"})
	f.idea2 = createItem(t, srv, slug, "ideas", map[string]any{"title": "Hidden idea"})
	owner := f.user(t, "owner")
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember owner: %v", err)
	}
	f.ownerTok = f.session(t, owner)
	return f
}

func (f bug3687Fixture) user(t *testing.T, name string) *models.User {
	t.Helper()
	u, err := f.srv.store.CreateUser(models.UserCreate{Email: name + "-3687@example.com", Name: name, Username: name + "-3687", Password: "pw-test-12345"})
	if err != nil {
		t.Fatalf("CreateUser %s: %v", name, err)
	}
	return u
}

func (f bug3687Fixture) session(t *testing.T, u *models.User) string {
	t.Helper()
	tok, err := f.srv.store.CreateSession(u.ID, "go-test", "192.0.2.1", "go-test", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return tok
}

// restricted is an editor who sees only the tasks collection.
func (f bug3687Fixture) restricted(t *testing.T) *models.User {
	t.Helper()
	u := f.user(t, "restricted")
	if err := f.srv.store.AddWorkspaceMember(f.ws.ID, u.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	tasks, err := f.srv.store.GetCollectionBySlug(f.ws.ID, "tasks")
	if err != nil || tasks == nil {
		t.Fatalf("GetCollectionBySlug tasks: %v", err)
	}
	if err := f.srv.store.SetMemberCollectionAccess(f.ws.ID, u.ID, "specific", []string{tasks.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}
	return u
}

// seed writes n activity rows on docID ("" = workspace-level) with
// created_at = base + i seconds. The bases are far in the future so these
// rows are newer than the fixture's own item-creation activity.
func (f bug3687Fixture) seed(t *testing.T, docID string, n int, base time.Time) {
	t.Helper()
	for i := range n {
		id, err := f.srv.store.CreateActivity(models.Activity{WorkspaceID: f.ws.ID, DocumentID: docID, Action: bug3687Action, Actor: "user", Source: "web"})
		if err != nil {
			t.Fatalf("CreateActivity: %v", err)
		}
		ts := base.Add(time.Duration(i) * time.Second).UTC().Format(time.RFC3339)
		if _, err := f.srv.store.DB().Exec(fmt.Sprintf("UPDATE activities SET created_at = '%s' WHERE id = '%s'", ts, id)); err != nil {
			t.Fatalf("set created_at: %v", err)
		}
	}
}

var (
	bug3687Older = time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC)
	bug3687Newer = time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
)

// feed walks the workspace feed for the seeded action by keyset pages of
// pageSize, as the web client does, until a page comes back short.
func (f bug3687Fixture) feed(t *testing.T, tok string, pageSize int) []models.Activity {
	t.Helper()
	base := fmt.Sprintf("/api/v1/workspaces/%s/activity?action=%s&limit=%d", f.ws.Slug, bug3687Action, pageSize)
	path := base
	var all []models.Activity
	for range 100 {
		rr := doRequestWithCookie(f.srv, "GET", path, nil, tok)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET activity: %d %s", rr.Code, rr.Body.String())
		}
		var page []models.Activity
		parseJSON(t, rr, &page)
		all = append(all, page...)
		if len(page) < pageSize {
			return all
		}
		last := page[len(page)-1]
		path = base + "&before=" + url.QueryEscape(last.CreatedAt.UTC().Format(time.RFC3339)) + "&before_id=" + url.QueryEscape(last.ID)
	}
	t.Fatalf("feed did not end in 100 pages")
	return nil
}

func countByDoc(rows []models.Activity) map[string]int {
	m := map[string]int{}
	for _, a := range rows {
		m[a.DocumentID]++
	}
	return m
}

func TestBUG3687_FeedPagesCountOnlyVisibleRows(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		f := newBUG3687Fixture(t, srv)
		f.seed(t, f.task.ID, 3, bug3687Older)
		f.seed(t, f.idea2.ID, 40, bug3687Newer)
		tok := f.session(t, f.restricted(t))

		// Control: the owner sees all 43, so the seed is what the test says.
		if got := countByDoc(f.feed(t, f.ownerTok, 30)); got[f.task.ID] != 3 || got[f.idea2.ID] != 40 {
			t.Fatalf("owner control: %v, want 3 task rows and 40 idea rows", got)
		}
		// The first page of 30 used to be the 30 newest rows, all hidden,
		// filtered to an empty page that ends the feed.
		for _, size := range []int{30, 2} {
			got := countByDoc(f.feed(t, tok, size))
			if got[f.task.ID] != 3 || len(got) != 1 {
				t.Errorf("restricted feed, page size %d: %v, want exactly the 3 task rows", size, got)
			}
		}
	})
}

func TestBUG3687_FeedItemGrantNarrowsItsCollection(t *testing.T) {
	// A restricted member with tasks access AND a grant on one idea sees the
	// ideas collection only through that grant: its sibling's rows stay out,
	// and a run of them does not starve the page.
	bothBackends(t, func(t *testing.T, srv *Server) {
		f := newBUG3687Fixture(t, srv)
		f.seed(t, f.task.ID, 2, bug3687Older)
		f.seed(t, f.idea1.ID, 2, bug3687Older.Add(time.Hour))
		f.seed(t, f.idea2.ID, 40, bug3687Newer)
		u := f.restricted(t)
		if _, err := srv.store.CreateItemGrant(f.ws.ID, f.idea1.ID, u.ID, "view", u.ID); err != nil {
			t.Fatalf("CreateItemGrant: %v", err)
		}
		tok := f.session(t, u)

		got := countByDoc(f.feed(t, tok, 30))
		if got[f.task.ID] != 2 || got[f.idea1.ID] != 2 || len(got) != 2 {
			t.Errorf("restricted feed with an item grant: %v, want 2 task rows and 2 granted-idea rows only", got)
		}
	})
}

type bug3687Dashboard struct {
	RecentActivity []DashboardActivity `json:"recent_activity"`
}

func (f bug3687Fixture) dashboard(t *testing.T, tok string) []DashboardActivity {
	t.Helper()
	rr := doRequestWithCookie(f.srv, "GET", "/api/v1/workspaces/"+f.ws.Slug+"/dashboard", nil, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET dashboard: %d %s", rr.Code, rr.Body.String())
	}
	var d bug3687Dashboard
	parseJSON(t, rr, &d)
	return d.RecentActivity
}

func TestBUG3687_DashboardRecentActivityNotStarvedByHiddenRows(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		f := newBUG3687Fixture(t, srv)
		f.seed(t, f.task.ID, 3, bug3687Older)
		f.seed(t, f.idea2.ID, 40, bug3687Newer)
		tok := f.session(t, f.restricted(t))

		recent := f.dashboard(t, tok)
		seen := 0
		for _, a := range recent {
			if a.ItemTitle == f.idea2.Title {
				t.Fatalf("dashboard leaked a hidden collection's row: %+v", a)
			}
			if a.ItemTitle == f.task.Title && a.Action == bug3687Action {
				seen++
			}
		}
		if seen != 3 {
			t.Errorf("dashboard recent activity shows %d of the 3 visible seeded rows: %+v", seen, recent)
		}
	})
}

func TestBUG3687_DashboardGuestNotStarvedByWorkspaceLevelRows(t *testing.T) {
	// A guest is never shown workspace-level activity on the dashboard; a run
	// of it newer than the guest's granted item used to fill all 30 rows.
	bothBackends(t, func(t *testing.T, srv *Server) {
		f := newBUG3687Fixture(t, srv)
		f.seed(t, f.idea1.ID, 2, bug3687Older)
		f.seed(t, "", 40, bug3687Newer)
		guest := f.user(t, "guest")
		if _, err := srv.store.CreateItemGrant(f.ws.ID, f.idea1.ID, guest.ID, "view", guest.ID); err != nil {
			t.Fatalf("CreateItemGrant: %v", err)
		}
		tok := f.session(t, guest)

		recent := f.dashboard(t, tok)
		seen := 0
		for _, a := range recent {
			if a.ItemTitle == "" {
				t.Fatalf("guest dashboard shows a workspace-level row: %+v", a)
			}
			if a.ItemTitle == f.idea1.Title && a.Action == bug3687Action {
				seen++
			}
		}
		if seen != 2 {
			t.Errorf("guest dashboard recent activity shows %d of the 2 granted-item rows: %+v", seen, recent)
		}
	})
}

func TestBUG3687_DashboardNotStarvedByRowsOfMissingItems(t *testing.T) {
	// Codex round 1: rows naming an item that no longer exists pass the feed
	// as workspace-level, but the dashboard drops them, so 30 of them used to
	// fill its window as surely as hidden rows did.
	bothBackends(t, func(t *testing.T, srv *Server) {
		f := newBUG3687Fixture(t, srv)
		f.seed(t, f.task.ID, 3, bug3687Older)
		f.seed(t, "00000000-0000-0000-0000-00000bug3687", 40, bug3687Newer)
		tok := f.session(t, f.restricted(t))

		seen := 0
		for _, a := range f.dashboard(t, tok) {
			if a.ItemTitle == f.task.Title && a.Action == bug3687Action {
				seen++
			}
		}
		if seen != 3 {
			t.Errorf("dashboard recent activity shows %d of the 3 visible seeded rows under 40 newer rows of a missing item", seen)
		}
		// The feed keeps showing those rows, as workspace-level, as before.
		got := countByDoc(f.feed(t, tok, 30))
		if got[f.task.ID] != 3 || got["00000000-0000-0000-0000-00000bug3687"] != 40 {
			t.Errorf("restricted feed: %v, want 3 task rows and the 40 missing-item rows", got)
		}
	})
}

func TestBUG3687_FeedBearerAdminIsScopedAndNotStarved(t *testing.T) {
	// A platform admin who is a restricted member, over a bearer token, is
	// scoped to that membership (BUG-1617), and the scope is in the query too.
	bothBackends(t, func(t *testing.T, srv *Server) {
		f := newBUG3687Fixture(t, srv)
		f.seed(t, f.task.ID, 3, bug3687Older)
		f.seed(t, f.idea2.ID, 40, bug3687Newer)
		admin, err := srv.store.CreateUser(models.UserCreate{Email: "admin-3687@example.com", Name: "Admin", Username: "admin-3687", Password: "pw-test-12345", Role: "admin"})
		if err != nil {
			t.Fatalf("CreateUser admin: %v", err)
		}
		if err := srv.store.AddWorkspaceMember(f.ws.ID, admin.ID, "editor"); err != nil {
			t.Fatalf("AddWorkspaceMember: %v", err)
		}
		tasks, err := srv.store.GetCollectionBySlug(f.ws.ID, "tasks")
		if err != nil || tasks == nil {
			t.Fatalf("GetCollectionBySlug tasks: %v", err)
		}
		if err := srv.store.SetMemberCollectionAccess(f.ws.ID, admin.ID, "specific", []string{tasks.ID}); err != nil {
			t.Fatalf("SetMemberCollectionAccess: %v", err)
		}
		tok, err := srv.store.CreateAPIToken(admin.ID, models.APITokenCreate{Name: "admin-pat", WorkspaceID: f.ws.ID}, 0, 0)
		if err != nil {
			t.Fatalf("CreateAPIToken: %v", err)
		}
		rr := doRequestWithBearer(srv, "GET", fmt.Sprintf("/api/v1/workspaces/%s/activity?action=%s&limit=30", f.ws.Slug, bug3687Action), tok.Token, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("bearer GET activity: %d %s", rr.Code, rr.Body.String())
		}
		var page []models.Activity
		parseJSON(t, rr, &page)
		if got := countByDoc(page); got[f.task.ID] != 3 || len(got) != 1 {
			t.Errorf("bearer admin feed: %v, want exactly the 3 task rows", got)
		}
	})
}

func TestBUG3687_UnscopedDashboardNotStarvedByRowsOfMissingItems(t *testing.T) {
	// Codex round 2: a cookie-session platform admin has no visibility scope,
	// so the exclusion of missing-item rows cannot ride on that scope.
	bothBackends(t, func(t *testing.T, srv *Server) {
		f := newBUG3687Fixture(t, srv)
		f.seed(t, f.task.ID, 3, bug3687Older)
		f.seed(t, "00000000-0000-0000-0000-00000bug3687", 40, bug3687Newer)
		admin, err := srv.store.CreateUser(models.UserCreate{Email: "admin-3687@example.com", Name: "Admin", Username: "admin-3687", Password: "pw-test-12345", Role: "admin"})
		if err != nil {
			t.Fatalf("CreateUser admin: %v", err)
		}
		seen := 0
		for _, a := range f.dashboard(t, f.session(t, admin)) {
			if a.ItemTitle == f.task.Title && a.Action == bug3687Action {
				seen++
			}
		}
		if seen != 3 {
			t.Errorf("cookie admin dashboard shows %d of the 3 seeded task rows under 40 newer rows of a missing item", seen)
		}
	})
}
