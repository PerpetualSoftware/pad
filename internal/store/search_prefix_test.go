package store

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestSearchPrefix(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Prefix search")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	for _, title := range []string{"polishing widget", "polish", "studies", "thesis", "task forest", "alpha beta", "monkey"} {
		createTestItem(t, s, ws.ID, col.ID, title, title)
		createTestDoc(t, s, ws.ID, title, title)
	}
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"polis", []string{"polish", "polishing widget"}},
		{"polish", []string{"polish", "polishing widget"}},
		{"studie", []string{"studies"}},
		{"polis wid", []string{}},
		{"polishing wid", []string{"polishing widget"}},
		{"polishing w", []string{}},
		{"polishing w:", []string{}},
		{"polishing:wi", []string{"polishing widget"}},
		{"a", []string{}},
		{"a:", []string{}},
		{"task-f", []string{}},
		{"the", []string{"thesis"}},
		{"The", []string{"thesis"}},
		{"THE", []string{"thesis"}},
		{"Polis", []string{"polish", "polishing widget"}},
		{"m²", []string{}},
		{"mo", []string{"monkey"}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			want := tc.want
			t.Run("Search", func(t *testing.T) {
				res, err := s.Search(SearchParams{Query: tc.query, WorkspaceIDs: []string{ws.ID}, Unrestricted: true})
				if err != nil {
					t.Fatal(err)
				}
				got := []string{}
				for _, hit := range res.Results {
					got = append(got, hit.Item.Title)
					if tc.query == "polis" && !strings.Contains(hit.Snippet, "<mark>"+strings.Fields(hit.Item.Title)[0]+"</mark>") {
						t.Errorf("prefix snippet = %q", hit.Snippet)
					}
				}
				sort.Strings(got)
				if !reflect.DeepEqual(got, want) || res.Total != len(want) {
					t.Errorf("rows = %v, total = %d; want %v", got, res.Total, want)
				}
				if tc.query == "polis" {
					if res.Facets == nil || res.Facets.Collections[col.Slug] != len(want) || res.Facets.Statuses["open"] != len(want) {
						t.Errorf("prefix facets = %+v", res.Facets)
					}
				}
			})
			t.Run("ListItems", func(t *testing.T) {
				items, err := s.ListItems(ws.ID, models.ItemListParams{Search: tc.query})
				if err != nil {
					t.Fatal(err)
				}
				got := []string{}
				for _, item := range items {
					got = append(got, item.Title)
				}
				sort.Strings(got)
				if !reflect.DeepEqual(got, want) {
					t.Errorf("titles = %v, want %v", got, want)
				}
			})
			t.Run("ListDocuments", func(t *testing.T) {
				docs, err := s.ListDocuments(ws.ID, models.DocumentListParams{Query: tc.query})
				if err != nil {
					t.Fatal(err)
				}
				got := []string{}
				for _, doc := range docs {
					got = append(got, doc.Title)
				}
				sort.Strings(got)
				if !reflect.DeepEqual(got, want) {
					t.Errorf("titles = %v, want %v", got, want)
				}
			})
			t.Run("SearchItems", func(t *testing.T) {
				hits, err := s.SearchItems(ws.ID, tc.query)
				if err != nil {
					t.Fatal(err)
				}
				got := []string{}
				for _, hit := range hits {
					got = append(got, hit.Item.Title)
					if tc.query == "polis" && !strings.Contains(hit.Snippet, "<mark>"+strings.Fields(hit.Item.Title)[0]+"</mark>") {
						t.Errorf("prefix snippet = %q", hit.Snippet)
					}
				}
				sort.Strings(got)
				if !reflect.DeepEqual(got, want) {
					t.Errorf("titles = %v, want %v", got, want)
				}
			})
		})
	}
}

func TestSearchPrefixPunctuation(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Literal search")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	createTestItem(t, s, ws.ID, col.ID, "polishing widget", "polishing widget")
	createTestDoc(t, s, ws.ID, "polishing widget", "polishing widget")
	for _, punctuation := range []string{`"`, `'`, `:`, `&`, `*`, `(`, `)`, `\`, `|`, `!`, `AND`, `OR`, `NOT`, `:*`, `foo')|!bar:*`} {
		for _, query := range []string{punctuation, "polishing " + punctuation, "polishing" + punctuation} {
			t.Run(query, func(t *testing.T) {
				if _, err := s.Search(SearchParams{Query: query, WorkspaceIDs: []string{ws.ID}, Unrestricted: true}); err != nil {
					t.Errorf("Search: %v", err)
				}
				if _, err := s.ListItems(ws.ID, models.ItemListParams{Search: query}); err != nil {
					t.Errorf("ListItems: %v", err)
				}
				if _, err := s.ListDocuments(ws.ID, models.DocumentListParams{Query: query}); err != nil {
					t.Errorf("ListDocuments: %v", err)
				}
				if _, err := s.SearchItems(ws.ID, query); err != nil {
					t.Errorf("SearchItems: %v", err)
				}
			})
		}
	}
}

