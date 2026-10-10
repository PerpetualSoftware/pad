package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3530: a shared collection lists its items in the owner's arranged
// (manual) order: sort_order, then oldest first, then id. It used to be
// ListItems' default, pinned and then most recently updated, so every edit
// reshuffled what a reader saw.
func TestBUG3530_SharedCollectionKeepsTheManualOrder(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSForTest(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{
		"name":   "Shelf",
		"prefix": "SHLF",
		"schema": `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"]}]}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection: %d %s", rr.Code, rr.Body.String())
	}
	var coll models.Collection
	parseJSON(t, rr, &coll)

	create := func(title string) models.Item {
		t.Helper()
		rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/shelf/items", map[string]interface{}{
			"title": title, "fields": map[string]interface{}{"status": "open"},
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", title, rr.Code, rr.Body.String())
		}
		var it models.Item
		parseJSON(t, rr, &it)
		return it
	}
	a, b, c, d, e := create("A"), create("B"), create("C"), create("D"), create("E")

	// Arranged order: B (-2048), C (-1024), D and E tied at 0 with D older,
	// then A (1024).
	for _, w := range []struct {
		it models.Item
		v  int
	}{{a, 1024}, {b, -2048}, {c, -1024}} {
		v := w.v
		if _, err := srv.store.UpdateItem(w.it.ID, models.ItemUpdate{SortOrder: &v}); err != nil {
			t.Fatalf("set sort_order of %s: %v", w.it.Title, err)
		}
	}
	// created_at has one-second resolution, so set the tie's order explicitly.
	if _, err := srv.store.DB().Exec(srv.store.D().Rebind(`UPDATE items SET created_at = ? WHERE id = ?`), "2026-01-01T00:00:00Z", d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.DB().Exec(srv.store.D().Rebind(`UPDATE items SET created_at = ? WHERE id = ?`), "2026-01-02T00:00:00Z", e.ID); err != nil {
		t.Fatal(err)
	}
	// Make the old default order differ: A is the most recently updated and
	// pinned, so the default would list it first.
	pinned := true
	if _, err := srv.store.UpdateItem(a.ID, models.ItemUpdate{Pinned: &pinned}); err != nil {
		t.Fatalf("pin A: %v", err)
	}

	owner, err := srv.store.CreateUser(models.UserCreate{Email: "owner3530@test.com", Name: "Owner", Password: "pw-owner"})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("get workspace: %v", err)
	}
	link, err := srv.store.CreateShareLink(ws.ID, "collection", coll.ID, "view", owner.ID, nil)
	if err != nil {
		t.Fatal(err)
	}

	rr = doRequest(srv, "GET", "/api/v1/s/"+link.Token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Items []struct {
			Title string `json:"title"`
		} `json:"items"`
	}
	parseJSON(t, rr, &resp)
	var got []string
	for _, it := range resp.Items {
		got = append(got, it.Title)
	}
	want := []string{"B", "C", "D", "E", "A"}
	if len(got) != len(want) {
		t.Fatalf("items %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("shared order %v, want the arranged order %v", got, want)
		}
	}
}
