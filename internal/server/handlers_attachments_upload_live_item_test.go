package server

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// Upload-path live-parent tests (PLAN-2391 DR-14).
//
// handleUploadAttachment validated the parent item near the top of the
// handler and inserted the row with plain CreateAttachment at the bottom.
// Everything in between — spooling the multipart body to a temp file,
// hashing it, probing dimensions, store.Put — is unbounded work, and item
// deletion commits in its own transaction, so an item archived during the
// upload window left a quota-counted live row bound to an archived parent
// whose bytes the read gate (DR-13) then refuses to serve. The handler now
// inserts through store.CreateAttachmentForLiveItem, which re-checks the
// parent under a row lock inside the insert's own transaction.
//
// Transform closes the same window the same way. Derivation deliberately
// does NOT — see the comment on thumbnailParentItemLive.

// archivingStore wraps an AttachmentStore and runs fn inside Put, once, and
// ONLY while armed. Put is the last thing the upload handler does before the
// insert, so it is a deterministic harness for the check-then-work window.
//
// Armed rather than an unconditional sync.Once for the reason the transform
// test's Encode hook documents: thumbnail derivation calls Put too, on a
// background goroutine this test is not synchronised with, and an unarmed
// hook would fire on whichever Put wins.
type archivingStore struct {
	attachments.AttachmentStore

	mu    sync.Mutex
	armed bool
	fired bool
	fn    func()
}

func (s *archivingStore) arm() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armed = true
}

func (s *archivingStore) didFire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fired
}

func (s *archivingStore) Put(ctx context.Context, hash, mime string, r io.Reader) (string, error) {
	s.mu.Lock()
	run := s.armed && !s.fired
	if run {
		s.fired = true
		s.armed = false
	}
	s.mu.Unlock()
	if run {
		s.fn()
	}
	return s.AttachmentStore.Put(ctx, hash, mime, r)
}

// TestUpload_ItemArchivedMidUploadIsRefused drives the window directly: the
// item is archived from inside store.Put, strictly after the handler's
// up-front validation and strictly before the insert.
//
// The assertion is on the persisted state as much as the status: a row bound
// to the archived item is exactly the artifact the fix exists to prevent,
// and it would be unreadable and quota-counted for as long as the item stays
// archived.
func TestUpload_ItemArchivedMidUploadIsRefused(t *testing.T) {
	srv, _ := testServerWithAttachments(t)

	dir := t.TempDir()
	fs, err := attachments.NewFSStore(dir)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	slug, item := uploadItemFixture(t, srv, "UploadMidArchive")

	var archiveErr error
	hooked := &archivingStore{AttachmentStore: fs}
	hooked.fn = func() { archiveErr = srv.store.DeleteItem(item.ID) }

	reg := attachments.NewRegistry()
	reg.Register(attachments.FSPrefix, hooked)
	srv.SetAttachments(reg, 0)

	hooked.arm()
	rr := uploadChannels(srv, slug, item.ID, "", realPNG())

	if !hooked.didFire() {
		t.Fatal("the archive hook never ran — the request was refused before store.Put, " +
			"so this test says nothing about the check-then-work window it exists for")
	}
	if archiveErr != nil {
		t.Fatalf("DeleteItem from the hook: %v", archiveErr)
	}
	// Sanity: DeleteItem really is a soft delete, so GetItem resolving to
	// nil below means "archived" and not "hard gone".
	if live, gErr := srv.store.GetItemIncludeDeleted(item.ID); gErr != nil || live == nil {
		t.Fatalf("GetItemIncludeDeleted after archive: item=%v err=%v — the hook did "+
			"something other than a soft delete", live, gErr)
	}

	if rr.Code != 404 {
		t.Fatalf("upload against an item archived mid-flight: status = %d, want 404; body = %s",
			rr.Code, rr.Body.String())
	}
	// ...and byte-identical to every other attachment denial: the caller must
	// not be able to tell "your item was archived just now" from anything else.
	if got, want := rr.Body.String(),
		`{"error":{"code":"not_found","message":"Attachment not found"}}`+"\n"; got != want {
		t.Errorf("denial body = %q, want the shared attachment 404 %q", got, want)
	}

	// The listing deliberately surfaces attachments whose parent item is
	// soft-deleted (they still consume quota), so a row bound to the archived
	// item WOULD show up here if one had been written.
	_, total, lErr := srv.store.WorkspaceAttachments(
		itemWorkspaceID(t, srv, item.ID), store.AttachmentListFilters{ItemID: item.ID})
	if lErr != nil {
		t.Fatalf("WorkspaceAttachments: %v", lErr)
	}
	if total != 0 {
		t.Fatalf("%d attachment row(s) were written against the archived item — the "+
			"insert must refuse rather than bind to an archived parent", total)
	}
}

// TestUpload_LiveItemStillSucceeds is the control for the test above: the
// same fixture and the same bytes, with no archival, must still produce a row
// bound to the item. Without it a handler that refused EVERY item-bound
// upload would pass the refusal assertions vacuously.
func TestUpload_LiveItemStillSucceeds(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	slug, item := uploadItemFixture(t, srv, "UploadLiveControl")

	rr := uploadChannels(srv, slug, item.ID, "", realPNG())
	if rr.Code != 201 {
		t.Fatalf("upload against a live item: status = %d, want 201; body = %s",
			rr.Code, rr.Body.String())
	}
	if got := attachmentItemID(t, srv, uploadedAttachmentID(t, rr)); got != item.ID {
		t.Errorf("stored item_id = %q, want %q", got, item.ID)
	}
}

// itemWorkspaceID resolves an item's workspace, including after a soft delete.
func itemWorkspaceID(t *testing.T, srv *Server, itemID string) string {
	t.Helper()
	item, err := srv.store.GetItemIncludeDeleted(itemID)
	if err != nil || item == nil {
		t.Fatalf("GetItemIncludeDeleted(%s): item=%v err=%v", itemID, item, err)
	}
	return item.WorkspaceID
}
