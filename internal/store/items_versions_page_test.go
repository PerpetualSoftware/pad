package store

// BUG-2608 — the claim that summary mode SKIPS reverse-patch resolution is a
// claim about work not done, which a response-shape assertion cannot make: a
// handler that resolved everything and then blanked the fields would look
// identical from outside (codex round 2).
//
// This asserts it where it is observable — the paged raw reader returns rows
// still carrying patch text and is_diff, while the resolved reader returns
// reconstructed content. Summary mode calls the former, and that one-line
// reading is what carries the performance claim.

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestListItemVersionsPage_ReturnsUnresolvedRows(t *testing.T) {
	s := testStore(t)
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Versions WS"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Slug: "tasks", Prefix: "TASK",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	// A body large enough that a patch really is smaller than the whole
	// content — otherwise the store keeps full copies, nothing is a diff, and
	// every assertion below is vacuous.
	base := strings.Repeat("a line of body text that makes the patch worth storing\n", 200)
	item, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "versioned", Content: base + "v0\n"})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	sources := []string{"web", "cli", "mcp", "skill"}
	for i := 1; i <= 4; i++ {
		content := base + "v" + string(rune('0'+i)) + "\n"
		if _, err := s.UpdateItem(item.ID, models.ItemUpdate{
			Content: &content,
			Source:  sources[i%len(sources)],
		}); err != nil {
			t.Fatalf("edit %d: %v", i, err)
		}
	}

	fresh, err := s.GetItem(item.ID)
	if err != nil || fresh == nil {
		t.Fatalf("reload item: %v", err)
	}

	raw, err := s.ListItemVersionsPage(item.ID, 0)
	if err != nil {
		t.Fatalf("ListItemVersionsPage: %v", err)
	}
	resolved, err := s.ListItemVersionsResolvedPage(item.ID, fresh.Content, 0)
	if err != nil {
		t.Fatalf("ListItemVersionsResolvedPage: %v", err)
	}
	if len(raw) != len(resolved) || len(raw) == 0 {
		t.Fatalf("raw=%d resolved=%d rows; need the same non-empty set", len(raw), len(resolved))
	}

	var rawDiffs int
	for _, v := range raw {
		if v.IsDiff {
			rawDiffs++
		}
	}
	if rawDiffs == 0 {
		t.Fatal("fixture never armed: the store recorded no reverse-patch versions, " +
			"so 'raw rows are unresolved' is trivially true of full content too")
	}

	// The resolved reader must have DONE the work the raw one skips.
	for i, v := range resolved {
		if v.IsDiff {
			t.Errorf("resolved version %d still marked is_diff — it was not resolved", i)
		}
	}
	// And the raw reader must NOT have: at least one row differs from its
	// resolved counterpart, which can only be true if no patch was applied.
	var differ int
	for i := range raw {
		if raw[i].ID == resolved[i].ID && raw[i].Content != resolved[i].Content {
			differ++
		}
	}
	if differ == 0 {
		t.Error("every raw row already equalled its resolved content — the paged " +
			"reader is resolving patches, which is the work summary mode exists " +
			"to skip")
	}
}

func TestListItemVersionsPage_LimitTakesTheNewest(t *testing.T) {
	s := testStore(t)
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Versions WS 2"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Slug: "tasks", Prefix: "TASK",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	item, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "versioned", Content: "v0\n"})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	sources := []string{"web", "cli", "mcp", "skill"}
	for i := 1; i <= 6; i++ {
		content := "v" + string(rune('0'+i)) + "\n"
		if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Content: &content, Source: sources[i%len(sources)]}); err != nil {
			t.Fatalf("edit %d: %v", i, err)
		}
	}

	all, err := s.ListItemVersionsPage(item.ID, 0)
	if err != nil {
		t.Fatalf("unbounded: %v", err)
	}
	if len(all) < 4 {
		t.Fatalf("fixture never armed: %d versions", len(all))
	}
	got, err := s.ListItemVersionsPage(item.ID, 2)
	if err != nil {
		t.Fatalf("limited: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("limit=2 returned %d rows", len(got))
	}
	// The NEWEST two — the only window the reverse-patch chain makes cheap.
	if got[0].ID != all[0].ID || got[1].ID != all[1].ID {
		t.Errorf("limit window = [%s %s], want newest [%s %s]",
			got[0].ID, got[1].ID, all[0].ID, all[1].ID)
	}
}
