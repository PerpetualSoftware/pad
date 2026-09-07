package store

import (
	"fmt"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestImportWorkspacePreservesReferenceTarget(t *testing.T) {
	t.Parallel()
	for _, deleteFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("deleted_first=%v", deleteFirst), func(t *testing.T) {
			s := testStore(t)
			owner := createTestUser(t, s, "audit@example.com", "Audit", "password123")
			ws := createTestWorkspace(t, s, "Reference source")
			coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Prefix: "TASK"})
			if err != nil {
				t.Fatal(err)
			}
			first := createTestItem(t, s, ws.ID, coll.ID, "Obsolete task", "")
			target := createTestItem(t, s, ws.ID, coll.ID, "Deploy the release", "")
			note := createTestItem(t, s, ws.ID, coll.ID, "Release instructions", "Follow [[TASK-2]] before continuing.")
			comment, err := s.CreateComment(ws.ID, note.ID, "", models.CommentCreate{Body: "Review [[TASK-2]] first."})
			if err != nil {
				t.Fatal(err)
			}
			// Deterministic fixture for ordinary items created one second apart.
			for i, item := range []*models.Item{first, target, note} {
				_, err := s.db.Exec(s.q("UPDATE items SET created_at = ? WHERE id = ?"), fmt.Sprintf("2026-09-01T12:00:0%dZ", i), item.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			before, err := s.ResolveItem(ws.ID, "TASK-2")
			if err != nil || before == nil || before.ID != target.ID {
				t.Fatalf("source control: item=%+v err=%v", before, err)
			}
			if deleteFirst {
				if err := s.DeleteItem(first.ID); err != nil {
					t.Fatal(err)
				}
			}
			bundle, err := s.ExportWorkspace(ws.Slug)
			if err != nil {
				t.Fatal(err)
			}
			dst, err := s.ImportWorkspace(bundle, "Reference restored", owner.ID, "cli")
			if err != nil {
				t.Fatal(err)
			}
			after, err := s.ResolveItem(dst.ID, "TASK-2")
			if err != nil || after == nil {
				t.Fatalf("restored ref lookup: item=%+v err=%v", after, err)
			}
			importedNote, err := s.ResolveItem(dst.ID, note.Slug)
			if err != nil || importedNote == nil {
				t.Fatalf("restored note lookup: item=%+v err=%v", importedNote, err)
			}
			t.Logf("before TASK-2=%q; after TASK-2=%q; restored body=%q; export/import succeeded", before.Title, after.Title, importedNote.Content)
			if after.Title != target.Title {
				t.Errorf("reference changed target: want %q, got %q", target.Title, after.Title)
			}
			if importedNote.Content != note.Content {
				t.Errorf("content changed: %q", importedNote.Content)
			}
			comments, err := s.ListComments(importedNote.ID)
			if err != nil || len(comments) != 1 || comments[0].Body != comment.Body {
				t.Errorf("comments did not round-trip: %+v, err=%v", comments, err)
			}
		})
	}
}

func TestImportWorkspaceItemNumberCompatibility(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		numbers []int
		want    []int
		orphan  bool
	}{
		{name: "out of order with gaps", numbers: []int{7, 2}, want: []int{7, 2}},
		{name: "legacy duplicate", numbers: []int{1, 1}, want: []int{1, 2}},
		{name: "missing number", numbers: []int{0, 2}, want: []int{1, 2}},
		{name: "negative number", numbers: []int{-1, 2}, want: []int{1, 2}},
		{name: "unnumbered archive", numbers: []int{0, 0}, want: []int{1, 2}},
		{name: "orphan duplicate ignored", numbers: []int{7, 2}, want: []int{7, 2}, orphan: true},
		{name: "empty archive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := testStore(t)
			owner := createTestUser(t, s, "import@example.com", "Importer", "password123")
			const timestamp = "2026-09-01T12:00:00Z"
			bundle := &models.WorkspaceExport{
				Version:   1,
				Workspace: models.WorkspaceExportMeta{Name: "Number archive", Slug: "number-archive"},
				Collections: []models.CollectionExport{
					{ID: "tasks", Name: "Tasks", Slug: "tasks", Prefix: "TASK", Schema: `{"fields":[]}`, CreatedAt: timestamp, UpdatedAt: timestamp},
					{ID: "plans", Name: "Plans", Slug: "plans", Prefix: "PLAN", Schema: `{"fields":[]}`, CreatedAt: timestamp, UpdatedAt: timestamp},
				},
			}
			for i, number := range tc.numbers {
				slug := fmt.Sprintf("item-%d", i)
				bundle.Items = append(bundle.Items, models.ItemExport{
					ID: slug, CollectionID: bundle.Collections[i].ID, Title: slug, Slug: slug,
					ItemNumber: number, CreatedAt: timestamp, UpdatedAt: timestamp,
				})
			}
			if tc.orphan {
				bundle.Items = append(bundle.Items, models.ItemExport{ID: "orphan", CollectionID: "missing", ItemNumber: 7})
			}
			dst, err := s.ImportWorkspace(bundle, "Imported numbers", owner.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			imported, err := s.ExportWorkspace(dst.Slug)
			if err != nil {
				t.Fatal(err)
			}
			if len(imported.Items) != len(tc.want) {
				t.Fatalf("imported %d items, want %d", len(imported.Items), len(tc.want))
			}
			maxNumber := 0
			for i, number := range tc.want {
				item, err := s.GetItemBySlug(dst.ID, bundle.Items[i].Slug)
				if err != nil || item == nil || item.ItemNumber == nil {
					t.Fatalf("read imported item: %+v, err=%v", item, err)
				}
				if *item.ItemNumber != number {
					t.Errorf("%s number=%d, want %d", item.Slug, *item.ItemNumber, number)
				}
				maxNumber = max(maxNumber, number)
			}
			created := createTestItem(t, s, dst.ID, imported.Collections[0].ID, "After import", "")
			if created.ItemNumber == nil || *created.ItemNumber != maxNumber+1 {
				t.Errorf("next number=%v, want %d", created.ItemNumber, maxNumber+1)
			}
		})
	}
}
