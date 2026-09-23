package server

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3167: the store now matches field filters EXACTLY, and the query-string
// parse is the one place a comma becomes an OR. These legs pin both halves at
// the HTTP door.

// The documented external contract is unchanged: `?status=open,done` (what
// `pad item list --status open,done` sends) returns items in either status.
func TestBUG3167_QueryCommaIsStillAnOr(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug := createWSWithCollections(t, srv)
		open := createItem(t, srv, slug, "tasks", map[string]any{"title": "Open one", "fields": `{"status":"open"}`})
		done := createItem(t, srv, slug, "tasks", map[string]any{"title": "Done one", "fields": `{"status":"done"}`})
		createItem(t, srv, slug, "tasks", map[string]any{"title": "Elsewhere", "fields": `{"status":"in-progress"}`})

		rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/collections/tasks/items?status=open,done", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
		}
		var items []models.Item
		parseJSON(t, rr, &items)
		var got []string
		for _, it := range items {
			got = append(got, it.Ref)
		}
		sort.Strings(got)
		want := []string{done.Ref, open.Ref}
		sort.Strings(want)
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("?status=open,done = %v, want %v", got, want)
		}
	})
}

// checkUniqueFields asks the store whether a value is taken. With the split,
// a comma value "a,b" was looked up as IN ('a','b'): an item holding "a" made
// it a false conflict, and a real duplicate "a,b" was never found by that
// lookup. The lookup is exact now.
func TestBUG3167_UniqueCheckTreatsACommaValueAsOneValue(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug := createWSWithCollections(t, srv)
		if rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]any{
			"name": "Codes", "slug": "codes", "prefix": "CODE",
			"schema": `{"fields":[{"key":"code","type":"text","unique_scope":"workspace_collection"}]}`,
		}); rr.Code != http.StatusCreated {
			t.Fatalf("create collection: %d %s", rr.Code, rr.Body.String())
		}
		createItem(t, srv, slug, "codes", map[string]any{"title": "Holds a", "fields": `{"code":"a"}`})

		rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/codes/items", map[string]any{"title": "Holds a,b", "fields": `{"code":"a,b"}`})
		if rr.Code != http.StatusCreated {
			t.Fatalf("a,b is not taken (only a is): expected 201, got %d %s", rr.Code, rr.Body.String())
		}
		rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/codes/items", map[string]any{"title": "Duplicate a,b", "fields": `{"code":"a,b"}`})
		if rr.Code != http.StatusConflict {
			t.Errorf("a real duplicate a,b must conflict: got %d %s", rr.Code, rr.Body.String())
		}
	})
}

// A parent filter names one item. Before the lowering, `?parent=A,B` reached
// the resolver as the whole string and was refused; it still is, rather than
// turning into an OR over a `parent` field no item stores.
func TestBUG3167_CommaParentFilterIsStillRefused(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug := createWSWithCollections(t, srv)
		a := createItem(t, srv, slug, "plans", map[string]any{"title": "Plan A"})
		b := createItem(t, srv, slug, "plans", map[string]any{"title": "Plan B"})
		rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/collections/tasks/items?parent="+a.Ref+","+b.Ref, nil)
		if rr.Code == http.StatusOK {
			t.Errorf("?parent=%s,%s answered 200 %s; a comma parent must still be refused", a.Ref, b.Ref, rr.Body.String())
		}
	})
}

// The parse is where a comma becomes an OR, so its edges are pinned directly
// (codex round 1 on BUG-3167 named them as uncovered): pieces are trimmed, an
// empty piece survives as the empty string (as the old split did), a value
// without a comma stays verbatim in Fields, a repeated key keeps its FIRST
// value, and known parameters never become field filters.
func TestBUG3167_ParseLowersOnlyCommaValues(t *testing.T) {
	r := httptest.NewRequest("GET", "/x?status=open%2C+done&priority=+high+&stage=a%2C&owner=first&owner=second&sort=title", nil)
	p := parseItemListParams(r)
	if got := p.FieldsAnyOf["status"]; !reflect.DeepEqual(got, []string{"open", "done"}) {
		t.Errorf("status pieces = %q, want trimmed [open done]", got)
	}
	if got := p.FieldsAnyOf["stage"]; !reflect.DeepEqual(got, []string{"a", ""}) {
		t.Errorf("stage pieces = %q, want [a \"\"]", got)
	}
	if got, ok := p.Fields["priority"]; !ok || got != " high " {
		t.Errorf("a value without a comma must stay verbatim in Fields: got %q (present=%v)", got, ok)
	}
	if got := p.Fields["owner"]; got != "first" {
		t.Errorf("repeated key: got %q, want the first value", got)
	}
	if _, ok := p.Fields["status"]; ok {
		t.Errorf("a comma value must not ALSO be left in Fields")
	}
	if _, ok := p.Fields["sort"]; ok {
		t.Errorf("a known parameter became a field filter")
	}
}
