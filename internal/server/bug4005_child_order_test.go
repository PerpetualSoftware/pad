package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-4005: the HTTP create tells the store whether the new item is a child
// (the parent link is written after the insert), so a parentless item lands
// on top of its collection and a child appends under its parent.
func TestBUG4005_CreateOverHTTPParentlessOnTopChildAppends(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSForTest(t, srv)
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{
		"name": "Shelf", "prefix": "SHLF",
		"schema": `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"]}]}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection: %d %s", rr.Code, rr.Body.String())
	}
	create := func(title string, extra map[string]interface{}) models.Item {
		t.Helper()
		fields := map[string]interface{}{"status": "open"}
		for k, v := range extra {
			fields[k] = v
		}
		rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/shelf/items", map[string]interface{}{
			"title": title, "fields": fields,
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", title, rr.Code, rr.Body.String())
		}
		var it models.Item
		parseJSON(t, rr, &it)
		return it
	}
	a := create("A", nil)
	b := create("B", nil)
	if b.SortOrder >= a.SortOrder {
		t.Errorf("parentless B sort_order %d should be below A's %d", b.SortOrder, a.SortOrder)
	}
	c1 := create("C1", map[string]interface{}{"parent": a.ID})
	c2 := create("C2", map[string]interface{}{"parent": a.ID})
	if c1.SortOrder != 0 || c2.SortOrder != 0 {
		t.Errorf("children sort_order = %d, %d; want 0, 0", c1.SortOrder, c2.SortOrder)
	}
}
