package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestListItems_NonTerminalFilter is the BUG-2001 acceptance test: the
// default (no --status / no --all) `pad item list` must resolve "open"
// per-collection from each schema's terminal_options, so collections with
// CUSTOM status vocabularies (todo / drafting / scheduled) show their open
// items instead of being hidden behind a hardcoded global allowlist.
func TestListItems_NonTerminalFilter(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "NonTerminalFilter")

	// A collection with a CUSTOM status vocabulary that the old hardcoded
	// allowlist did NOT cover: `todo`, `drafting`, `scheduled` are open;
	// only `published` is terminal.
	blog, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Blog",
		Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["todo","drafting","scheduled","published"],"terminal_options":["published"],"default":"todo"}]}`,
	})
	if err != nil {
		t.Fatalf("create blog collection: %v", err)
	}

	// A second collection with the built-in `done` terminal so we confirm
	// the per-collection resolution composes across collections.
	tasks, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Tasks",
		Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["todo","in-progress","done"],"terminal_options":["done"],"default":"todo"}]}`,
	})
	if err != nil {
		t.Fatalf("create tasks collection: %v", err)
	}

	seed := []struct {
		coll   string
		id     string
		fields string
	}{
		{"blog", "blog-todo", `{"status":"todo"}`},
		{"blog", "blog-drafting", `{"status":"drafting"}`},
		{"blog", "blog-scheduled", `{"status":"scheduled"}`},
		{"blog", "blog-published", `{"status":"published"}`}, // terminal
		{"tasks", "task-todo", `{"status":"todo"}`},
		{"tasks", "task-done", `{"status":"done"}`}, // terminal
	}
	collID := map[string]string{"blog": blog.ID, "tasks": tasks.ID}
	for _, sd := range seed {
		if _, err := s.CreateItem(ws.ID, collID[sd.coll], models.ItemCreate{
			Title:  sd.id,
			Fields: sd.fields,
		}); err != nil {
			t.Fatalf("create item %s: %v", sd.id, err)
		}
	}

	titles := func(items []models.Item) map[string]bool {
		m := make(map[string]bool, len(items))
		for _, it := range items {
			m[it.Title] = true
		}
		return m
	}

	// Default (NonTerminal): every non-terminal item across BOTH collections
	// appears; the two terminal items (blog-published, task-done) are hidden.
	got, err := s.ListItems(ws.ID, models.ItemListParams{NonTerminal: true})
	if err != nil {
		t.Fatalf("ListItems non-terminal: %v", err)
	}
	tset := titles(got)
	wantPresent := []string{"blog-todo", "blog-drafting", "blog-scheduled", "task-todo"}
	for _, w := range wantPresent {
		if !tset[w] {
			t.Errorf("non-terminal list missing open item %q (got %v)", w, tset)
		}
	}
	wantHidden := []string{"blog-published", "task-done"}
	for _, w := range wantHidden {
		if tset[w] {
			t.Errorf("non-terminal list wrongly includes terminal item %q", w)
		}
	}
	if len(got) != 4 {
		t.Errorf("expected 4 non-terminal items, got %d (%v)", len(got), tset)
	}

	// --all (NonTerminal false): every item, including terminals.
	all, err := s.ListItems(ws.ID, models.ItemListParams{})
	if err != nil {
		t.Fatalf("ListItems all: %v", err)
	}
	if len(all) != 6 {
		t.Errorf("expected 6 items with no filter (--all), got %d", len(all))
	}

	// Explicit status filter still works: only the terminal blog item.
	explicit, err := s.ListItems(ws.ID, models.ItemListParams{
		CollectionSlug: "blog",
		Fields:         map[string]string{"status": "published"},
	})
	if err != nil {
		t.Fatalf("ListItems explicit status: %v", err)
	}
	if len(explicit) != 1 || explicit[0].Title != "blog-published" {
		t.Errorf("explicit status=published expected [blog-published], got %v", titles(explicit))
	}
}
