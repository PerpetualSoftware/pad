package store

import (
	"fmt"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// DR-15 pinning tests (PLAN-2392). Three attachment semantics that the copy /
// move / bundle paths rely on silently; pinned here so a future change that
// breaks any of them fails loudly rather than shipping. No behavior changes —
// these lock down what the code already does.
//
// All three use only store methods and the shared cross-workspace-copy harness
// (copyFixture, attachIn, ...) so they are dual-dialect and pass under
// make test-pg as well as SQLite.

// dr15CopyOneAttachment sets up workspace A with a single original attachment
// referenced by a source item, copies that item to workspace B, and returns the
// source row plus its lone clone in B. The original has no variants, so exactly
// one row lands in B — which keeps the independence assertions below free of any
// dependence on row ordering.
func dr15CopyOneAttachment(t *testing.T) (copyFixture, *models.Attachment, *models.Attachment) {
	t.Helper()
	f := newCopyFixture(t)
	orig := f.attachIn(t, f.wsA.ID, "diagram.png", 4096)
	src, err := f.s.CreateItem(f.wsA.ID, f.colA.ID, models.ItemCreate{
		Title:   "Has attachment",
		Content: fmt.Sprintf("![d](pad-attachment:%s)", orig.ID),
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	req := f.req()
	req.SourceItemID = src.ID
	res := f.copy(t, req)
	if res.AttachmentsCopied != 1 {
		t.Fatalf("AttachmentsCopied = %d, want 1 (the referenced original)", res.AttachmentsCopied)
	}
	rows := attachmentsIn(t, f.s, f.wsB.ID)
	if len(rows) != 1 {
		t.Fatalf("workspace B has %d attachment rows, want the 1 clone", len(rows))
	}
	clone := rows[0]
	// The clone is a fresh, live row that shares only the bytes: distinct id,
	// deleted_at NULL, same content hash (content-addressed storage, so a
	// same-instance copy is a row copy, not a byte copy). If any of these breaks,
	// the delete-independence below is not what it claims to test.
	if clone.ID == orig.ID {
		t.Fatalf("clone reused the source id %s", orig.ID)
	}
	if clone.DeletedAt != nil {
		t.Fatalf("clone %s was minted soft-deleted", clone.ID)
	}
	if clone.ContentHash != orig.ContentHash {
		t.Fatalf("clone %s content hash = %q, want the source's %q (shared bytes)", clone.ID, clone.ContentHash, orig.ContentHash)
	}
	return f, orig, &clone
}

// DR-15 (clone independence): deleting the CLONE leaves the source row live.
// The clone is a separate row, so its soft-delete must not touch the source —
// otherwise a delete in workspace B would silently remove the file from
// workspace A, which content-addressed sharing makes plausible-looking.
func TestDR15_DeletingCloneLeavesSourceLive(t *testing.T) {
	f, orig, clone := dr15CopyOneAttachment(t)

	if err := f.s.SoftDeleteAttachment(clone.ID); err != nil {
		t.Fatalf("SoftDeleteAttachment(clone): %v", err)
	}

	gotSrc, err := f.s.GetAttachment(orig.ID)
	if err != nil {
		t.Fatalf("GetAttachment(source): %v", err)
	}
	if gotSrc == nil || gotSrc.DeletedAt != nil {
		t.Errorf("deleting the clone disturbed the source row: %+v", gotSrc)
	}
	gotClone, err := f.s.GetAttachment(clone.ID)
	if err != nil {
		t.Fatalf("GetAttachment(clone): %v", err)
	}
	if gotClone == nil || gotClone.DeletedAt == nil {
		t.Errorf("the clone was not soft-deleted: %+v", gotClone)
	}
}

// DR-15 (clone independence, the other direction): deleting the SOURCE leaves
// the clone live. Symmetric to the above — the two rows share bytes but neither
// delete cascades to the other. (There is no store-level attachment RESTORE, so
// independence is pinned via delete in both directions; restore would ride the
// same row separation.)
func TestDR15_DeletingSourceLeavesCloneLive(t *testing.T) {
	f, orig, clone := dr15CopyOneAttachment(t)

	if err := f.s.SoftDeleteAttachment(orig.ID); err != nil {
		t.Fatalf("SoftDeleteAttachment(source): %v", err)
	}

	gotClone, err := f.s.GetAttachment(clone.ID)
	if err != nil {
		t.Fatalf("GetAttachment(clone): %v", err)
	}
	if gotClone == nil || gotClone.DeletedAt != nil {
		t.Errorf("deleting the source disturbed the clone row: %+v", gotClone)
	}
	gotSrc, err := f.s.GetAttachment(orig.ID)
	if err != nil {
		t.Fatalf("GetAttachment(source): %v", err)
	}
	if gotSrc == nil || gotSrc.DeletedAt == nil {
		t.Errorf("the source was not soft-deleted: %+v", gotSrc)
	}
}

// DR-15 (a move charges both workspaces): moving an item (ArchiveSource) archives
// only the source ITEM and clones its attachments into the destination — it does
// NOT soft-delete the source attachment ROWS, so the bytes are counted in both
// workspaces' storage usage. Pre-existing and arguably intended; pinned so it
// isn't a surprise, and so a change that started charging only one side fails
// here.
func TestDR15_MoveLeavesSourceAttachmentsLiveAndChargesBoth(t *testing.T) {
	f := newCopyFixture(t)
	// The source item comes first so the original can be ATTACHED to it
	// (item_id = src.ID) — which is what makes the "move leaves source
	// attachments live" pin honest: a regression that cascaded the archive to the
	// item's own attachments (WHERE item_id = the archived item) would soft-delete
	// this original, and the assertions below would catch it. An unattached
	// (item_id NULL) original would sail straight past such a cascade.
	src, err := f.s.CreateItem(f.wsA.ID, f.colA.ID, models.ItemCreate{
		Title:   "Moving out with an attachment",
		Content: "",
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	orig := &models.Attachment{
		WorkspaceID: f.wsA.ID,
		ItemID:      &src.ID,
		UploadedBy:  "source-uploader",
		StorageKey:  "fs:" + newID(),
		ContentHash: newID(),
		MimeType:    "image/png",
		SizeBytes:   4096,
		Filename:    "diagram.png",
	}
	if err := f.s.CreateAttachment(orig); err != nil {
		t.Fatalf("CreateAttachment(original): %v", err)
	}
	thumb := f.variantOf(t, f.wsA.ID, orig, "thumb-md", 512)

	// Now that the ids exist, point the item's content at the original; the copy
	// clones it by reference and auto-pulls the variant into the plan.
	refContent := fmt.Sprintf("![d](pad-attachment:%s)", orig.ID)
	if _, err := f.s.UpdateItem(src.ID, models.ItemUpdate{Content: &refContent}); err != nil {
		t.Fatalf("UpdateItem(content): %v", err)
	}

	req := f.req()
	req.SourceItemID = src.ID
	req.ArchiveSource = true
	res := f.copy(t, req)

	if !res.Move.ArchivedSource {
		t.Fatal("provenance says archived_source=false for a move")
	}
	// The source ITEM is SOFT-deleted — a tombstone row remains with deleted_at
	// set. Asserted via the include-deleted read so a hard delete (row gone) or a
	// read error can't masquerade as a successful archive.
	archived, err := f.s.GetItemIncludeDeleted(src.ID)
	if err != nil {
		t.Fatalf("GetItemIncludeDeleted(source): %v", err)
	}
	if archived == nil {
		t.Fatal("source item row vanished after a move; a move soft-deletes the source, it does not hard-delete")
	}
	if archived.DeletedAt == nil {
		t.Error("source item is still live after a move")
	}
	// ...but the source ATTACHMENT rows are untouched: both stay live.
	for _, id := range []string{orig.ID, thumb.ID} {
		a, err := f.s.GetAttachment(id)
		if err != nil {
			t.Fatalf("GetAttachment(%s): %v", id, err)
		}
		if a == nil || a.DeletedAt != nil {
			t.Errorf("move soft-deleted source attachment %s: %+v", id, a)
		}
	}

	// The clones actually landed on the copied item (not orphaned or
	// mis-associated), live, with the variant reparented to the new original.
	// Order-independent: find the parentless original, then check every row.
	if res.AttachmentsCopied != 2 {
		t.Fatalf("AttachmentsCopied = %d, want 2 (original + variant)", res.AttachmentsCopied)
	}
	destRows := attachmentsIn(t, f.s, f.wsB.ID)
	if len(destRows) != 2 {
		t.Fatalf("workspace B has %d attachment rows, want 2", len(destRows))
	}
	var origins, variants []models.Attachment
	for _, a := range destRows {
		if a.DeletedAt != nil {
			t.Errorf("clone %s landed soft-deleted", a.ID)
		}
		if a.ItemID == nil || *a.ItemID != res.Item.ID {
			t.Errorf("clone %s item_id = %v, want the copied item %s", a.ID, a.ItemID, res.Item.ID)
		}
		if a.ParentID == nil {
			origins = append(origins, a)
		} else {
			variants = append(variants, a)
		}
	}
	// Exactly one parentless original and one variant reparented onto it —
	// pinning the topology, so a dropped variant parent_id (two originals) or an
	// inverted parent/child mapping fails here rather than sliding through.
	if len(origins) != 1 {
		t.Fatalf("workspace B has %d parentless originals, want exactly 1", len(origins))
	}
	if len(variants) != 1 {
		t.Fatalf("workspace B has %d variant rows, want exactly 1", len(variants))
	}
	if variants[0].ParentID == nil || *variants[0].ParentID != origins[0].ID {
		t.Errorf("cloned variant parent_id = %v, want the new original %s", variants[0].ParentID, origins[0].ID)
	}
	if variants[0].Variant == nil {
		t.Error("cloned variant has a nil variant kind")
	}

	// Storage usage counts the same bytes in BOTH workspaces: the fixture starts
	// empty, so each side is exactly the original + its variant (4096 + 512).
	const wantBytes = 4096 + 512
	usageA, err := f.s.WorkspaceStorageUsage(f.wsA.ID)
	if err != nil {
		t.Fatalf("WorkspaceStorageUsage(A): %v", err)
	}
	if usageA != wantBytes {
		t.Errorf("source workspace storage usage = %d, want %d (the move must not release the source bytes)", usageA, wantBytes)
	}
	usageB, err := f.s.WorkspaceStorageUsage(f.wsB.ID)
	if err != nil {
		t.Fatalf("WorkspaceStorageUsage(B): %v", err)
	}
	if usageB != wantBytes {
		t.Errorf("destination workspace storage usage = %d, want %d (the clone must be charged there too)", usageB, wantBytes)
	}
}

// DR-15 (a bundle round-trip can orphan an attachment): the workspace bundle
// pairs two store queries that disagree about a soft-deleted parent —
// WorkspaceAttachmentsForExport includes a LIVE attachment whose parent item is
// soft-deleted, while ExportWorkspace's item list EXCLUDES that item. On import,
// the attachment's ItemID is remapped through the exported items' slugs
// (handlers_import_bundle.go:479-510); with no exported item carrying that id,
// the remap misses and the imported row lands with ItemID = nil — an orphan.
// This pins the store-level divergence that is the root of that orphan; it is
// known behavior, not fixed here.
func TestDR15_BundleExportOrphansAttachmentOfSoftDeletedParent(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Bundle Orphan")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Doomed parent", "body")
	// A live sibling that MUST survive into the export, so the "deleted parent is
	// absent" assertion below can't pass vacuously by the export returning nothing.
	control := createTestItem(t, s, ws.ID, col.ID, "Live sibling", "still here")

	itemID := item.ID
	a := &models.Attachment{
		WorkspaceID: ws.ID,
		ItemID:      &itemID,
		UploadedBy:  "uploader",
		StorageKey:  "fs:" + newID(),
		ContentHash: newID(),
		MimeType:    "image/png",
		SizeBytes:   2048,
		Filename:    "attached.png",
	}
	if err := s.CreateAttachment(a); err != nil {
		t.Fatalf("CreateAttachment: %v", err)
	}

	// Soft-delete the PARENT ITEM — the attachment row itself stays live.
	if err := s.DeleteItem(item.ID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	// The parent is SOFT-deleted (row present, deleted_at set), not hard-deleted:
	// a hard delete would produce the same export mismatch for a different reason,
	// so pin the actual DR-15 scenario.
	deletedParent, err := s.GetItemIncludeDeleted(item.ID)
	if err != nil {
		t.Fatalf("GetItemIncludeDeleted: %v", err)
	}
	if deletedParent == nil || deletedParent.DeletedAt == nil {
		t.Fatalf("parent item is not soft-deleted: %+v", deletedParent)
	}

	// The attachment export still includes the live attachment...
	atts, err := s.WorkspaceAttachmentsForExport(ws.ID)
	if err != nil {
		t.Fatalf("WorkspaceAttachmentsForExport: %v", err)
	}
	var exported *models.Attachment
	for i := range atts {
		if atts[i].ID == a.ID {
			cp := atts[i]
			exported = &cp
		}
	}
	if exported == nil {
		t.Fatal("the bundle attachment export dropped a live attachment whose parent item was soft-deleted; the orphan pin would be vacuous")
	}
	// ...still pointing at the now-soft-deleted parent, which is what import
	// tries (and fails) to remap.
	if exported.ItemID == nil || *exported.ItemID != item.ID {
		t.Fatalf("exported attachment item_id = %v, want the soft-deleted parent %s", exported.ItemID, item.ID)
	}

	// But the item export the bundle pairs it with excludes that soft-deleted
	// item, so nothing carries its id/slug for the import remap to land on.
	exp, err := s.ExportWorkspace(ws.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}
	var sawControl, sawDeletedParent bool
	for _, it := range exp.Items {
		switch it.ID {
		case item.ID:
			sawDeletedParent = true
		case control.ID:
			sawControl = true
		}
	}
	if !sawControl {
		t.Fatal("the item export returned no live items (the control is missing); the absent-parent check would be vacuous")
	}
	if sawDeletedParent {
		t.Fatalf("the item export included the soft-deleted parent %s; import could remap and there would be no orphan", item.ID)
	}
}
