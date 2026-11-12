package store

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestSearchMatchRelationHasNoSnippets(t *testing.T) {
	for _, dialect := range []Dialect{&sqliteDialect{}, &postgresDialect{}} {
		t.Run(string(dialect.Driver()), func(t *testing.T) {
			s := &Store{dialect: dialect}
			query, _ := s.searchMatches(SearchParams{Query: "nebula", Unrestricted: true}, []string{"direct-hit"})
			for _, expression := range []string{"snippet(", "ts_headline("} {
				if strings.Contains(query, expression) {
					t.Errorf("count/facet match relation contains %s", expression)
				}
			}
		})
	}
}

// pageOnlySnippetDialect makes a snippet expression fail if it is evaluated
// for any item or comment outside the expected page. The real snippet still
// runs for allowed rows, including SQLite FTS auxiliary-function checks.
type pageOnlySnippetDialect struct {
	Dialect
	itemIDs, commentIDs []string
}

func (d *pageOnlySnippetDialect) FTSSnippet(table string, col int, source, query string) (string, []interface{}) {
	expression, args := d.Dialect.FTSSnippet(table, col, source, query)
	column, ids := "i.id", d.itemIDs
	if source == "cm.body" {
		column, ids = "cm.id", d.commentIDs
	}
	condition := "1 = 0"
	var guardArgs []interface{}
	if len(ids) > 0 {
		condition = column + " IN (" + placeholders(len(ids)) + ")"
		for _, id := range ids {
			guardArgs = append(guardArgs, id)
		}
	}
	// Include the row id so PostgreSQL cannot constant-fold the error branch.
	failure := "json('snippet outside page: ' || " + column + ")"
	if d.Driver() == DriverPostgres {
		failure = "CAST(CAST('snippet outside page: ' || " + column + " AS INTEGER) AS TEXT)"
	}
	return "CASE WHEN " + condition + " THEN " + expression + " ELSE " + failure + " END", append(guardArgs, args...)
}

func TestSearchSnippetsOnlyForPage(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Snippet pages")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	type expectedHit struct {
		id, source, commentID, context string
	}
	var expected []expectedHit
	for n, source := range []string{"content", "title", "tags", "content", "title", "tags"} {
		input := models.ItemCreate{Title: fmt.Sprintf("Item %02d", n), Content: "Plain body", Fields: `{"status":"open"}`}
		switch source {
		case "content":
			input.Content = "nebulaquartz bodycontext"
		case "title":
			input.Title += " nebulaquartz titlecontext"
		case "tags":
			input.Tags = `["nebulaquartz", "tagcontext"]`
		}
		item, err := s.CreateItem(ws.ID, col.ID, input)
		if err != nil {
			t.Fatal(err)
		}
		addSearchComment(t, s, item, "nebulaquartz deduplicatedcomment")
		expected = append(expected, expectedHit{item.ID, source, "", ""})
	}
	for n := range 4 {
		item := createTestItem(t, s, ws.ID, col.ID, fmt.Sprintf("Comment %02d", n), "Plain body")
		body := fmt.Sprintf("nebulaquartz selectedcontext%d", n)
		first := addSearchComment(t, s, item, body)
		second := addSearchComment(t, s, item, body)
		ids := []string{first.ID, second.ID}
		sort.Strings(ids)
		deleted := addSearchComment(t, s, item, "nebulaquartz nebulaquartz nebulaquartz deletedcontext")
		if _, err := s.db.Exec(s.q("UPDATE comments SET deleted_at = ? WHERE id = ?"), now(), deleted.ID); err != nil {
			t.Fatal(err)
		}
		expected = append(expected, expectedHit{item.ID, "comment", ids[0], fmt.Sprintf("selectedcontext%d", n)})
	}
	guard := &pageOnlySnippetDialect{Dialect: s.dialect}
	s.dialect = guard
	for _, page := range []struct {
		name          string
		limit, offset int
	}{
		{"item content and title", 2, 0},
		{"item tags and content", 2, 2},
		{"leg boundary", 2, 5},
		{"selected comments", 2, 7},
		{"last comment", 2, 9},
		{"past end", 2, 10},
	} {
		t.Run(page.name, func(t *testing.T) {
			end := min(page.offset+page.limit, len(expected))
			want := expected[page.offset:end]
			guard.itemIDs, guard.commentIDs = nil, nil
			for _, hit := range want {
				if hit.source == "comment" {
					guard.commentIDs = append(guard.commentIDs, hit.commentID)
				} else {
					guard.itemIDs = append(guard.itemIDs, hit.id)
				}
			}
			res, err := s.Search(SearchParams{Query: "nebula", Workspace: ws.Slug, Unrestricted: true, Sort: "title", Limit: page.limit, Offset: page.offset})
			if err != nil {
				t.Fatal(err)
			}
			if res.Total != len(expected) || len(res.Results) != len(want) {
				t.Fatalf("total/rows = %d/%d, want %d/%d", res.Total, len(res.Results), len(expected), len(want))
			}
			if res.Facets == nil || !reflect.DeepEqual(res.Facets.Collections, map[string]int{col.Slug: len(expected)}) || !reflect.DeepEqual(res.Facets.Statuses, map[string]int{"open": len(expected)}) {
				t.Errorf("facets = %+v", res.Facets)
			}
			for n, hit := range res.Results {
				if hit.Item.ID != want[n].id {
					t.Errorf("row %d item = %s, want %s", n, hit.Item.ID, want[n].id)
				}
				assertSearchSource(t, hit, want[n].source, want[n].commentID)
				if !strings.Contains(hit.Snippet, "<mark>") || !strings.Contains(hit.Snippet, want[n].context) {
					t.Errorf("snippet = %q, want highlight and %q", hit.Snippet, want[n].context)
				}
			}
		})
	}
}
