package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3385. A thumbnail job snapshots its parent before it decodes and
// resizes (server/handlers_attachments_thumbnails.go); if the original is
// bound to an item (POST /attachments/{id}/attach) while the job runs, the
// job still hands CreateAttachmentVariantIfParentLive the snapshot's NULL
// item_id. The bind only updates variants that already exist, so the
// thumbnail was inserted unbound under a bound parent, and orphan GC later
// reaped it. The insert must take the parent's CURRENT binding, read under
// the parent-row lock it already holds.
//
// The race's outcome is reproduced deterministically: bind the parent, then
// insert a variant carrying the stale snapshot. The unbound-parent leg is the
// control: the same call with a parent that really is unbound must leave the
// variant unbound, so the bound leg is about the parent's state and not a
// constant.
func TestBug3385_VariantTakesParentsCurrentBinding(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Bug 3385")
	coll := createTestCollection(t, s, ws.ID, "Docs")
	item := createTestItem(t, s, ws.ID, coll.ID, "Has a picture", "")

	variantOf := func(parent *models.Attachment, staleItemID *string) *models.Attachment {
		t.Helper()
		pid, v := parent.ID, "thumb-sm"
		row := &models.Attachment{
			WorkspaceID: ws.ID,
			ItemID:      staleItemID, // the job's earlier snapshot
			UploadedBy:  parent.UploadedBy,
			StorageKey:  "fs:" + newID(),
			ContentHash: newID(),
			MimeType:    "image/png",
			SizeBytes:   1,
			Filename:    "thumb.png",
			ParentID:    &pid,
			Variant:     &v,
		}
		ok, err := s.CreateAttachmentVariantIfParentLive(row)
		if err != nil || !ok {
			t.Fatalf("CreateAttachmentVariantIfParentLive: ok=%v err=%v", ok, err)
		}
		got, err := s.GetAttachment(row.ID)
		if err != nil || got == nil {
			t.Fatalf("GetAttachment(variant): %v", err)
		}
		return got
	}

	// Bound parent, stale (NULL) snapshot: the variant must be bound.
	bound := seedAttachmentNamed(t, s, ws, "photo.png")
	if err := s.AttachAttachmentToItem(ws.ID, bound.ID, item.ID); err != nil {
		t.Fatalf("AttachAttachmentToItem: %v", err)
	}
	v := variantOf(bound, nil)
	if v.ItemID == nil || *v.ItemID != item.ID {
		t.Errorf("variant of a bound parent inserted with item_id=%v, want %s", v.ItemID, item.ID)
	}

	// Control: a parent that is really unbound gives an unbound variant.
	loose := seedAttachmentNamed(t, s, ws, "loose.png")
	if lv := variantOf(loose, nil); lv.ItemID != nil {
		t.Errorf("variant of an unbound parent inserted with item_id=%v, want nil", *lv.ItemID)
	}
}
