package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3480: the item list endpoints read every query parameter they do not
// know as a field filter, so a parameter the server never had (the web
// Library page's `all`, WebMCP's `collection`) answered 200 with an empty list
// and nothing said why. A filter key must now name a field a schema in scope
// declares, or one items in scope store (honoured, and named in a header).

func bug3480List(t *testing.T, srv *Server, path string) (int, []models.Item, string, string) {
	t.Helper()
	rr := doRequest(srv, "GET", path, nil)
	var items []models.Item
	if rr.Code == http.StatusOK {
		parseJSON(t, rr, &items)
	}
	return rr.Code, items, rr.Header().Get(undeclaredFilterKeysHeader), rr.Body.String()
}

func TestBUG3480_ListFilterKeys(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug, wsID := task3462Workspace(t, srv, "Filters 3480", "startup")
		tasks, err := srv.store.GetCollectionBySlug(wsID, "tasks")
		if err != nil || tasks == nil {
			t.Fatalf("tasks collection: %v", err)
		}
		// A key no schema declares, stored on one task (what the BUG-2850
		// census found in live data). The store does not validate fields.
		legacy, err := srv.store.CreateItem(wsID, tasks.ID, models.ItemCreate{Title: "Carries a legacy key", Fields: `{"status":"open","legacy_key":"x"}`})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := srv.store.CreateItem(wsID, tasks.ID, models.ItemCreate{Title: "High task", Fields: `{"status":"open","priority":"high"}`}); err != nil {
			t.Fatal(err)
		}
		coll := "/api/v1/workspaces/" + slug + "/collections/"
		ws := "/api/v1/workspaces/" + slug + "/items"

		refused := func(path, key string) {
			t.Helper()
			code, _, _, body := bug3480List(t, srv, path)
			if code != http.StatusBadRequest || !strings.Contains(body, "validation_error") ||
				!strings.Contains(body, `invalid list filter \"`+key+`\"`) {
				t.Errorf("%s: got %d %s, want 400 validation_error naming %q", path, code, body, key)
			}
		}

		// The live victims: a parameter the server never had.
		refused(coll+"conventions/items?all=true", "all")
		refused(ws+"?collection=tasks", "collection")
		// A key that is not a field key used to be DROPPED by the store, which
		// answered the unfiltered list.
		refused(coll+"tasks/items?bad.key=1", "bad.key")
		// A schema may DECLARE a key the store cannot filter on (no charset
		// rule on schema keys), and the store drops such a filter, answering
		// the unfiltered list. Refused here before it gets that far.
		rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]any{
			"name":   "Odd 3480",
			"schema": `{"fields":[{"key":"odd.key","label":"Odd","type":"text"}]}`,
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("create odd collection: %d %s", rr.Code, rr.Body.String())
		}
		var odd models.Collection
		parseJSON(t, rr, &odd)
		if _, err := srv.store.CreateItem(wsID, odd.ID, models.ItemCreate{Title: "Odd one", Fields: `{"odd.key":"y"}`}); err != nil {
			t.Fatal(err)
		}
		refused(coll+odd.Slug+"/items?odd.key=nope", "odd.key")

		// Declared by tasks, not by ideas: the collection list is scoped to its
		// own schema; the workspace list accepts it.
		refused(coll+"ideas/items?priority=high", "priority")
		if code, items, hdr, body := bug3480List(t, srv, ws+"?priority=high"); code != http.StatusOK || len(items) != 1 || hdr != "" {
			t.Errorf("workspace ?priority=high: %d, %d items, header %q: %s", code, len(items), hdr, body)
		}
		// A declared field filters as before, comma OR included.
		if code, items, hdr, body := bug3480List(t, srv, coll+"tasks/items?priority=high,low"); code != http.StatusOK || len(items) != 1 || hdr != "" {
			t.Errorf("tasks ?priority=high,low: %d, %d items, header %q: %s", code, len(items), hdr, body)
		}

		// Stored but undeclared: honoured, and named in the header.
		for _, path := range []string{coll + "tasks/items?legacy_key=x", ws + "?legacy_key=x"} {
			code, items, hdr, body := bug3480List(t, srv, path)
			if code != http.StatusOK || len(items) != 1 || items[0].ID != legacy.ID || hdr != "legacy_key" {
				t.Errorf("%s: %d, %d items, header %q: %s", path, code, len(items), hdr, body)
			}
		}
		// The stored check is scoped like the list: ideas stores no legacy_key.
		refused(coll+"ideas/items?legacy_key=x", "legacy_key")

		// Keys the handlers consume are not field filters.
		if code, _, _, body := bug3480List(t, srv, coll+"tasks/items?parent="+legacy.Slug); code != http.StatusOK {
			t.Errorf("?parent: %d %s", code, body)
		}
		if code, _, _, body := bug3480List(t, srv, ws+"?unparented=true&non_terminal=true&include_archived=false&limit=5&offset=0&sort=title:asc"); code != http.StatusOK {
			t.Errorf("known params: %d %s", code, body)
		}
	})
}
