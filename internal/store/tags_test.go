package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// createTaggedItem creates an item with the given JSON tags array.
func createTaggedItem(t *testing.T, s *Store, wsID, collID, title, tags string) *models.Item {
	t.Helper()
	item, err := s.CreateItem(wsID, collID, models.ItemCreate{
		Title:  title,
		Fields: `{"status":"open"}`,
		Tags:   tags,
	})
	if err != nil {
		t.Fatalf("failed to create tagged item: %v", err)
	}
	return item
}

func TestListWorkspaceTags(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Tags WS")
	ideas := createTestCollection(t, s, ws.ID, "Ideas")
	bugs := createTestCollection(t, s, ws.ID, "Bugs")

	// "User feedback" spans BOTH collections (2 ideas + 1 bug) — the
	// cross-collection aggregation is the whole point of the feature.
	createTaggedItem(t, s, ws.ID, ideas.ID, "Idea A", `["User feedback","ux"]`)
	createTaggedItem(t, s, ws.ID, ideas.ID, "Idea B", `["User feedback"]`)
	createTaggedItem(t, s, ws.ID, bugs.ID, "Bug A", `["User feedback"]`)
	createTaggedItem(t, s, ws.ID, bugs.ID, "Bug B", `["perf"]`)
	createTaggedItem(t, s, ws.ID, bugs.ID, "Bug C", `[]`) // untagged — excluded

	t.Run("unfiltered aggregates across collections, ordered by count then tag", func(t *testing.T) {
		tags, err := s.ListWorkspaceTags(ws.ID, nil, nil)
		if err != nil {
			t.Fatalf("ListWorkspaceTags: %v", err)
		}
		want := []models.TagCount{
			{Tag: "User feedback", Count: 3},
			{Tag: "perf", Count: 1},
			{Tag: "ux", Count: 1},
		}
		if len(tags) != len(want) {
			t.Fatalf("got %d tags, want %d: %+v", len(tags), len(want), tags)
		}
		for i, w := range want {
			if tags[i] != w {
				t.Errorf("tag[%d] = %+v, want %+v", i, tags[i], w)
			}
		}
	})

	t.Run("collection filter scopes counts to visible collections", func(t *testing.T) {
		tags, err := s.ListWorkspaceTags(ws.ID, []string{ideas.ID}, nil)
		if err != nil {
			t.Fatalf("ListWorkspaceTags: %v", err)
		}
		want := []models.TagCount{
			{Tag: "User feedback", Count: 2}, // only the 2 ideas, not the bug
			{Tag: "ux", Count: 1},
		}
		if len(tags) != len(want) {
			t.Fatalf("got %d tags, want %d: %+v", len(tags), len(want), tags)
		}
		for i, w := range want {
			if tags[i] != w {
				t.Errorf("tag[%d] = %+v, want %+v", i, tags[i], w)
			}
		}
	})

	t.Run("non-nil empty collection set returns no tags", func(t *testing.T) {
		tags, err := s.ListWorkspaceTags(ws.ID, []string{}, nil)
		if err != nil {
			t.Fatalf("ListWorkspaceTags: %v", err)
		}
		if len(tags) != 0 {
			t.Errorf("expected empty result for no visible collections, got %+v", tags)
		}
	})

	t.Run("duplicate tags on one item count the item once", func(t *testing.T) {
		dupWS := createTestWorkspace(t, s, "Dup WS")
		coll := createTestCollection(t, s, dupWS.ID, "Ideas")
		// The write path doesn't enforce per-item tag uniqueness, so a single
		// item can carry ["ux","ux"]. The count is "items carrying the tag".
		createTaggedItem(t, s, dupWS.ID, coll.ID, "Dup", `["ux","ux"]`)
		tags, err := s.ListWorkspaceTags(dupWS.ID, nil, nil)
		if err != nil {
			t.Fatalf("ListWorkspaceTags: %v", err)
		}
		if len(tags) != 1 || tags[0].Tag != "ux" || tags[0].Count != 1 {
			t.Errorf("expected ux:1 (item counted once), got %+v", tags)
		}
	})

	t.Run("archived items are excluded", func(t *testing.T) {
		extra := createTaggedItem(t, s, ws.ID, ideas.ID, "Idea Archived", `["archive-only"]`)
		if err := s.DeleteItem(extra.ID); err != nil {
			t.Fatalf("DeleteItem: %v", err)
		}
		tags, err := s.ListWorkspaceTags(ws.ID, nil, nil)
		if err != nil {
			t.Fatalf("ListWorkspaceTags: %v", err)
		}
		for _, tc := range tags {
			if tc.Tag == "archive-only" {
				t.Errorf("archived item's tag leaked into results: %+v", tags)
			}
		}
	})
}
