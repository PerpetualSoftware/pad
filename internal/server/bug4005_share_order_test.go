package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-4005 r2: a shared collection breaks a same-second tie the way the board
// does, by item number, not by the random id. The two items tie on sort_order
// and created_at, and their item numbers are arranged to run OPPOSITE to their
// ids, so an id tie-break would list them in the other order.
func TestBUG4005_SharedCollectionBreaksSameSecondTiesByItemNumber(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSForTest(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{
		"name":   "Ties",
		"prefix": "TIE",
		"schema": `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"]}]}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection: %d %s", rr.Code, rr.Body.String())
	}
	var coll models.Collection
	parseJSON(t, rr, &coll)

	create := func(title string) models.Item {
		t.Helper()
		rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/ties/items", map[string]interface{}{
			"title": title, "fields": map[string]interface{}{"status": "open"},
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", title, rr.Code, rr.Body.String())
		}
		var it models.Item
		parseJSON(t, rr, &it)
		return it
	}
	x, y := create("X"), create("Y")

	// hi has the larger id. Give it the SMALLER item number, so item-number
	// order is [hi, lo] and id order is [lo, hi].
	hi, lo := x, y
	if lo.ID > hi.ID {
		hi, lo = lo, hi
	}
	if x.ItemNumber == nil || y.ItemNumber == nil {
		t.Fatal("created items carry no item_number")
	}
	small, big := *x.ItemNumber, *y.ItemNumber
	if small > big {
		small, big = big, small
	}
	exec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := srv.store.DB().Exec(srv.store.D().Rebind(q), args...); err != nil {
			t.Fatal(err)
		}
	}
	// Swap through a free number: item_number is unique per workspace.
	exec(`UPDATE items SET item_number = ? WHERE id = ?`, 999999, hi.ID)
	exec(`UPDATE items SET item_number = ? WHERE id = ?`, big, lo.ID)
	exec(`UPDATE items SET item_number = ? WHERE id = ?`, small, hi.ID)
	exec(`UPDATE items SET sort_order = 0, created_at = ? WHERE id IN (?, ?)`, "2026-01-01T00:00:00Z", x.ID, y.ID)

	owner, err := srv.store.CreateUser(models.UserCreate{Email: "owner4005@test.com", Name: "Owner", Password: "pw-owner"})
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
	// The share payload carries titles, not ids.
	if len(got) != 2 || got[0] != hi.Title || got[1] != lo.Title {
		t.Fatalf("shared order = %v, want item-number order [%s %s] (id order would be [%s %s])", got, hi.Title, lo.Title, lo.Title, hi.Title)
	}
}
