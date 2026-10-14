package server

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3791: GET /changes?since= is bounded, and continues by a cursor on the
// workspace write order, so a caller can pull every change in pages and a
// change committed during the walk lands in a later page of it.

type bug3791Page struct {
	Updated    []models.Item `json:"updated"`
	Deleted    []string      `json:"deleted"`
	ServerTime int64         `json:"server_time"`
	HasMore    bool          `json:"has_more"`
	NextCursor string        `json:"next_cursor"`
}

func bug3791Get(t *testing.T, srv *Server, path, cookie string) bug3791Page {
	t.Helper()
	res := doRequest(srv, "GET", path, nil)
	if cookie != "" {
		res = doRequestWithCookie(srv, "GET", path, nil, cookie)
	}
	if res.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", path, res.Code, res.Body.String())
	}
	var p bug3791Page
	parseJSON(t, res, &p)
	return p
}

// bug3791Walk pages from since to the end, calling between after each page
// that has a next one.
func bug3791Walk(t *testing.T, srv *Server, slug, cookie string, sinceMs int64, limit int, between func(page int, p bug3791Page)) []bug3791Page {
	t.Helper()
	base := "/api/v1/workspaces/" + slug + "/changes"
	path := fmt.Sprintf("%s?since=%d&limit=%d", base, sinceMs, limit)
	var pages []bug3791Page
	for i := 0; ; i++ {
		if i > 200 {
			t.Fatal("walk did not end")
		}
		p := bug3791Get(t, srv, path, cookie)
		pages = append(pages, p)
		if len(p.Updated) > limit {
			t.Fatalf("page %d: %d rows over limit %d", i, len(p.Updated), limit)
		}
		if !p.HasMore {
			if p.NextCursor != "" {
				t.Fatalf("page %d: last page carries a cursor %q", i, p.NextCursor)
			}
			return pages
		}
		if p.NextCursor == "" {
			t.Fatalf("page %d: has_more without a cursor", i)
		}
		if between != nil {
			between(i, p)
		}
		path = base + "?cursor=" + p.NextCursor + fmt.Sprintf("&limit=%d", limit)
	}
}

func TestBUG3791_ChangesIsBounded(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug, wsID := task3462Workspace(t, srv, "Bounds 3791", "startup")
		tasks, err := srv.store.GetCollectionBySlug(wsID, "tasks")
		if err != nil || tasks == nil {
			t.Fatalf("tasks: %v", err)
		}
		for i := 0; i < store.MaxChangesLimit+1; i++ {
			if _, err := srv.store.CreateItem(wsID, tasks.ID, models.ItemCreate{Title: fmt.Sprintf("Bounded %04d", i), Fields: `{"status":"open"}`}); err != nil {
				t.Fatal(err)
			}
		}
		for _, q := range []string{"", "&limit=0", "&limit=-5", "&limit=abc", "&limit=5000"} {
			p := bug3791Get(t, srv, "/api/v1/workspaces/"+slug+"/changes?since=0"+q, "")
			if len(p.Updated) != store.MaxChangesLimit || !p.HasMore || p.NextCursor == "" {
				t.Fatalf("since=0%s: %d rows, has_more %v, cursor %q; want %d, true, a cursor", q, len(p.Updated), p.HasMore, p.NextCursor, store.MaxChangesLimit)
			}
		}
	})
}

