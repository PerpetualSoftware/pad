package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3686: an item list with a search term (listItemsFTS) ignored
// include_archived and sort. (Its dropped offset was BUG-3704's.)
// BUG-3706: the tag filter matched by LIKE on SQLite (so % and _ were
// wildcards and case was ignored) and built its JSON by hand on Postgres.
// Property: a list returns exactly the items its filters describe, with or
// without a search term, on both dialects (make test-pg runs these on PG).

func listTitles(t *testing.T, s *Store, wsID string, p models.ItemListParams) []string {
	t.Helper()
	items, err := s.ListItems(wsID, p)
	if err != nil {
		t.Fatalf("ListItems(%+v): %v", p, err)
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Title)
	}
	return out
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, x := range a {
		seen[x]++
	}
	for _, x := range b {
		seen[x]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

func TestBUG3686_SearchHonoursIncludeArchived(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "BUG-3686 archived")
	col := createTestCollection(t, s, ws.ID, "Notes")
	live, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "Zephyrine live note"})
	if err != nil {
		t.Fatal(err)
	}
	gone, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "Zephyrine archived note"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteItem(gone.ID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	_ = live

	// Premise: the plain list honours the flag.
	if got := listTitles(t, s, ws.ID, models.ItemListParams{IncludeArchived: true}); len(got) != 2 {
		t.Fatalf("premise: plain list with include_archived: %v, want both", got)
	}
	if got := listTitles(t, s, ws.ID, models.ItemListParams{Search: "zephyrine"}); !sameSet(got, []string{"Zephyrine live note"}) {
		t.Errorf("search without include_archived: %v, want only the live note", got)
	}
	if got := listTitles(t, s, ws.ID, models.ItemListParams{Search: "zephyrine", IncludeArchived: true}); !sameSet(got, []string{"Zephyrine live note", "Zephyrine archived note"}) {
		t.Errorf("search with include_archived: %v, want both notes", got)
	}
}

func TestBUG3686_SearchHonoursSort(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "BUG-3686 sort")
	col := createTestCollection(t, s, ws.ID, "Notes")
	// Titles whose relevance ties or runs against the alphabet, so a list
	// that ignores sort is visibly out of order.
	for _, title := range []string{"Quillback charlie", "Quillback alpha quillback", "Quillback bravo"} {
		if _, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: title}); err != nil {
			t.Fatal(err)
		}
	}
	asc := listTitles(t, s, ws.ID, models.ItemListParams{Search: "quillback", Sort: "title:asc"})
	want := []string{"Quillback alpha quillback", "Quillback bravo", "Quillback charlie"}
	if len(asc) != 3 || asc[0] != want[0] || asc[1] != want[1] || asc[2] != want[2] {
		t.Errorf("search sorted title:asc: %v, want %v", asc, want)
	}
	desc := listTitles(t, s, ws.ID, models.ItemListParams{Search: "quillback", Sort: "title:desc"})
	if len(desc) != 3 || desc[0] != want[2] || desc[2] != want[0] {
		t.Errorf("search sorted title:desc: %v, want the reverse of %v", desc, want)
	}
	// No sort: relevance still decides, and every match comes back.
	if got := listTitles(t, s, ws.ID, models.ItemListParams{Search: "quillback"}); !sameSet(got, want) {
		t.Errorf("search with no sort: %v, want all three", got)
	}
}

func TestBUG3706_TagFilterIsExact(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "BUG-3706 tags")
	col := createTestCollection(t, s, ws.ID, "Notes")
	for _, it := range []struct{ title, tags string }{
		{"Marrowind underscore", `["team_one"]`},
		{"Marrowind wildcard bait", `["teamXone"]`},
		{"Marrowind upper", `["UX"]`},
		{"Marrowind quote", `["a\"b"]`},
		{"Marrowind pair", `["a","b"]`},
		{"Marrowind untagged", `[]`},
	} {
		if _, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: it.title, Tags: it.tags}); err != nil {
			t.Fatalf("CreateItem %s: %v", it.title, err)
		}
	}
	cases := []struct {
		tag  string
		want []string
	}{
		{"team_one", []string{"Marrowind underscore"}},
		{"%", nil},
		{"_", nil},
		{"UX", []string{"Marrowind upper"}},
		{"ux", nil},
		{`a"b`, []string{"Marrowind quote"}},
		{`a","b`, nil},
		{"a", []string{"Marrowind pair"}},
	}
	for _, search := range []string{"", "marrowind"} {
		for _, c := range cases {
			got := listTitles(t, s, ws.ID, models.ItemListParams{Tag: c.tag, Search: search})
			if !sameSet(got, c.want) {
				t.Errorf("tag %q (search %q): %v, want %v", c.tag, search, got, c.want)
			}
		}
	}
}
