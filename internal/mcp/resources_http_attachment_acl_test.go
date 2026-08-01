package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// The MCP image resource pad://workspace/{ws}/attachments/{id} is a SECOND
// byte path for attachments. It reaches the canonical blob handler through
// the same in-process handler chain the tool dispatcher uses, so PLAN-2391
// DR-10's item-visibility gate should cover it automatically — but the
// acceptance criteria require that be ASSERTED, not assumed.
//
// The existing resource tests (resources_test.go, resources_http_test.go)
// drive fake fetchers, which can't see a real ACL. These run the fetcher
// against a real *server.Server so the grant chain is genuinely exercised.

// attachmentACLFixture is a workspace with one item, one attachment bound to
// it, and a real blob on disk.
type attachmentACLFixture struct {
	srv    *server.Server
	st     *store.Store
	wsSlug string
	wsID   string
	itemID string
	attID  string
	body   []byte
}

func newAttachmentACLFixture(t *testing.T) attachmentACLFixture {
	t.Helper()

	srv, st := newPadServer(t)

	fs, err := attachments.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	reg := attachments.NewRegistry()
	reg.Register(attachments.FSPrefix, fs)
	srv.SetAttachments(reg, 0)

	ws, err := st.CreateWorkspace(models.WorkspaceCreate{Name: "MCP Attach ACL"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	col, err := st.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Schema: `{"fields":[]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	item, err := st.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "Shared", Fields: `{}`})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	// A real PNG (1x1 transparent) so the resource's image-MIME gate passes.
	body := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
		0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41,
		0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00,
		0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
		0x42, 0x60, 0x82,
	}
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	blobStore, err := reg.Resolve(attachments.FSPrefix + ":" + hash)
	if err != nil {
		t.Fatalf("resolve blob store: %v", err)
	}
	key, err := blobStore.Put(context.Background(), hash, "image/png", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("blob Put: %v", err)
	}

	att := &models.Attachment{
		WorkspaceID: ws.ID,
		ItemID:      &item.ID,
		UploadedBy:  "system",
		StorageKey:  key,
		ContentHash: hash,
		MimeType:    "image/png",
		SizeBytes:   int64(len(body)),
		Filename:    "shared.png",
	}
	if err := st.CreateAttachment(att); err != nil {
		t.Fatalf("CreateAttachment: %v", err)
	}

	return attachmentACLFixture{
		srv: srv, st: st,
		wsSlug: ws.Slug, wsID: ws.ID,
		itemID: item.ID, attID: att.ID,
		body: body,
	}
}

func (f attachmentACLFixture) fetcherFor(user *models.User) *HTTPResourceFetcher {
	return NewHTTPResourceFetcher(&HTTPHandlerDispatcher{
		Handler:      f.srv,
		UserResolver: fixedUserResolver(user),
	})
}

func (f attachmentACLFixture) resourceArgs() (show, download []string) {
	show = []string{
		"attachment", "show", f.attID,
		"--workspace", f.wsSlug,
		"--variant", attachmentResourceVariant,
		"--format", "json",
	}
	download = []string{
		"attachment", "download", f.attID, "-",
		"--workspace", f.wsSlug,
		"--variant", attachmentResourceVariant,
	}
	return show, download
}

func mkACLUser(t *testing.T, st *store.Store, email string) *models.User {
	t.Helper()
	u, err := st.CreateUser(models.UserCreate{
		Email: email, Name: email,
		Password: "correct-horse-battery-staple",
		Role:     "member",
	})
	if err != nil {
		t.Fatalf("CreateUser %s: %v", email, err)
	}
	return u
}

// TestMCPAttachmentResource_GrantGuestCanRead asserts the DR-10 gate reaches
// the MCP image resource in the permissive direction: a guest holding an
// item grant — who the old flat requireMinRole("viewer") rejected outright —
// gets both the metadata and the bytes.
func TestMCPAttachmentResource_GrantGuestCanRead(t *testing.T) {
	f := newAttachmentACLFixture(t)
	guest := mkACLUser(t, f.st, "mcp-item-grant@test.com")
	if _, err := f.st.CreateItemGrant(f.wsID, f.itemID, guest.ID, "view", guest.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}

	fetcher := f.fetcherFor(guest)
	show, download := f.resourceArgs()

	metadata, err := fetcher.Fetch(context.Background(), show)
	if err != nil {
		t.Fatalf("item-grant guest metadata fetch: %v", err)
	}
	if !strings.Contains(metadata, "image/png") {
		t.Errorf("metadata = %s, want an image/png MIME", metadata)
	}

	got, err := fetcher.FetchBytes(context.Background(), download)
	if err != nil {
		t.Fatalf("item-grant guest byte fetch: %v", err)
	}
	// No thumb-md row exists, so the handler's documented fallback serves
	// the original — the bytes still have to be THIS attachment's.
	if !bytes.Equal(got, f.body) {
		t.Errorf("served %d bytes, want the %d-byte fixture blob", len(got), len(f.body))
	}
}

// TestMCPAttachmentResource_CollectionGrantGuestCanRead — same, one level up
// the grant chain.
func TestMCPAttachmentResource_CollectionGrantGuestCanRead(t *testing.T) {
	f := newAttachmentACLFixture(t)
	guest := mkACLUser(t, f.st, "mcp-coll-grant@test.com")

	item, err := f.st.GetItem(f.itemID)
	if err != nil || item == nil {
		t.Fatalf("GetItem: %v (item=%v)", err, item)
	}
	if _, err := f.st.CreateCollectionGrant(f.wsID, item.CollectionID, guest.ID, "view", guest.ID); err != nil {
		t.Fatalf("CreateCollectionGrant: %v", err)
	}

	_, download := f.resourceArgs()
	got, err := f.fetcherFor(guest).FetchBytes(context.Background(), download)
	if err != nil {
		t.Fatalf("collection-grant guest byte fetch: %v", err)
	}
	if !bytes.Equal(got, f.body) {
		t.Errorf("served %d bytes, want the %d-byte fixture blob", len(got), len(f.body))
	}
}

// TestMCPAttachmentResource_UngrantedUserIsDenied asserts the gate in the
// restrictive direction — the half that actually matters for security.
//
// The caller is deliberately REACHABLE: a guest holding a grant on a
// different item in the same workspace, so RequireWorkspaceAccess admits
// them and the blob handler's own item-visibility check is what denies. A
// plain non-member would be turned away by middleware first and the test
// would pass without exercising the gate at all.
func TestMCPAttachmentResource_UngrantedUserIsDenied(t *testing.T) {
	f := newAttachmentACLFixture(t)
	stranger := mkACLUser(t, f.st, "mcp-stranger@test.com")

	item, err := f.st.GetItem(f.itemID)
	if err != nil || item == nil {
		t.Fatalf("GetItem: %v (item=%v)", err, item)
	}
	decoy, err := f.st.CreateItem(f.wsID, item.CollectionID, models.ItemCreate{
		Title: "Decoy", Fields: `{}`,
	})
	if err != nil {
		t.Fatalf("CreateItem decoy: %v", err)
	}
	if _, err := f.st.CreateItemGrant(f.wsID, decoy.ID, stranger.ID, "view", stranger.ID); err != nil {
		t.Fatalf("CreateItemGrant decoy: %v", err)
	}

	fetcher := f.fetcherFor(stranger)
	show, download := f.resourceArgs()

	if metadata, err := fetcher.Fetch(context.Background(), show); err == nil {
		t.Errorf("ungranted metadata fetch succeeded: %s", metadata)
	} else if !strings.Contains(err.Error(), "HTTP 404") {
		// 404, not 403: a 403 would mean middleware turned the caller away
		// before the handler ran (making the test vacuous), or that the
		// handler disclosed the attachment's existence.
		t.Errorf("ungranted metadata fetch error = %v, want an HTTP 404 from the blob handler", err)
	}

	got, err := fetcher.FetchBytes(context.Background(), download)
	if err == nil {
		t.Fatalf("ungranted byte fetch succeeded, returned %d bytes", len(got))
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("ungranted byte fetch error = %v, want an HTTP 404 from the blob handler", err)
	}
	if bytes.Equal(got, f.body) {
		t.Fatal("ungranted byte fetch returned the fixture blob despite the error")
	}
}

// TestMCPAttachmentResource_SoftDeletedParentIsDenied — DR-13 reaches this
// surface too: archiving the parent takes the resource offline for a caller
// who could read it a moment earlier.
//
// The reader is a full workspace VIEWER, not a grant-only guest, so
// RequireWorkspaceAccess admits them identically before and after the
// archive. That isolates the blob handler's GetItem lookup as the only thing
// that changes between the two fetches.
func TestMCPAttachmentResource_SoftDeletedParentIsDenied(t *testing.T) {
	f := newAttachmentACLFixture(t)
	viewer := mkACLUser(t, f.st, "mcp-archived@test.com")
	if err := f.st.AddWorkspaceMember(f.wsID, viewer.ID, "viewer"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}

	fetcher := f.fetcherFor(viewer)
	_, download := f.resourceArgs()

	if _, err := fetcher.FetchBytes(context.Background(), download); err != nil {
		t.Fatalf("pre-archive byte fetch: %v", err)
	}
	if err := f.st.DeleteItem(f.itemID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	got, err := fetcher.FetchBytes(context.Background(), download)
	if err == nil {
		t.Fatalf("post-archive byte fetch succeeded, returned %d bytes", len(got))
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("post-archive byte fetch error = %v, want an HTTP 404 from the blob handler", err)
	}
}