// A walk visits every change exactly once, reports deletions, repeats the first
// page's server time, and a change committed between pages, to a row an earlier
// page already returned, to a row no page has reached yet, or a new row, is
// returned by a later page of the same walk.
func TestBUG3791_WalkSeesEveryChangeIncludingMidWalkWrites(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug, wsID := task3462Workspace(t, srv, "Walk 3791", "startup")
		tasks, err := srv.store.GetCollectionBySlug(wsID, "tasks")
		if err != nil || tasks == nil {
			t.Fatalf("tasks: %v", err)
		}
		var made []*models.Item
		for i := 0; i < 9; i++ {
			it, err := srv.store.CreateItem(wsID, tasks.ID, models.ItemCreate{Title: fmt.Sprintf("Walk %d", i), Fields: `{"status":"open"}`})
			if err != nil {
				t.Fatal(err)
			}
			made = append(made, it)
		}
		if err := srv.store.DeleteItem(made[4].ID); err != nil {
			t.Fatal(err)
		}
		// The template's seeded rows predate the walk's since. The live rows made
		// here all tie on updated_at, so an updated_at order cannot be what keeps
		// the walk exact.
		if _, err := srv.store.DB().Exec(srv.store.D().Rebind(`UPDATE items SET updated_at = '2020-01-01T00:00:00Z' WHERE workspace_id = ? AND collection_id <> ?`), wsID, tasks.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := srv.store.DB().Exec(srv.store.D().Rebind(`UPDATE items SET updated_at = '2026-01-01T00:00:00Z' WHERE collection_id = ? AND deleted_at IS NULL`), tasks.ID); err != nil {
			t.Fatal(err)
		}
		// They tie on seq as well, which no write path produces but the column
		// does not forbid: the id tie-break is what keeps the walk exact then.
		if _, err := srv.store.DB().Exec(srv.store.D().Rebind(`UPDATE items SET seq = (SELECT MAX(seq) FROM items WHERE workspace_id = ?) WHERE collection_id = ?`), wsID, tasks.ID); err != nil {
			t.Fatal(err)
		}
		sinceMs := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()

		var touchedEarly, touchedLate, added *models.Item
		const renamed = "Walk renamed mid-walk"
		pages := bug3791Walk(t, srv, slug, "", sinceMs, 3, func(page int, p bug3791Page) {
			if page != 0 {
				return
			}
			firstPageIDs := map[string]bool{}
			for _, it := range p.Updated {
				firstPageIDs[it.ID] = true
			}
			// A made row page 0 returned, and one it did not.
			for _, it := range made {
				if it.ID == made[4].ID {
					continue
				}
				if firstPageIDs[it.ID] && touchedEarly == nil {
					touchedEarly = it
				}
				if !firstPageIDs[it.ID] && touchedLate == nil {
					touchedLate = it
				}
			}
			if touchedLate == nil {
				t.Fatal("precondition: page 0 returned every made row")
			}
			if touchedEarly == nil {
				t.Fatal("precondition: page 0 returned no made row")
			}
			for _, it := range []*models.Item{touchedEarly, touchedLate} {
				title := renamed
				if _, err := srv.store.UpdateItem(it.ID, models.ItemUpdate{Title: &title}); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			added, err = srv.store.CreateItem(wsID, tasks.ID, models.ItemCreate{Title: "Walk added mid-walk", Fields: `{"status":"open"}`})
			if err != nil {
				t.Fatal(err)
			}
		})

		seen := map[string]int{}
		latestTitle := map[string]string{}
		deleted := map[string]bool{}
		for i, p := range pages {
			if p.ServerTime != pages[0].ServerTime {
				t.Fatalf("page %d server_time %d, first page %d", i, p.ServerTime, pages[0].ServerTime)
			}
			for _, it := range p.Updated {
				seen[it.ID]++
				latestTitle[it.ID] = it.Title
			}
			for _, id := range p.Deleted {
				deleted[id] = true
			}
		}
		if !deleted[made[4].ID] {
			t.Errorf("the deleted row is not in any page's deleted list")
		}
		for _, it := range made {
			if it == touchedEarly {
				continue
			}
			if seen[it.ID] != 1 {
				t.Errorf("%s returned %d times, want once", it.Title, seen[it.ID])
			}
		}
		if seen[added.ID] != 1 {
			t.Errorf("the row created mid-walk returned %d times, want once", seen[added.ID])
		}
		if latestTitle[touchedLate.ID] != renamed {
			t.Errorf("the row renamed before any page reached it ends the walk titled %q", latestTitle[touchedLate.ID])
		}
		if seen[touchedEarly.ID] != 2 || latestTitle[touchedEarly.ID] != renamed {
			t.Errorf("the row renamed after page 0 returned it: seen %d times, last title %q; want 2, %q", seen[touchedEarly.ID], latestTitle[touchedEarly.ID], renamed)
		}
	})
}

