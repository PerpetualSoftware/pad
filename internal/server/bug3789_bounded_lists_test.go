package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3789, BUG-3790, BUG-3792: the children list, the starred list and a
// shared collection are bounded like the item lists (BUG-3704). A missing or
// invalid limit is the maximum, an explicit one is clamped, and the paging
// state comes back in headers (and, on the share payload, in the body).

type bug3789Page struct {
	items   []models.Item
	more    string
	next    string
	rawBody []byte
}

func bug3789Get(t *testing.T, srv *Server, path, cookie string) bug3789Page {
	t.Helper()
	rr := doRequest(srv, "GET", path, nil)
	if cookie != "" {
		rr = doRequestWithCookie(srv, "GET", path, nil, cookie)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
	}
	var items []models.Item
	_ = json.Unmarshal(rr.Body.Bytes(), &items)
	return bug3789Page{items, rr.Header().Get(itemListHasMoreHeader), rr.Header().Get(itemListNextOffsetHeader), rr.Body.Bytes()}
}

// walk follows next-offset from offset 0, asking for pageSize rows a page.
func bug3789Walk(t *testing.T, srv *Server, base, cookie string, pageSize int) ([]models.Item, int) {
	t.Helper()
	sep := "?"
	if containsByte(base, '?') {
		sep = "&"
	}
	var all []models.Item
	offset, pages := 0, 0
	for {
		p := bug3789Get(t, srv, fmt.Sprintf("%s%slimit=%d&offset=%d", base, sep, pageSize, offset), cookie)
		pages++
		all = append(all, p.items...)
		if p.more == "false" {
			if p.next != "" {
				t.Fatalf("last page carries next-offset %q", p.next)
			}
			return all, pages
		}
		if p.more != "true" || p.next != fmt.Sprint(offset+pageSize) {
			t.Fatalf("page at %d: has-more %q next %q", offset, p.more, p.next)
		}
		offset += pageSize
		if pages > 100 {
			t.Fatal("walk did not end")
		}
	}
}

func containsByte(s string, b byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return true
		}
	}
	return false
}

func TestBUG3789_ChildrenAreBoundedAndPaged(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug, wsID := task3462Workspace(t, srv, "Children 3789", "startup")
		tasks, _ := srv.store.GetCollectionBySlug(wsID, "tasks")
		parent, err := srv.store.CreateItem(wsID, tasks.ID, models.ItemCreate{Title: "Parent", Fields: `{"status":"open"}`})
		if err != nil {
			t.Fatal(err)
		}
		n := maxItemListQueryLimit + 1
		for i := 0; i < n; i++ {
			child, err := srv.store.CreateItem(wsID, tasks.ID, models.ItemCreate{Title: fmt.Sprintf("Child %04d", i), Fields: `{"status":"open"}`})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := srv.store.CreateItemLink(wsID, models.ItemLinkCreate{TargetID: parent.ID, LinkType: "parent"}, child.ID); err != nil {
				t.Fatal(err)
			}
		}
		// Every child ties on the list order's leading keys.
		if _, err := srv.store.DB().Exec(srv.store.D().Rebind(`UPDATE items SET sort_order = 0, created_at = '2026-01-01T00:00:00Z' WHERE collection_id = ? AND id <> ?`), tasks.ID, parent.ID); err != nil {
			t.Fatal(err)
		}
		base := "/api/v1/workspaces/" + slug + "/items/" + parent.Slug + "/children"
		for _, q := range []string{"", "?limit=0", "?limit=-5", "?limit=abc", "?limit=5000"} {
			p := bug3789Get(t, srv, base+q, "")
			if len(p.items) != maxItemListQueryLimit || p.more != "true" || p.next != fmt.Sprint(maxItemListQueryLimit) {
				t.Fatalf("%s: %d rows, has-more %q, next %q", q, len(p.items), p.more, p.next)
			}
		}
		all, _ := bug3789Walk(t, srv, base, "", 400)
		seen := map[string]int{}
		for _, it := range all {
			seen[it.ID]++
		}
		if len(seen) != n || len(all) != n {
			t.Fatalf("walk returned %d rows, %d distinct; want %d each", len(all), len(seen), n)
		}
	})
}

