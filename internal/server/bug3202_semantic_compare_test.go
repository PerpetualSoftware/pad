package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3202, the lead's condition: field writes now keep a number's literal, so
// one VALUE can be stored as different TEXT (1e3 and 1000). Every place that
// compares field values must answer by value, in both directions:
//
//   - an equal value resent in another spelling is NOT a change (the text
//     differs, the value does not);
//   - two different integers above 2^53 ARE a change, even though a float64
//     comparison would call them equal.
//
// The item under test is created at the store, which writes the blob verbatim,
// so the stored spelling is exactly the one each case names.
func bug3202CompareFixture(t *testing.T) (*Server, string, func(title, fields string) *models.Item) {
	t.Helper()
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	wsm, _ := srv.store.GetWorkspaceBySlug(ws)
	schema := `{"fields":[{"key":"status","type":"select","options":["open","done"],"terminal_options":["done"]},{"key":"n","type":"number"}]}`
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections", map[string]any{"name": "Nums", "schema": schema})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection: %d %s", rr.Code, rr.Body.String())
	}
	coll, _ := srv.store.GetCollectionBySlug(wsm.ID, "nums")
	return srv, ws, func(title, fields string) *models.Item {
		it, err := srv.store.CreateItem(wsm.ID, coll.ID, models.ItemCreate{Title: title, Fields: fields})
		if err != nil {
			t.Fatal(err)
		}
		return it
	}
}

// The activity feed's `changes` metadata (diffFields).
func TestActivityChangesCompareNumbersByValue(t *testing.T) {
	srv, ws, mk := bug3202CompareFixture(t)
	changesFor := func(it *models.Item) string {
		acts, err := srv.store.ListDocumentActivity(it.ID, models.ActivityListParams{})
		if err != nil {
			t.Fatal(err)
		}
		var all []string
		for _, a := range acts {
			all = append(all, a.Metadata)
		}
		return strings.Join(all, " | ")
	}

	same := mk("same", `{"status":"open","n":1e3}`)
	if rr := rawJSONRequest(srv, "PATCH", "/api/v1/workspaces/"+ws+"/items/"+same.Slug, `{"fields_patch":{"n":1000}}`); rr.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rr.Code, rr.Body.String())
	}
	// PREMISE: the write landed and rewrote the stored spelling, so the two
	// sides the diff compared really were different text.
	if raw := mustGetItemFields(t, srv, same.ID); !strings.Contains(raw, `"n":1000`) {
		t.Fatalf("premise: the resend did not rewrite the stored text: %s", raw)
	}
	if got := changesFor(same); strings.Contains(got, "n:") {
		t.Errorf("resending 1000 over a stored 1e3 recorded a change: %s", got)
	}

	diff := mk("diff", `{"status":"open","n":9007199254740993}`)
	if rr := rawJSONRequest(srv, "PATCH", "/api/v1/workspaces/"+ws+"/items/"+diff.Slug, `{"fields_patch":{"n":9007199254740992}}`); rr.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rr.Code, rr.Body.String())
	}
	if got := changesFor(diff); !strings.Contains(got, "9007199254740993") || !strings.Contains(got, "9007199254740992") {
		t.Errorf("a change between two integers that share a float64 was not recorded with both values: %s", got)
	}
}

// The BUG-3163 carry check: a full `fields` write may carry a stored reserved
// value unchanged, compared by value.
func TestReservedCarryComparesNumbersByValue(t *testing.T) {
	srv, ws, mk := bug3202CompareFixture(t)
	const pr = `"url":"https://example.com/pr/1"`

	same := mk("same", `{"status":"open","github_pr":{"number":1e3,`+pr+`}}`)
	rr := rawJSONRequest(srv, "PATCH", "/api/v1/workspaces/"+ws+"/items/"+same.Slug,
		`{"fields":{"status":"done","github_pr":{"number":1000,`+pr+`}}}`)
	if rr.Code != http.StatusOK {
		t.Errorf("carrying github_pr.number as 1000 over a stored 1e3 was refused: %d %s", rr.Code, rr.Body.String())
	}

	// The same integer above 2^53, resent exactly: a carry. A float64
	// normalisation of the caller's side turns it into ...992 and refuses it.
	exact := mk("exact", `{"status":"open","github_pr":{"number":9007199254740993,`+pr+`}}`)
	rr = rawJSONRequest(srv, "PATCH", "/api/v1/workspaces/"+ws+"/items/"+exact.Slug,
		`{"fields":{"status":"done","github_pr":{"number":9007199254740993,`+pr+`}}}`)
	if rr.Code != http.StatusOK {
		t.Errorf("carrying github_pr.number 9007199254740993 unchanged was refused: %d %s", rr.Code, rr.Body.String())
	}

	diff := mk("diff", `{"status":"open","github_pr":{"number":9007199254740993,`+pr+`}}`)
	rr = rawJSONRequest(srv, "PATCH", "/api/v1/workspaces/"+ws+"/items/"+diff.Slug,
		`{"fields":{"status":"done","github_pr":{"number":9007199254740992,`+pr+`}}}`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("CHANGING github_pr.number to an integer that shares its float64 was accepted as a carry: %d %s", rr.Code, rr.Body.String())
	}
}

// Sorting on a number field orders by value, so equal values in different
// spellings sit together (SQLite; json_extract yields a number).
func TestFieldSortOrdersNumbersByValue(t *testing.T) {
	srv, ws, mk := bug3202CompareFixture(t)
	mk("a", `{"status":"open","n":1001}`)
	mk("b", `{"status":"open","n":1e3}`)
	mk("c", `{"status":"open","n":999}`)
	mk("d", `{"status":"open","n":1000.0}`)
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/collections/nums/items?sort=n:asc", nil)
	var items []models.Item
	parseJSON(t, rr, &items)
	var order []string
	for _, it := range items {
		order = append(order, it.Title)
	}
	got := strings.Join(order, "")
	if got != "cbda" && got != "cdba" {
		t.Errorf("sort by n: got order %q, want 999, then the two spellings of 1000, then 1001", got)
	}
}
