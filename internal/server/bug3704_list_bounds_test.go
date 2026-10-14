package server

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3704: every HTTP item list is bounded. A missing, zero, negative or
// invalid limit takes the maximum (lead ruling: an old client then loses rows
// only past it), an explicit one is clamped to it, and the paging state comes
// back in headers on every response, because the body is a bare array.

func bug3704List(t *testing.T, srv *Server, path string) ([]models.Item, string, string) {
	t.Helper()
	rr := doRequest(srv, "GET", path, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
	}
	var items []models.Item
	parseJSON(t, rr, &items)
	return items, rr.Header().Get(itemListHasMoreHeader), rr.Header().Get(itemListNextOffsetHeader)
}

func TestBUG3704_ItemListsAreBoundedAndPaged(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug, wsID := task3462Workspace(t, srv, "Bounds 3704", "startup")
		tasks, err := srv.store.GetCollectionBySlug(wsID, "tasks")
		if err != nil || tasks == nil {
			t.Fatalf("tasks: %v", err)
		}
		n := maxItemListQueryLimit + 1
		for i := 0; i < n; i++ {
			if _, err := srv.store.CreateItem(wsID, tasks.ID, models.ItemCreate{Title: fmt.Sprintf("Bounded %04d", i), Fields: `{"status":"open"}`}); err != nil {
				t.Fatal(err)
			}
		}
		for _, base := range []string{
			"/api/v1/workspaces/" + slug + "/items",
			"/api/v1/workspaces/" + slug + "/collections/tasks/items",
		} {
			for _, q := range []string{"", "?limit=0", "?limit=-5", "?limit=abc", "?limit=5000"} {
				items, more, next := bug3704List(t, srv, base+q)
				if len(items) != maxItemListQueryLimit || more != "true" || next != fmt.Sprint(maxItemListQueryLimit) {
					t.Fatalf("%s%s: %d items, has-more %q, next %q; want %d, true, %d", base, q, len(items), more, next, maxItemListQueryLimit, maxItemListQueryLimit)
				}
			}
			// The last page says there is no more, and carries no next offset.
			// The workspace list also holds the template's seeded items.
			params := models.ItemListParams{}
			if base != "/api/v1/workspaces/"+slug+"/items" {
				params.ScopeCollectionID = tasks.ID
			}
			all, err := srv.store.ListItems(wsID, params)
			if err != nil {
				t.Fatal(err)
			}
			items, more, next := bug3704List(t, srv, fmt.Sprintf("%s?offset=%d", base, maxItemListQueryLimit))
			if len(items) != len(all)-maxItemListQueryLimit || more != "false" || next != "" {
				t.Fatalf("%s last page: %d items (of %d), has-more %q, next %q", base, len(items), len(all), more, next)
			}
		}
	})
}

// Paging by next-offset visits every row exactly once, also on the ?search=
// path, which used to drop the offset, and across rows that tie on the sort.
func TestBUG3704_PagingVisitsEveryRowOnce(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug, wsID := task3462Workspace(t, srv, "Paging 3704", "startup")
		tasks, err := srv.store.GetCollectionBySlug(wsID, "tasks")
		if err != nil || tasks == nil {
			t.Fatalf("tasks: %v", err)
		}
		want := map[string]bool{}
		for i := 0; i < 7; i++ {
			it, err := srv.store.CreateItem(wsID, tasks.ID, models.ItemCreate{Title: fmt.Sprintf("Pagewalk %d", i), Fields: `{"status":"open"}`})
			if err != nil {
				t.Fatal(err)
			}
			want[it.ID] = true
		}
		// Every row ties on the default sort keys (pinned, updated_at).
		if _, err := srv.store.DB().Exec(srv.store.D().Rebind(`UPDATE items SET updated_at = '2026-01-01T00:00:00Z' WHERE collection_id = ?`), tasks.ID); err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{"", "search=Pagewalk&"} {
			seen := map[string]int{}
			offset, pages := 0, 0
			for {
				items, more, next := bug3704List(t, srv, fmt.Sprintf("/api/v1/workspaces/%s/collections/tasks/items?%slimit=3&offset=%d", slug, q, offset))
				pages++
				for _, it := range items {
					seen[it.ID]++
				}
				if more != "true" {
					break
				}
				if _, err := fmt.Sscan(next, &offset); err != nil || pages > 10 {
					t.Fatalf("q=%q: next %q after %d pages", q, next, pages)
				}
			}
			if len(seen) != len(want) || pages != 3 {
				t.Fatalf("q=%q: %d distinct rows in %d pages, want %d in 3", q, len(seen), pages, len(want))
			}
			for id, c := range seen {
				if !want[id] || c != 1 {
					t.Fatalf("q=%q: row %s seen %d times", q, id, c)
				}
			}
		}
	})
}
