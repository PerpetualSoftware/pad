//go:build !libvips

package server

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Thumbnail-derivation parent-item tests for PLAN-2391 / TASK-2404 (DR-14).
//
// deriveThumbnails checked only that the parent ATTACHMENT row was live and
// then copied parent.ItemID into every derived row. After TASK-2401's read
// gate a variant of an archived item's attachment is quota-counted storage
// that the blob path refuses to serve, so derivation now skips a parent whose
// ITEM is soft-deleted or whose item_id resolves to nothing in the row's own
// workspace.
//
// These tests cover the SEQUENTIAL cases only — the item is already gone
// before derivation starts. The raced case (item archived after the check,
// while the resize is running) is deliberately NOT asserted: that window is
// accepted behaviour, documented on thumbnailParentItemLive, so an assertion
// either way would pin down something the design leaves free.

// derivationFixture is a workspace + collection + item, on a server with the
// attachment registry and the pure-Go image processor wired.
type derivationFixture struct {
	srv    *Server
	wsID   string
	colID  string
	itemID string
}

func newDerivationFixture(t *testing.T) derivationFixture {
	t.Helper()
	srv, _ := testServerWithAttachments(t)

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "DeriveParent"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	col, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Schema: `{"fields":[]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	item, err := srv.store.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title: "Parent", Fields: `{}`,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	return derivationFixture{srv: srv, wsID: ws.ID, colID: col.ID, itemID: item.ID}
}

// attachTo writes a real 2000x1500 PNG — big enough that BOTH specs must
// scale down, so a skip is unambiguous — and returns the new row's id.
// Width/Height are left nil so the "source already within bounds" skip
// cannot be what suppresses derivation.
func (f derivationFixture) attachTo(t *testing.T, itemID *string) string {
	t.Helper()
	return putBlob(t, f.srv, &models.Attachment{
		WorkspaceID: f.wsID,
		ItemID:      itemID,
		Filename:    "screenshot.png",
	}, makeIntegrationPNG(t, 2000, 1500)).ID
}

// assertNoVariants fails if any spec produced a row.
func assertNoVariants(t *testing.T, srv *Server, parentID string) {
	t.Helper()
	for _, variant := range []string{models.AttachmentVariantThumbSm, models.AttachmentVariantThumbMd} {
		row, err := variantRow(t, srv, parentID, variant)
		if err != nil {
			t.Fatalf("GetAttachmentVariant(%s): %v", variant, err)
		}
		if row != nil {
			t.Errorf("variant %q was derived; derivation should have been skipped", variant)
		}
	}
}

// assertVariants fails unless every spec produced a row. This is the control
// that keeps the skip tests honest: same fixture, same bytes, live parent.
func assertVariants(t *testing.T, srv *Server, parentID string) {
	t.Helper()
	for _, variant := range []string{models.AttachmentVariantThumbSm, models.AttachmentVariantThumbMd} {
		row, err := variantRow(t, srv, parentID, variant)
		if err != nil {
			t.Fatalf("GetAttachmentVariant(%s): %v", variant, err)
		}
		if row == nil {
			t.Fatalf("variant %q missing; the fixture does not derive at all, "+
				"so the skip assertions elsewhere in this file prove nothing", variant)
		}
	}
}

// TestThumbnails_DerivedForLiveParentItem is the control. It shares the
// fixture and the source bytes with the skip tests below, so if this one
// fails they stop being evidence.
func TestThumbnails_DerivedForLiveParentItem(t *testing.T) {
	f := newDerivationFixture(t)
	attID := f.attachTo(t, &f.itemID)

	f.srv.deriveThumbnails(attID)

	assertVariants(t, f.srv, attID)
}

// TestThumbnails_SkippedForArchivedParentItem — the sequential DR-14 case.
// The item is soft-deleted before derivation runs, so the variants would be
// quota-counted bytes that the blob path (DR-13) refuses to serve.
func TestThumbnails_SkippedForArchivedParentItem(t *testing.T) {
	f := newDerivationFixture(t)
	attID := f.attachTo(t, &f.itemID)

	if err := f.srv.store.DeleteItem(f.itemID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	// Sanity: DeleteItem must be a SOFT delete, or this test would be
	// asserting the wrong mechanism.
	item, err := f.srv.store.GetItemIncludeDeleted(f.itemID)
	if err != nil {
		t.Fatalf("GetItemIncludeDeleted: %v", err)
	}
	if item == nil || item.DeletedAt == nil {
		t.Fatalf("item is not soft-deleted after DeleteItem: %+v", item)
	}

	f.srv.deriveThumbnails(attID)

	assertNoVariants(t, f.srv, attID)
}

// TestThumbnails_SkippedForUnresolvableParentItem — a malformed item_id that
// names no item at all. attachments.item_id carries no FK, so this shape is
// representable until PLAN-2397's repair lands.
func TestThumbnails_SkippedForUnresolvableParentItem(t *testing.T) {
	f := newDerivationFixture(t)
	ghost := "00000000-0000-0000-0000-0000000000ff"
	attID := f.attachTo(t, &ghost)

	f.srv.deriveThumbnails(attID)

	assertNoVariants(t, f.srv, attID)
}

// TestThumbnails_SkippedForForeignWorkspaceParentItem — a malformed item_id
// that resolves, but to an item in ANOTHER workspace. Resolution alone would
// pass; only the workspace-identity half of the check catches it.
func TestThumbnails_SkippedForForeignWorkspaceParentItem(t *testing.T) {
	f := newDerivationFixture(t)

	other, err := f.srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Elsewhere"})
	if err != nil {
		t.Fatalf("CreateWorkspace other: %v", err)
	}
	otherCol, err := f.srv.store.CreateCollection(other.ID, models.CollectionCreate{
		Name: "Tasks", Schema: `{"fields":[]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection other: %v", err)
	}
	otherItem, err := f.srv.store.CreateItem(other.ID, otherCol.ID, models.ItemCreate{
		Title: "Foreign", Fields: `{}`,
	})
	if err != nil {
		t.Fatalf("CreateItem other: %v", err)
	}

	attID := f.attachTo(t, &otherItem.ID)

	f.srv.deriveThumbnails(attID)

	assertNoVariants(t, f.srv, attID)
}

// TestThumbnails_DerivedForOrphanAttachment — an orphan row (item_id NULL)
// has no item to check, so derivation must still run. Guards against the
// skip being implemented as "no live item ⇒ skip".
func TestThumbnails_DerivedForOrphanAttachment(t *testing.T) {
	f := newDerivationFixture(t)
	attID := f.attachTo(t, nil)

	f.srv.deriveThumbnails(attID)

	assertVariants(t, f.srv, attID)
}