// A restricted member's children window counts only children they may see,
// and has_children reflects only visible grandchildren.
func TestBUG3789_ChildrenWindowIsOverVisibleRows(t *testing.T) {
	srv := testServer(t)
	ownerCookie := bootstrapFirstUser(t, srv, "owner@test.com", "Owner")
	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "Vis 3789"}, ownerCookie)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)
	tasks, _ := srv.store.GetCollectionBySlug(ws.ID, "tasks")
	ideas, _ := srv.store.GetCollectionBySlug(ws.ID, "ideas")
	mk := func(collID, title string, parent *models.Item) *models.Item {
		it, err := srv.store.CreateItem(ws.ID, collID, models.ItemCreate{Title: title})
		if err != nil {
			t.Fatal(err)
		}
		if parent != nil {
			if _, err := srv.store.CreateItemLink(ws.ID, models.ItemLinkCreate{TargetID: parent.ID, LinkType: "parent"}, it.ID); err != nil {
				t.Fatal(err)
			}
		}
		return it
	}
	parent := mk(tasks.ID, "Parent", nil)
	visible := mk(tasks.ID, "Visible child", parent)
	for i := 0; i < 3; i++ {
		mk(ideas.ID, fmt.Sprintf("Hidden child %d", i), parent)
	}
	mk(ideas.ID, "Hidden grandchild", visible)
	if _, err := srv.store.DB().Exec(srv.store.D().Rebind(`UPDATE items SET sort_order = 0, created_at = '2026-01-01T00:00:00Z' WHERE workspace_id = ?`), ws.ID); err != nil {
		t.Fatal(err)
	}
	// The visible child sorts first.
	if _, err := srv.store.DB().Exec(srv.store.D().Rebind(`UPDATE items SET sort_order = -1 WHERE id = ?`), visible.ID); err != nil {
		t.Fatal(err)
	}

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
	cookie, err := srv.store.CreateSession(m.ID, "web-test", "192.0.2.1", "", webSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/workspaces/" + ws.Slug + "/items/" + parent.Slug + "/children"
	p := bug3789Get(t, srv, base+"?limit=1", cookie)
	if len(p.items) != 1 || p.items[0].ID != visible.ID || p.more != "false" {
		t.Fatalf("restricted page: %d rows, has-more %q; want only the visible child and has-more false", len(p.items), p.more)
	}
	if p.items[0].HasChildren {
		t.Fatal("has_children counts a grandchild the member cannot see")
	}
	owner := bug3789Get(t, srv, base, ownerCookie)
	if len(owner.items) != 4 {
		t.Fatalf("owner sees %d children, want 4", len(owner.items))
	}
	for _, it := range owner.items {
		if it.ID == visible.ID && !it.HasChildren {
			t.Fatal("owner: has_children misses the grandchild")
		}
	}
}

