package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2629 — RestoreItem / RestoreDocument must re-stamp the attachment
// references in the restored content, so an orphan-GC claim racing the
// restore is refused. The claim (ClaimNeverAttachedAttachment) bypasses the
// live AttachmentReferenced scan and keys on last_referenced_at; if that has
// gone stale during the archive window and the restore does not refresh it,
// the claim reclaims a now-referenced attachment.
//
// ARMED FOR THE RIGHT REASON: last_referenced_at is aged to BEFORE the
// claim's refCutoff, so the ONLY thing that can refuse the claim is a fresh
// stamp written BY the restore. A full post-restore sweep would be vacuous
// here — its live AttachmentReferenced scan protects the now-live content
// regardless of the fix — so these drive ClaimNeverAttachedAttachment
// directly, which is the claim half the restore actually races. The aged
// stamp is the counterfactual arm: without it, CreateDocument/CreateItem's
// own fresh stamp would refuse the claim and the test would pass for the
// wrong reason.

// stampAged forces an attachment's last_referenced_at to `when`, standing in
// for time elapsing while the referencing content sat archived.
func stampAged(t *testing.T, f *gcClaimFixture, id string, when time.Time) {
	t.Helper()
	ts := when.UTC().Format(time.RFC3339)
	if _, err := f.s.db.Exec(f.s.q(`UPDATE attachments SET last_referenced_at = ? WHERE id = ?`), ts, id); err != nil {
		t.Fatalf("age last_referenced_at: %v", err)
	}
}

const restoreStaleWindow = 15 * time.Minute

func TestRestoreDocument_ReStampsAttachmentRefs(t *testing.T) {
	f := newGCClaimFixture(t)
	att := f.seedNeverAttached(t)

	doc, err := f.s.CreateDocument(f.wsID, models.DocumentCreate{
		Title:   "spec",
		Content: "diagram ![d](pad-attachment:" + att.ID + ")",
	})
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	if f.lastReferencedAt(t, att.ID) == nil {
		t.Fatalf("precondition: CreateDocument did not stamp the reference")
	}

	// Simulate the archive window: the create-time stamp goes stale.
	stale := time.Now().Add(-restoreStaleWindow - time.Hour)
	stampAged(t, f, att.ID, stale)

	if err := f.s.DeleteDocument(doc.ID); err != nil {
		t.Fatalf("DeleteDocument (soft): %v", err)
	}
	if _, err := f.s.RestoreDocument(doc.ID); err != nil {
		t.Fatalf("RestoreDocument: %v", err)
	}

	// The claim the sweep would run while racing this restore. A fresh
	// restore-stamp (>= refCutoff) refuses it; the aged stamp does not.
	refCutoff := time.Now().Add(-restoreStaleWindow)
	claimed, err := f.s.ClaimNeverAttachedAttachment(att.ID, refCutoff)
	if err != nil {
		t.Fatalf("ClaimNeverAttachedAttachment: %v", err)
	}
	if claimed {
		t.Error("attachment claimed after RestoreDocument — restore did not re-stamp the reference (BUG-2629)")
	}
	if !f.rowExists(t, att.ID) {
		t.Error("attachment row gone after restore+claim — reference not protected")
	}
}

func TestRestoreItem_ReStampsAttachmentRefs(t *testing.T) {
	f := newGCClaimFixture(t)
	// Unattached upload (item_id NULL) referenced only from an item's content
	// — the reachable item-leg case (an item-attached row carries item_id and
	// is refused by the claim's own predicate).
	att := f.seedNeverAttached(t)

	item, err := f.s.CreateItem(f.wsID, f.collID, models.ItemCreate{
		Title:   "host",
		Content: "shot ![s](pad-attachment:" + att.ID + ")",
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if f.lastReferencedAt(t, att.ID) == nil {
		t.Fatalf("precondition: CreateItem did not stamp the reference")
	}

	stale := time.Now().Add(-restoreStaleWindow - time.Hour)
	stampAged(t, f, att.ID, stale)

	if err := f.s.DeleteItem(item.ID); err != nil {
		t.Fatalf("DeleteItem (soft): %v", err)
	}
	if _, err := f.s.RestoreItem(item.ID); err != nil {
		t.Fatalf("RestoreItem: %v", err)
	}

	refCutoff := time.Now().Add(-restoreStaleWindow)
	claimed, err := f.s.ClaimNeverAttachedAttachment(att.ID, refCutoff)
	if err != nil {
		t.Fatalf("ClaimNeverAttachedAttachment: %v", err)
	}
	if claimed {
		t.Error("attachment claimed after RestoreItem — restore did not re-stamp the reference (BUG-2629)")
	}
	if !f.rowExists(t, att.ID) {
		t.Error("attachment row gone after restore+claim — reference not protected")
	}
}

// TestRestoreItem_ReStampsFieldsRefs covers the item fields JSON channel — an
// attachment referenced only from a field value (e.g. a cover image) must be
// re-stamped on restore too, since RestoreItem stamps content AND fields.
func TestRestoreItem_ReStampsFieldsRefs(t *testing.T) {
	f := newGCClaimFixture(t)
	att := f.seedNeverAttached(t)

	item, err := f.s.CreateItem(f.wsID, f.collID, models.ItemCreate{
		Title:  "host",
		Fields: `{"status":"open","cover":"pad-attachment:` + att.ID + `"}`,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if f.lastReferencedAt(t, att.ID) == nil {
		t.Fatalf("precondition: CreateItem did not stamp the fields reference")
	}

	stampAged(t, f, att.ID, time.Now().Add(-restoreStaleWindow-time.Hour))
	if err := f.s.DeleteItem(item.ID); err != nil {
		t.Fatalf("DeleteItem (soft): %v", err)
	}
	if _, err := f.s.RestoreItem(item.ID); err != nil {
		t.Fatalf("RestoreItem: %v", err)
	}

	claimed, err := f.s.ClaimNeverAttachedAttachment(att.ID, time.Now().Add(-restoreStaleWindow))
	if err != nil {
		t.Fatalf("ClaimNeverAttachedAttachment: %v", err)
	}
	if claimed {
		t.Error("fields-referenced attachment claimed after RestoreItem — restore did not stamp fields (BUG-2629)")
	}
}