// Visibility is applied before the window, not after: a restricted member's
// walk returns only the rows they may see, and the has-more probe does not count
// hidden rows, so a page followed only by hidden rows is the last page.
func TestBUG3791_WalkWindowIsOverVisibleRows(t *testing.T) {
	srv := testServer(t)
	ownerCookie := bootstrapFirstUser(t, srv, "owner@test.com", "Owner")
	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "Vis 3791"}, ownerCookie)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)
	owner, _ := srv.store.GetUserByEmail("owner@test.com")
	tasks, _ := srv.store.GetCollectionBySlug(ws.ID, "tasks")
	ideas, _ := srv.store.GetCollectionBySlug(ws.ID, "ideas")
	docs, _ := srv.store.GetCollectionBySlug(ws.ID, "docs")
	if tasks == nil || ideas == nil || docs == nil {
		t.Fatal("precondition: startup collections")
	}
	mk := func(collID, title string) *models.Item {
		it, err := srv.store.CreateItem(ws.ID, collID, models.ItemCreate{Title: title})
		if err != nil {
			t.Fatal(err)
		}
		return it
	}
	// Write order: the visible rows first, then only hidden ones.
	visibleTask := mk(tasks.ID, "Task in an assigned collection")
	grantedIdea := mk(ideas.ID, "Granted idea")
	hidden := []*models.Item{mk(ideas.ID, "Hidden idea"), mk(docs.ID, "Hidden doc"), mk(ideas.ID, "Hidden idea 2")}

	m, err := srv.store.CreateUser(models.UserCreate{Email: "restricted@test.com", Name: "Restricted", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, m.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.SetMemberCollectionAccess(ws.ID, m.ID, "specific", []string{tasks.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.CreateItemGrant(ws.ID, grantedIdea.ID, m.ID, "view", owner.ID); err != nil {
		t.Fatal(err)
	}
	cookie, err := srv.store.CreateSession(m.ID, "web-test", "192.0.2.1", "", webSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	// Only the rows made above are after since.
	if _, err := srv.store.DB().Exec(srv.store.D().Rebind(`UPDATE items SET updated_at = '2020-01-01T00:00:00Z' WHERE workspace_id = ?`), ws.ID); err != nil {
		t.Fatal(err)
	}
	ids := []string{visibleTask.ID, grantedIdea.ID}
	for _, h := range hidden {
		ids = append(ids, h.ID)
	}
	for _, id := range ids {
		if _, err := srv.store.DB().Exec(srv.store.D().Rebind(`UPDATE items SET updated_at = '2026-01-01T00:00:00Z' WHERE id = ?`), id); err != nil {
			t.Fatal(err)
		}
	}
	sinceMs := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()

	pages := bug3791Walk(t, srv, ws.Slug, cookie, sinceMs, 1, nil)
	var got []string
	for _, p := range pages {
		for _, it := range p.Updated {
			got = append(got, it.Title)
		}
	}
	if len(pages) != 2 || len(got) != 2 || got[0] != visibleTask.Title || got[1] != grantedIdea.Title {
		t.Fatalf("restricted walk: %d pages, rows %q; want 2 pages: [%q %q]", len(pages), got, visibleTask.Title, grantedIdea.Title)
	}

	// The owner's walk over the same window sees all five.
	owned := 0
	for _, p := range bug3791Walk(t, srv, ws.Slug, ownerCookie, sinceMs, 2, nil) {
		owned += len(p.Updated)
	}
	if owned != 5 {
		t.Fatalf("owner walk returned %d rows, want 5", owned)
	}
}

func TestBUG3791_CursorIsValidated(t *testing.T) {
	srv := testServer(t)
	slug, wsID := task3462Workspace(t, srv, "Cursor 3791", "startup")
	_ = wsID
	base := "/api/v1/workspaces/" + slug + "/changes"
	p := bug3791Get(t, srv, base+"?since=0&limit=1", "")
	if !p.HasMore {
		t.Fatal("precondition: the template seeds more than one row")
	}
	for _, q := range []string{
		"?cursor=garbage",
		"?cursor=c1.!!!",
		"?cursor=" + changesCursor{SinceMs: 0, ServerTimeMs: 1}.encode(), // no row id
		"?since=5&cursor=" + p.NextCursor,
	} {
		if rr := doRequest(srv, "GET", base+q, nil); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", q, rr.Code)
		}
	}
	if rr := doRequest(srv, "GET", base+"?since=0&cursor="+p.NextCursor, nil); rr.Code != http.StatusOK {
		t.Errorf("a since equal to the cursor's: %d, want 200", rr.Code)
	}
}
