package store

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4514: matched_in and the snippet choice come from per-column match
// flags computed in SQL, not from "<mark>" appearing in the snippet text, so
// stored text containing a literal <mark> cannot fake a match. A tags match
// reads as a plain list, not as the stored JSON array.

func searchOne(t *testing.T, s *Store, ws *models.Workspace, itemID, query string) SearchResult {
	t.Helper()
	res, err := s.Search(SearchParams{Query: query, Workspace: ws.Slug, ItemIDs: []string{itemID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 1 {
		t.Fatalf("got %d results, want 1: %+v", len(res.Results), res)
	}
	return res.Results[0]
}

func TestSearchLiteralMarkInBodyTitleMatch(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Literal mark title")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:   "nebulaquartz title context",
		Content: "Copied from a page: <mark>pasted</mark> highlight text",
	})
	if err != nil {
		t.Fatal(err)
	}
	// A body with unflushed edits: a content snippet would carry this marker,
	// a title snippet must not.
	if _, err := s.AppendYjsUpdate(item.ID, []byte{7}, "1"); err != nil {
		t.Fatal(err)
	}
	hit := searchOne(t, s, ws, item.ID, "nebulaquartz")
	if hit.MatchedIn != "title" {
		t.Errorf("matched_in = %q, want title", hit.MatchedIn)
	}
	if !strings.Contains(hit.Snippet, "nebulaquartz") || strings.Contains(hit.Snippet, "pasted") {
		t.Errorf("snippet = %q, want the title", hit.Snippet)
	}
	if hit.Item.ContentState != "" {
		t.Errorf("a title match carries the body's content_state %q", hit.Item.ContentState)
	}
}

func TestSearchLiteralMarkInBodyTagsMatch(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Literal mark tags")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:   "Ordinary",
		Content: "Copied from a page: <mark>pasted</mark> highlight text",
		Tags:    `["nebulaquartz"]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	hit := searchOne(t, s, ws, item.ID, "nebulaquartz")
	if hit.MatchedIn != "tags" {
		t.Errorf("matched_in = %q, want tags", hit.MatchedIn)
	}
	if !strings.Contains(hit.Snippet, "nebulaquartz") {
		t.Errorf("snippet = %q, want the tags", hit.Snippet)
	}
}

func TestSearchLiteralMarkInBodyContentMatch(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Literal mark content")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:   "nebulaquartz title context",
		Content: "The nebulaquartz body, with <mark>pasted</mark> highlight text",
	})
	if err != nil {
		t.Fatal(err)
	}
	hit := searchOne(t, s, ws, item.ID, "nebulaquartz")
	if hit.MatchedIn != "content" {
		t.Errorf("matched_in = %q, want content", hit.MatchedIn)
	}
	if !strings.Contains(hit.Snippet, "body") {
		t.Errorf("snippet = %q, want the body", hit.Snippet)
	}
}

func TestSearchLiteralMarkInEntryHead(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Literal mark head")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Plain title", "Unrelated body")
	appendTestNote(t, s, item.ID, "Pasted <mark>headline</mark> words", "nebulaquartz detail words")
	hit := searchOne(t, s, ws, item.ID, "nebulaquartz")
	if hit.MatchedIn != "note" {
		t.Errorf("matched_in = %q, want note", hit.MatchedIn)
	}
	if !strings.Contains(hit.Snippet, "detail words") || strings.Contains(hit.Snippet, "headline") {
		t.Errorf("snippet = %q, want the body", hit.Snippet)
	}
}

func TestSearchTagsSnippetIsPlainList(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Tags snippet")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:   "Ordinary",
		Content: "Plain body",
		Tags:    `["nebulaquartz","other"]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	hit := searchOne(t, s, ws, item.ID, "nebulaquartz")
	if hit.MatchedIn != "tags" {
		t.Errorf("matched_in = %q, want tags", hit.MatchedIn)
	}
	if want := "<mark>nebulaquartz</mark>, other"; hit.Snippet != want {
		t.Errorf("snippet = %q, want %q", hit.Snippet, want)
	}
}

func TestPlainTagsSnippet(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"empty", "", ""},
		{"empty array", "[]", ""},
		{"single", `["<mark>x</mark>"]`, "<mark>x</mark>"},
		{"sqlite separator", `["<mark>x</mark>","y"]`, "<mark>x</mark>, y"},
		{"postgres separator", `["<mark>x</mark>", "y", "z"]`, "<mark>x</mark>, y, z"},
		{"cut at the start", `...<mark>x</mark>","y"]`, "...<mark>x</mark>, y"},
		{"cut at the end", `["a","<mark>x</mark>","y...`, "a, <mark>x</mark>, y..."},
		{"cut at both ends", `...b", "<mark>x</mark>", "c...`, "...b, <mark>x</mark>, c..."},
		{"escaped quote", `["say \"<mark>x</mark>\"","y"]`, `say "<mark>x</mark>", y`},
		{"escaped backslash", `["a\\b","<mark>x</mark>"]`, `a\b, <mark>x</mark>`},
		{"escaped backslash before quote", `["a\\","<mark>x</mark>"]`, `a\, <mark>x</mark>`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := plainTagsSnippet(c.in); got != c.want {
				t.Errorf("plainTagsSnippet(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
