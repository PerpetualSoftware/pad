package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestCountWorkspaceContents_MatchesTheExport pins the property the
// post-import check rests on (TASK-3612): each count equals the length of the
// array the export writes, so a bundle's in_bundle and a workspace's imported
// counts are comparable. The workspace holds one of every row the export
// treats specially: an archived collection (exported), a deleted item and a
// tombstoned comment (not exported), and a thumbnail (not exported).
func TestCountWorkspaceContents_MatchesTheExport(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Counted")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	archived := createTestCollection(t, s, ws.ID, "Old Stuff")
	if err := s.DeleteCollection(archived.ID, ""); err != nil {
		t.Fatalf("archive collection: %v", err)
	}

	kept := createTestItem(t, s, ws.ID, col.ID, "Kept", "body")
	createTestItem(t, s, ws.ID, col.ID, "Also kept", "body")
	gone := createTestItem(t, s, ws.ID, col.ID, "Gone", "body")
	if err := s.DeleteItem(gone.ID); err != nil {
		t.Fatalf("delete item: %v", err)
	}

	if _, err := s.CreateComment(ws.ID, kept.ID, "", models.CommentCreate{Body: "stays"}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	tomb, err := s.CreateComment(ws.ID, kept.ID, "", models.CommentCreate{Body: "goes"})
	if err != nil {
		t.Fatalf("comment: %v", err)
	}
	// A reply keeps the deleted comment as a TOMBSTONE row; without one the
	// delete is a hard delete and there is nothing for the filter to skip.
	if _, err := s.CreateComment(ws.ID, kept.ID, "", models.CommentCreate{Body: "reply", ParentID: tomb.ID}); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if err := s.DeleteComment(tomb.ID); err != nil {
		t.Fatalf("delete comment: %v", err)
	}

	itemID := kept.ID
	orig := &models.Attachment{
		WorkspaceID: ws.ID, ItemID: &itemID, UploadedBy: "uploader",
		StorageKey: "fs:" + newID(), ContentHash: newID(), MimeType: "image/png",
		SizeBytes: 2048, Filename: "diagram.png",
	}
	if err := s.CreateAttachment(orig); err != nil {
		t.Fatalf("create original: %v", err)
	}
	variant := "thumb-md"
	thumb := &models.Attachment{
		WorkspaceID: ws.ID, ItemID: &itemID, UploadedBy: "uploader",
		StorageKey: "fs:" + newID(), ContentHash: newID(), MimeType: "image/png",
		SizeBytes: 512, Filename: "diagram.png", ParentID: &orig.ID, Variant: &variant,
	}
	if err := s.CreateAttachment(thumb); err != nil {
		t.Fatalf("create thumbnail: %v", err)
	}

	export, err := s.ExportWorkspace(ws.Slug)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	atts, err := s.WorkspaceAttachmentsForExport(ws.ID)
	if err != nil {
		t.Fatalf("export attachments: %v", err)
	}
	want := WorkspaceContentCounts{
		Collections: len(export.Collections),
		Items:       len(export.Items),
		Comments:    len(export.Comments),
		Attachments: len(atts),
	}
	// Guard against a vacuous match: the export must have seen the rows the
	// fixture built, with the excluded ones left out.
	if want.Items != 2 || want.Comments != 2 || want.Attachments != 1 {
		t.Fatalf("fixture not as built: export carries %+v", want)
	}
	foundArchived := false
	for _, c := range export.Collections {
		if c.ID == archived.ID {
			foundArchived = true
		}
	}
	if !foundArchived {
		t.Fatal("fixture not as built: the export left out the archived collection")
	}

	got, err := s.CountWorkspaceContents(ws.ID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != want {
		t.Errorf("CountWorkspaceContents = %+v, the export carries %+v", got, want)
	}
}
