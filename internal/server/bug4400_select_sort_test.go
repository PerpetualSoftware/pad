package server

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-4400 over HTTP: the item list's sort param (the one path the CLI's
// `item list --sort` and the MCP item list both call) ranks priority the way
// the dashboard does, whatever order each collection declares its options in.
func TestBUG4400_ListSortRanksPriority(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	wsm, _ := srv.store.GetWorkspaceBySlug(ws)
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections", map[string]any{
		"name":   "Polish",
		"schema": `{"fields":[{"key":"status","type":"select","options":["open","done"],"terminal_options":["done"]},{"key":"priority","type":"select","options":["high","medium","low"]}]}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection: %d %s", rr.Code, rr.Body.String())
	}
	mk := func(slug, title, pri string) {
		t.Helper()
		coll, err := srv.store.GetCollectionBySlug(wsm.ID, slug)
		if err != nil || coll == nil {
			t.Fatalf("collection %s: %v", slug, err)
		}
		fields := `{"status":"open"}`
		if pri != "" {
			fields = `{"status":"open","priority":"` + pri + `"}`
		}
		if _, err := srv.store.CreateItem(wsm.ID, coll.ID, models.ItemCreate{Title: title, Fields: fields}); err != nil {
			t.Fatal(err)
		}
	}
	mk("tasks", "t-low", "low")
	mk("tasks", "t-crit", "critical")
	mk("tasks", "t-none", "")
	mk("tasks", "t-high", "high")
	mk("polish", "p-low", "low")
	mk("polish", "p-high", "high")

	titles := func(path string) []string {
		t.Helper()
		rr := doRequest(srv, "GET", path, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, rr.Code, rr.Body.String())
		}
		var items []models.Item
		parseJSON(t, rr, &items)
		var out []string
		for _, it := range items {
			if strings.HasPrefix(it.Title, "t-") || strings.HasPrefix(it.Title, "p-") {
				out = append(out, it.Title)
			}
		}
		return out
	}
	base := "/api/v1/workspaces/" + ws
	if got, want := titles(base+"/items?sort=priority:desc,title:asc"), []string{"t-crit", "p-high", "t-high", "p-low", "t-low", "t-none"}; !reflect.DeepEqual(got, want) {
		t.Errorf("workspace list desc: %v, want %v", got, want)
	}
	if got, want := titles(base+"/collections/tasks/items?sort=priority:desc"), []string{"t-crit", "t-high", "t-low", "t-none"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tasks list desc: %v, want %v", got, want)
	}
	if got, want := titles(base+"/collections/tasks/items?sort=priority:asc"), []string{"t-low", "t-high", "t-crit", "t-none"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tasks list asc: %v, want %v", got, want)
	}
	if got, want := titles(base+"/collections/polish/items?sort=priority:desc"), []string{"p-high", "p-low"}; !reflect.DeepEqual(got, want) {
		t.Errorf("polish list desc: %v, want %v", got, want)
	}
}