func TestSearchPrefixDirectLookup(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Direct search")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Unrelated title", "Unrelated body")
	for _, query := range []string{fmt.Sprintf("%s-%d", col.Prefix, *item.ItemNumber), fmt.Sprint(*item.ItemNumber)} {
		t.Run(query, func(t *testing.T) {
			res, err := s.Search(SearchParams{Query: query, WorkspaceIDs: []string{ws.ID}, Unrestricted: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Results) != 1 || res.Results[0].Item.ID != item.ID || res.Total != 1 || res.Results[0].Rank != -1000 {
				t.Errorf("direct lookup = %+v", res)
			}
		})
	}
}

func TestSearchPrefixLongWord(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Long word search")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	createTestItem(t, s, ws.ID, col.ID, "polishing widget", "polishing widget")
	createTestDoc(t, s, ws.ID, "polishing widget", "polishing widget")
	for _, query := range []string{
		strings.Repeat("a", maxFTSPrefixBytes-1),
		strings.Repeat("a", maxFTSPrefixBytes),
		strings.Repeat("a", 3000),
		"polishing " + strings.Repeat("界", 700),
	} {
		t.Run(fmt.Sprintf("%d bytes", len(query)), func(t *testing.T) {
			if _, err := s.Search(SearchParams{Query: query, WorkspaceIDs: []string{ws.ID}, Unrestricted: true}); err != nil {
				t.Errorf("Search: %v", err)
			}
			if _, err := s.ListItems(ws.ID, models.ItemListParams{Search: query}); err != nil {
				t.Errorf("ListItems: %v", err)
			}
			if _, err := s.ListDocuments(ws.ID, models.DocumentListParams{Query: query}); err != nil {
				t.Errorf("ListDocuments: %v", err)
			}
			if _, err := s.SearchItems(ws.ID, query); err != nil {
				t.Errorf("SearchItems: %v", err)
			}
		})
	}
	if _, part := splitFTSPrefix(strings.Repeat("a", maxFTSPrefixBytes-1)); part == "" {
		t.Error("a part one byte under the limit lost its prefix")
	}
	if _, part := splitFTSPrefix(strings.Repeat("a", maxFTSPrefixBytes)); part != "" {
		t.Error("a part at the limit kept its prefix")
	}
}

func TestSearchPrefixDirectLookupTwoDigits(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Direct search two digits")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	var item models.Item
	for i := 0; i < 12; i++ {
		item = *createTestItem(t, s, ws.ID, col.ID, fmt.Sprintf("Filler %d", i), "body")
	}
	if *item.ItemNumber < 10 {
		t.Fatalf("item number %d has one digit", *item.ItemNumber)
	}
	for _, query := range []string{fmt.Sprintf("%s-%d", col.Prefix, *item.ItemNumber), fmt.Sprint(*item.ItemNumber)} {
		t.Run(query, func(t *testing.T) {
			res, err := s.Search(SearchParams{Query: query, WorkspaceIDs: []string{ws.ID}, Unrestricted: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Results) == 0 || res.Results[0].Item.ID != item.ID || res.Results[0].Rank != -1000 {
				t.Errorf("direct lookup = %+v", res)
			}
		})
	}
}

func TestPostgresFTSQuery(t *testing.T) {
	for _, tc := range []struct {
		query string
		args  []interface{}
	}{
		{"a", []interface{}{"a", "a"}},
		{"a:", []interface{}{"a:", "a:"}},
		{"a:b", []interface{}{"a:b", "a:b"}},
		{"task-f", []interface{}{"task-f", "task f"}},
		{"polishing w", []interface{}{"polishing w", "polishing w"}},
		{"polishing &", []interface{}{"polishing &", "polishing &"}},
		{"界", []interface{}{"界", "界"}},
		{"polis", []interface{}{"polis", "polis", "", "polis", "polis"}},
		{"the", []interface{}{"the", "the", "", "the", "the"}},
		{"12", []interface{}{"12", "12", "", "12", "12"}},
		{"a²", []interface{}{"a²", "a²"}},
		{"a٣", []interface{}{"a٣", "a٣", "", "a٣", "a٣"}},
		{" earlier\tfoo-bar:wi: ", []interface{}{" earlier\tfoo-bar:wi: ", " earlier\tfoo bar:wi: ", "earlier foo bar", "wi", "wi"}},
		{`foo')|!bar:*`, []interface{}{`foo')|!bar:*`, `foo')|!bar:*`, "foo", "bar", "bar"}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			sql, args := postgresFTSQuery(tc.query)
			if !reflect.DeepEqual(args, tc.args) {
				t.Fatalf("args = %#v, want %#v", args, tc.args)
			}
			if strings.Count(sql, "?") != len(args) {
				t.Errorf("SQL = %q has %d arguments", sql, len(args))
			}
			wholeWords := "plainto_tsquery('english', ?) || plainto_tsquery('english', ?)"
			if len(args) == 2 && sql != wholeWords {
				t.Errorf("whole-word SQL = %q, want %q", sql, wholeWords)
			}
			if len(args) > 2 && !strings.HasPrefix(sql, wholeWords+" || ") {
				t.Errorf("prefix SQL lost whole-word legs: %q", sql)
			}
		})
	}
}
