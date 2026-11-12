package store

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func addSearchComment(t *testing.T, s *Store, item *models.Item, body string) *models.Comment {
	t.Helper()
	c, err := s.CreateComment(item.WorkspaceID, item.ID, "", models.CommentCreate{Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func assertSearchSource(t *testing.T, r SearchResult, field, commentID string) {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		MatchedIn string `json:"matched_in"`
		CommentID string `json:"comment_id"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.MatchedIn != field || wire.CommentID != commentID {
		t.Errorf("source = %+v, want %s / %s", wire, field, commentID)
	}
}

func TestSearchCommentOnly(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Comment search")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Plain title", "Unrelated body")
	comment := addSearchComment(t, s, item, "The reason is nebulaquartz and this is comment context.")
	res, err := s.Search(SearchParams{Query: "nebulaquartz", Workspace: ws.Slug, Unrestricted: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 || len(res.Results) != 1 {
		t.Fatalf("got %d rows, total %d; want 1", len(res.Results), res.Total)
	}
	hit := res.Results[0]
	if hit.Item.ID != item.ID || !strings.Contains(hit.Snippet, "nebulaquartz") || !strings.Contains(hit.Snippet, "comment context") {
		t.Errorf("wrong comment hit: %+v", hit)
	}
	assertSearchSource(t, hit, "comment", comment.ID)
	if res.Facets == nil || res.Facets.Collections[col.Slug] != 1 || res.Facets.Statuses["open"] != 1 {
		t.Errorf("facets = %+v", res.Facets)
	}
}

func TestSearchCommentDedupAndPagination(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Comment pages")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	body := createTestItem(t, s, ws.ID, col.ID, "Body hit", "nebulaquartz body context")
	addSearchComment(t, s, body, "nebulaquartz comment context")
	only := createTestItem(t, s, ws.ID, col.ID, "Comment hit", "Unrelated")
	addSearchComment(t, s, only, "nebulaquartz nebulaquartz nebulaquartz")
	addSearchComment(t, s, only, "nebulaquartz second matching comment")
	third := createTestItem(t, s, ws.ID, col.ID, "Other comment hit", "Unrelated")
	addSearchComment(t, s, third, "nebulaquartz")
	p := SearchParams{Query: "nebulaquartz", WorkspaceIDs: []string{ws.ID}, Unrestricted: true}
	all, err := s.Search(p)
	if err != nil {
		t.Fatal(err)
	}
	if all.Total != 3 || len(all.Results) != 3 {
		t.Fatalf("got %d rows, total %d; want 3", len(all.Results), all.Total)
	}
	if all.Results[0].Item.ID != body.ID {
		t.Errorf("item-field match must come first: %+v", all.Results)
	}
	assertSearchSource(t, all.Results[0], "content", "")
	if !strings.Contains(all.Results[0].Snippet, "body context") {
		t.Errorf("snippet = %q", all.Results[0].Snippet)
	}
	if all.Facets == nil || all.Facets.Collections[col.Slug] != 3 || all.Facets.Statuses["open"] != 3 {
		t.Errorf("facets = %+v", all.Facets)
	}
	ids := []string{}
	for offset := 0; offset <= 3; offset++ {
		p.Limit, p.Offset = 1, offset
		page, err := s.Search(p)
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != 3 || !reflect.DeepEqual(page.Facets, all.Facets) {
			t.Errorf("offset %d: totals/facets disagree: %+v", offset, page)
		}
		if offset == 3 {
			if len(page.Results) != 0 {
				t.Errorf("past-end page: %+v", page)
			}
			continue
		}
		if len(page.Results) != 1 {
			t.Fatalf("offset %d has %d rows", offset, len(page.Results))
		}
		ids = append(ids, page.Results[0].Item.ID)
		if page.Results[0].Item.ID != all.Results[offset].Item.ID {
			t.Errorf("offset %d disagrees", offset)
		}
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Errorf("duplicate %s", id)
		}
		seen[id] = true
	}
	p.Limit, p.Offset = 2, 0
	page, err := s.Search(p)
	if err != nil || len(page.Results) != 2 || page.Results[1].Item.ID != all.Results[1].Item.ID {
		t.Fatalf("boundary page: %+v, %v", page, err)
	}
}

func TestSearchCommentVisibility(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Comment visibility")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	visible := createTestItem(t, s, ws.ID, col.ID, "Visible", "Plain")
	addSearchComment(t, s, visible, "nebulaquartz")
	hiddenCol := createTestCollection(t, s, ws.ID, "Private")
	hidden := createTestItem(t, s, ws.ID, hiddenCol.ID, "Hidden", "Plain")
	addSearchComment(t, s, hidden, "nebulaquartz")
	deleted := createTestItem(t, s, ws.ID, col.ID, "Deleted", "Plain")
	addSearchComment(t, s, deleted, "nebulaquartz")
	if err := s.DeleteItem(deleted.ID); err != nil {
		t.Fatal(err)
	}
	deadCol := createTestCollection(t, s, ws.ID, "Archived")
	archived := createTestItem(t, s, ws.ID, deadCol.ID, "Archived collection", "Plain")
	addSearchComment(t, s, archived, "nebulaquartz")
	if err := s.DeleteCollection(deadCol.ID, ""); err != nil {
		t.Fatal(err)
	}
	tombstone := createTestItem(t, s, ws.ID, col.ID, "Tombstone", "Plain")
	c := addSearchComment(t, s, tombstone, "nebulaquartz")
	// Keep the indexed body to prove the query checks the tombstone too.
	if _, err := s.db.Exec(s.q("UPDATE comments SET deleted_at = ? WHERE id = ?"), now(), c.ID); err != nil {
		t.Fatal(err)
	}
	otherWS := createTestWorkspace(t, s, "Other workspace")
	otherCol := createTestCollection(t, s, otherWS.ID, "Tasks")
	other := createTestItem(t, s, otherWS.ID, otherCol.ID, "Other workspace", "Plain")
	addSearchComment(t, s, other, "nebulaquartz")
	for name, p := range map[string]SearchParams{
		"collection grant":           {WorkspaceIDs: []string{ws.ID}, CollectionIDs: []string{col.ID, deadCol.ID}},
		"item grant":                 {Workspace: ws.Slug, ItemIDs: []string{visible.ID}},
		"collection filter":          {Workspace: ws.Slug, Unrestricted: true, Collection: col.Slug},
		"resolved collection filter": {Workspace: ws.Slug, Unrestricted: true, CollectionFilterIDs: []string{col.ID}},
		"field filter":               {Workspace: ws.Slug, CollectionIDs: []string{col.ID}, FieldFilters: map[string]string{"status": "open"}},
	} {
		t.Run(name, func(t *testing.T) {
			p.Query = "nebulaquartz"
			res, err := s.Search(p)
			if err != nil {
				t.Fatal(err)
			}
			if res.Total != 1 || len(res.Results) != 1 || res.Results[0].Item.ID != visible.ID {
				t.Fatalf("visibility: %+v", res)
			}
			if res.Facets == nil || !reflect.DeepEqual(res.Facets.Collections, map[string]int{col.Slug: 1}) || !reflect.DeepEqual(res.Facets.Statuses, map[string]int{"open": 1}) {
				t.Errorf("facets: %+v", res.Facets)
			}
		})
	}
	for name, p := range map[string]SearchParams{
		"no permission":     {Workspace: ws.Slug},
		"empty collections": {Workspace: ws.Slug, Unrestricted: true, CollectionFilterIDs: []string{}},
		"wrong field":       {Workspace: ws.Slug, Unrestricted: true, FieldFilters: map[string]string{"status": "done"}},
	} {
		t.Run(name, func(t *testing.T) {
			p.Query = "nebulaquartz"
			res, err := s.Search(p)
			if err != nil {
				t.Fatal(err)
			}
			if res.Total != 0 || len(res.Results) != 0 {
				t.Errorf("want none: %+v", res)
			}
			if res.Facets != nil {
				for _, n := range res.Facets.Collections {
					if n != 0 {
						t.Errorf("facet leak: %+v", res.Facets)
					}
				}
			}
		})
	}
}

func TestSearchMatchedInFieldsAndRef(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Search fields")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	for _, field := range []string{"title", "content", "tags"} {
		t.Run(field, func(t *testing.T) {
			input := models.ItemCreate{Title: "Ordinary", Content: "Plain body"}
			switch field {
			case "title":
				input.Title = "nebulaquartz title context"
			case "content":
				input.Content = "nebulaquartz body context"
			case "tags":
				input.Tags = `["nebulaquartz"]`
			}
			item, err := s.CreateItem(ws.ID, col.ID, input)
			if err != nil {
				t.Fatal(err)
			}
			res, err := s.Search(SearchParams{Query: "nebulaquartz", Workspace: ws.Slug, ItemIDs: []string{item.ID}})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Results) != 1 {
				t.Fatalf("hits: %+v", res)
			}
			assertSearchSource(t, res.Results[0], field, "")
			if !strings.Contains(res.Results[0].Snippet, "nebulaquartz") {
				t.Errorf("snippet: %q", res.Results[0].Snippet)
			}
			for _, q := range []string{item.Ref, fmt.Sprint(*item.ItemNumber)} {
				if q == "" {
					continue
				}
				direct, err := s.Search(SearchParams{Query: q, Workspace: ws.Slug, ItemIDs: []string{item.ID}})
				if err != nil {
					t.Fatal(err)
				}
				if len(direct.Results) != 1 {
					t.Fatalf("direct: %+v", direct)
				}
				assertSearchSource(t, direct.Results[0], "ref", "")
				if direct.Facets == nil || direct.Facets.Collections[col.Slug] != 1 {
					t.Errorf("direct facets: %+v", direct.Facets)
				}
			}
		})
	}
}