func TestBUG3790_StarredIsBoundedPagedAndLoadsBodiesForThePage(t *testing.T) {
	srv := testServer(t)
	ownerCookie := bootstrapFirstUser(t, srv, "owner@test.com", "Owner")
	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "Stars 3790"}, ownerCookie)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)
	owner, _ := srv.store.GetUserByEmail("owner@test.com")
	tasks, _ := srv.store.GetCollectionBySlug(ws.ID, "tasks")
	n := maxItemListQueryLimit + 1
	for i := 0; i < n; i++ {
		it, err := srv.store.CreateItem(ws.ID, tasks.ID, models.ItemCreate{Title: fmt.Sprintf("Star %04d", i), Content: fmt.Sprintf("body %04d", i), Fields: `{"status":"open"}`})
		if err != nil {
			t.Fatal(err)
		}
		if err := srv.store.StarItem(owner.ID, it.ID); err != nil {
			t.Fatal(err)
		}
	}
	// Every star ties on starred-at.
	if _, err := srv.store.DB().Exec(`UPDATE item_stars SET created_at = '2026-01-01T00:00:00Z'`); err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/workspaces/" + ws.Slug + "/starred"
	for _, q := range []string{"", "?limit=0", "?limit=abc", "?limit=5000"} {
		p := bug3789Get(t, srv, base+q, ownerCookie)
		if len(p.items) != maxItemListQueryLimit || p.more != "true" || p.next != fmt.Sprint(maxItemListQueryLimit) {
			t.Fatalf("%s: %d rows, has-more %q, next %q", q, len(p.items), p.more, p.next)
		}
	}
	all, _ := bug3789Walk(t, srv, base, ownerCookie, 300)
	seen := map[string]bool{}
	for _, it := range all {
		if seen[it.ID] {
			t.Fatalf("%s returned twice", it.Title)
		}
		seen[it.ID] = true
		if it.Content != "body "+it.Title[len("Star "):] {
			t.Fatalf("%s: body %q", it.Title, it.Content)
		}
	}
	if len(seen) != n {
		t.Fatalf("walk returned %d distinct, want %d", len(seen), n)
	}
	summary := bug3789Get(t, srv, base+"?summary=true&limit=5", ownerCookie)
	for _, it := range summary.items {
		if it.Content != "" {
			t.Fatalf("summary carries a body: %q", it.Content)
		}
	}
}

func TestBUG3792_SharedCollectionIsBoundedAndPaged(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		_, wsID := task3462Workspace(t, srv, "Share 3792", "startup")
		tasks, _ := srv.store.GetCollectionBySlug(wsID, "tasks")
		n := maxItemListQueryLimit + 1
		for i := 0; i < n; i++ {
			if _, err := srv.store.CreateItem(wsID, tasks.ID, models.ItemCreate{Title: fmt.Sprintf("Shared %04d", i), Fields: `{"status":"open"}`}); err != nil {
				t.Fatal(err)
			}
		}
		creator, err := srv.store.CreateUser(models.UserCreate{Email: "sharer@test.com", Name: "Sharer", Password: "correct-horse-battery-staple"})
		if err != nil {
			t.Fatal(err)
		}
		link, err := srv.store.CreateShareLink(wsID, "collection", tasks.ID, "view", creator.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		get := func(q string) (items []map[string]any, hasMore bool, next float64) {
			t.Helper()
			req := httptest.NewRequest("GET", "/api/v1/s/"+link.Token+q, nil)
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", q, rr.Code, rr.Body.String())
			}
			var body struct {
				Items      []map[string]any `json:"items"`
				HasMore    bool             `json:"has_more"`
				NextOffset float64          `json:"next_offset"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			return body.Items, body.HasMore, body.NextOffset
		}
		for _, q := range []string{"", "?limit=0", "?limit=abc", "?limit=5000"} {
			items, more, next := get(q)
			if len(items) != maxItemListQueryLimit || !more || next != maxItemListQueryLimit {
				t.Fatalf("%s: %d items, has_more %v, next %v", q, len(items), more, next)
			}
		}
		seen := map[string]bool{}
		for offset := 0; ; {
			items, more, next := get(fmt.Sprintf("?limit=400&offset=%d", offset))
			for _, it := range items {
				ref := fmt.Sprint(it["ref"])
				if seen[ref] {
					t.Fatalf("%s returned twice", ref)
				}
				seen[ref] = true
			}
			if !more {
				break
			}
			offset = int(next)
		}
		if len(seen) != n {
			t.Fatalf("walk returned %d items, want %d", len(seen), n)
		}
	})
}

// Rows that tie on every leading sort key come back in a total order: item
// number for children (creation order, BUG-4005), id for stars. The links and
// stars are written in DESCENDING id order, so the scan order the database
// would fall back to without the id tie-break is the reverse: an order that is
// not total lets offset pages repeat or skip rows (SQLite happens to keep
// paging consistent by scan order; Postgres does not promise it).
func TestBUG3789_TiesComeBackInATotalOrder(t *testing.T) {
	srv := testServer(t)
	ownerCookie := bootstrapFirstUser(t, srv, "owner@test.com", "Owner")
	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "Ties 3789"}, ownerCookie)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)
	owner, _ := srv.store.GetUserByEmail("owner@test.com")
	tasks, _ := srv.store.GetCollectionBySlug(ws.ID, "tasks")
	parent, err := srv.store.CreateItem(ws.ID, tasks.ID, models.ItemCreate{Title: "Parent"})
	if err != nil {
		t.Fatal(err)
	}
	var ids, creationOrder []string
	for i := 0; i < 6; i++ {
		it, err := srv.store.CreateItem(ws.ID, tasks.ID, models.ItemCreate{Title: fmt.Sprintf("Tie %d", i)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, it.ID)
	}
	creationOrder = append(creationOrder, ids...)
	sort.Strings(ids)
	for i := len(ids) - 1; i >= 0; i-- {
		if _, err := srv.store.CreateItemLink(ws.ID, models.ItemLinkCreate{TargetID: parent.ID, LinkType: "parent"}, ids[i]); err != nil {
			t.Fatal(err)
		}
		if err := srv.store.StarItem(owner.ID, ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := srv.store.DB().Exec(srv.store.D().Rebind(`UPDATE items SET sort_order = 0, created_at = '2026-01-01T00:00:00Z' WHERE collection_id = ? AND id <> ?`), tasks.ID, parent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.DB().Exec(`UPDATE item_stars SET created_at = '2026-01-01T00:00:00Z'`); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string][]string{
		"/api/v1/workspaces/" + ws.Slug + "/items/" + parent.Slug + "/children": creationOrder,
		"/api/v1/workspaces/" + ws.Slug + "/starred":                            ids,
	} {
		all, pages := bug3789Walk(t, srv, path, ownerCookie, 2)
		var got []string
		for _, it := range all {
			got = append(got, it.ID)
		}
		if pages != 3 || fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s: %d pages, order %v; want 3 pages in order %v", path, pages, got, want)
		}
	}
}

// Lead check (#2153): an offset past the end is an empty page that serialises
// as [] (not null), with has-more false, on both bare-array lists.
func TestBUG3789_AnOffsetPastTheEndIsAnEmptyArray(t *testing.T) {
	srv := testServer(t)
	ownerCookie := bootstrapFirstUser(t, srv, "owner@test.com", "Owner")
	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "End 3789"}, ownerCookie)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)
	owner, _ := srv.store.GetUserByEmail("owner@test.com")
	tasks, _ := srv.store.GetCollectionBySlug(ws.ID, "tasks")
	parent, _ := srv.store.CreateItem(ws.ID, tasks.ID, models.ItemCreate{Title: "Parent"})
	child, _ := srv.store.CreateItem(ws.ID, tasks.ID, models.ItemCreate{Title: "Child"})
	if _, err := srv.store.CreateItemLink(ws.ID, models.ItemLinkCreate{TargetID: parent.ID, LinkType: "parent"}, child.ID); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.StarItem(owner.ID, child.ID); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/api/v1/workspaces/" + ws.Slug + "/starred?offset=5",
		"/api/v1/workspaces/" + ws.Slug + "/items/" + parent.Slug + "/children?offset=5",
	} {
		p := bug3789Get(t, srv, path, ownerCookie)
		if body := string(p.rawBody); body != "[]\n" && body != "[]" {
			t.Fatalf("%s: body %q, want []", path, body)
		}
		if p.more != "false" || p.next != "" {
			t.Fatalf("%s: has-more %q next %q", path, p.more, p.next)
		}
	}
}
