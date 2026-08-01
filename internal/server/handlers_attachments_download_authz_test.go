package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/go-chi/chi/v5"
)

// Blob-read authorization tests for PLAN-2391 / TASK-2401 (BUG-2386).
//
// handleGetAttachment used to open with a flat requireMinRole("viewer").
// roleLevel("guest") is 0 — below viewer's 1 — so every grant-based guest
// was rejected before any item-level check ran, and inline images broke in
// items shared with them. The handler now authorizes per-attachment:
// item-bound rows go through workspace-identity → item visibility; orphans
// keep the flat viewer+ gate.
//
// Coverage here: the grant matrix (original + both variants × GET + HEAD),
// the non-disclosure 404s, the soft-deleted parent (DR-13), the foreign
// variant IDOR (DR-16), and the denial-path cache directive.

// putBlob writes body into the attachment registry and creates the matching
// row. Unlike the delete-authz fixtures, the read path actually streams the
// blob, so the bytes have to exist on disk.
func putBlob(t *testing.T, srv *Server, att *models.Attachment, body []byte) *models.Attachment {
	t.Helper()
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])

	st, err := srv.attachments.Resolve(attachments.FSPrefix + ":" + hash)
	if err != nil {
		t.Fatalf("resolve attachment store: %v", err)
	}
	key, err := st.Put(context.Background(), hash, "image/png", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("store.Put: %v", err)
	}

	att.StorageKey = key
	att.ContentHash = hash
	att.MimeType = "image/png"
	att.SizeBytes = int64(len(body))
	if att.UploadedBy == "" {
		att.UploadedBy = "system"
	}
	if att.Filename == "" {
		att.Filename = "blob.png"
	}
	if err := srv.store.CreateAttachment(att); err != nil {
		t.Fatalf("CreateAttachment(%s): %v", att.Filename, err)
	}
	return att
}

// downloadAuthzFixture is one workspace holding an item with an attachment
// (plus both derived variants, each with distinct bytes so a mis-served
// variant is detectable) and one orphan attachment.
type downloadAuthzFixture struct {
	wsID   string
	colID  string
	itemID string

	origID string
	smID   string
	mdID   string

	origBody []byte
	smBody   []byte
	mdBody   []byte

	orphanID   string
	orphanBody []byte
}

func newDownloadAuthzFixture(t *testing.T, srv *Server) downloadAuthzFixture {
	t.Helper()

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "DownloadAuthz"})
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
		Title: "Shared", Fields: `{}`,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	f := downloadAuthzFixture{
		wsID:       ws.ID,
		colID:      col.ID,
		itemID:     item.ID,
		origBody:   distinctPNG(t, 0x11),
		smBody:     distinctPNG(t, 0x22),
		mdBody:     distinctPNG(t, 0x33),
		orphanBody: distinctPNG(t, 0x44),
	}

	orig := putBlob(t, srv, &models.Attachment{
		WorkspaceID: ws.ID, ItemID: &item.ID, Filename: "orig.png",
	}, f.origBody)
	f.origID = orig.ID

	for _, v := range []struct {
		key  string
		body []byte
		out  *string
	}{
		{models.AttachmentVariantThumbSm, f.smBody, &f.smID},
		{models.AttachmentVariantThumbMd, f.mdBody, &f.mdID},
	} {
		variant := v.key
		row := putBlob(t, srv, &models.Attachment{
			WorkspaceID: ws.ID,
			ItemID:      &item.ID,
			ParentID:    &orig.ID,
			Variant:     &variant,
			Filename:    "orig-" + variant + ".png",
		}, v.body)
		*v.out = row.ID
	}

	orphan := putBlob(t, srv, &models.Attachment{
		WorkspaceID: ws.ID, Filename: "orphan.png",
	}, f.orphanBody)
	f.orphanID = orphan.ID

	return f
}

// distinctPNG returns a valid PNG whose bytes differ per seed, so an
// assertion on served content can tell one blob from another. The download
// handler never decodes, so trailing filler is fine — but the row still has
// to be a real image for MIME-driven disposition to behave.
func distinctPNG(t *testing.T, seed byte) []byte {
	t.Helper()
	out := append([]byte(nil), realPNG()...)
	return append(out, seed, seed, seed, seed)
}

// downloadAs issues GET or HEAD on the blob endpoint as a specific user +
// workspace role, bypassing the auth middleware the same way deleteAsUser
// does. variant "" means the original.
func downloadAs(srv *Server, method, wsID, attachmentID, variant string, user *models.User, role string) *httptest.ResponseRecorder {
	path := "/api/v1/workspaces/x/attachments/" + attachmentID
	if variant != "" {
		path += "?variant=" + variant
	}
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = "127.0.0.1:1234"

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("attachmentID", attachmentID)

	ctx := req.Context()
	ctx = context.WithValue(ctx, ctxResolvedWorkspaceID, wsID)
	ctx = context.WithValue(ctx, ctxCurrentUser, user)
	ctx = context.WithValue(ctx, ctxWorkspaceRole, role)
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	srv.handleGetAttachment(rr, req)
	return rr
}

// assertBlobServed checks a 200 whose body is want (GET) or whose headers say so
// (HEAD — no body on the wire, so Content-Type + the positive cache
// directive are all that can be asserted).
func assertBlobServed(t *testing.T, rr *httptest.ResponseRecorder, method string, want []byte, label string) {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("%s %s: status = %d, want 200; body = %s", method, label, rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "private, max-age=3600" {
		t.Errorf("%s %s: Cache-Control = %q, want the positive directive", method, label, got)
	}
	if method == http.MethodHead {
		return
	}
	if !bytes.Equal(rr.Body.Bytes(), want) {
		t.Errorf("%s %s: served %d bytes, want the %d-byte fixture blob",
			method, label, rr.Body.Len(), len(want))
	}
}

// assertBlobDenied pins the shared denial shape: 404, no-store, and the exact
// body every other denial writes.
func assertBlobDenied(t *testing.T, rr *httptest.ResponseRecorder, method, label string) {
	t.Helper()
	if rr.Code != http.StatusNotFound {
		t.Fatalf("%s %s: status = %d, want 404; body = %s", method, label, rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Errorf("%s %s: Cache-Control = %q, want \"private, no-store\" — an unlabelled "+
			"404 is heuristically cacheable by URL and would keep denying a caller "+
			"who later becomes authorized", method, label, got)
	}
}

// TestDownload_ItemGrantGuestCanFetch is the core BUG-2386 regression: a
// guest holding an item VIEW grant (no workspace viewer role) can fetch the
// attachment's bytes. Covers the full matrix the acceptance criteria name —
// original + both variants × GET + HEAD.
func TestDownload_ItemGrantGuestCanFetch(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	f := newDownloadAuthzFixture(t, srv)

	guest := mkUser(t, srv, "item-grant@test.com")
	if _, err := srv.store.CreateItemGrant(f.wsID, f.itemID, guest.ID, "view", guest.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		for _, tc := range []struct {
			label   string
			variant string
			want    []byte
		}{
			{"original", "", f.origBody},
			{"thumb-sm", models.AttachmentVariantThumbSm, f.smBody},
			{"thumb-md", models.AttachmentVariantThumbMd, f.mdBody},
		} {
			rr := downloadAs(srv, method, f.wsID, f.origID, tc.variant, guest, "guest")
			assertBlobServed(t, rr, method, tc.want, "item-grant guest "+tc.label)
		}
	}
}

// TestDownload_CollectionGrantGuestCanFetch — same, one level up the grant
// chain. The guest holds no item grant; visibility comes from the
// collection.
func TestDownload_CollectionGrantGuestCanFetch(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	f := newDownloadAuthzFixture(t, srv)

	guest := mkUser(t, srv, "coll-grant@test.com")
	if _, err := srv.store.CreateCollectionGrant(f.wsID, f.colID, guest.ID, "view", guest.ID); err != nil {
		t.Fatalf("CreateCollectionGrant: %v", err)
	}

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		for _, tc := range []struct {
			label   string
			variant string
			want    []byte
		}{
			{"original", "", f.origBody},
			{"thumb-sm", models.AttachmentVariantThumbSm, f.smBody},
			{"thumb-md", models.AttachmentVariantThumbMd, f.mdBody},
		} {
			rr := downloadAs(srv, method, f.wsID, f.origID, tc.variant, guest, "guest")
			assertBlobServed(t, rr, method, tc.want, "collection-grant guest "+tc.label)
		}
	}
}

// TestDownload_DirectVariantFetchHonorsTheGate — a derived row can be
// addressed by its OWN id, not just via ?variant= on the parent. That path
// must run the same gate (variants carry the parent's item_id, so it does).
func TestDownload_DirectVariantFetchHonorsTheGate(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	f := newDownloadAuthzFixture(t, srv)

	guest := mkUser(t, srv, "direct-variant@test.com")
	if _, err := srv.store.CreateItemGrant(f.wsID, f.itemID, guest.ID, "view", guest.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}
	stranger := mkUser(t, srv, "direct-variant-stranger@test.com")

	rr := downloadAs(srv, http.MethodGet, f.wsID, f.smID, "", guest, "guest")
	assertBlobServed(t, rr, http.MethodGet, f.smBody, "granted guest direct thumb-sm")

	rr = downloadAs(srv, http.MethodGet, f.wsID, f.smID, "", stranger, "guest")
	assertBlobDenied(t, rr, http.MethodGet, "ungranted guest direct thumb-sm")
}

// TestDownload_InvisibleItemIs404NotForbidden pins the non-disclosure
// posture AND the byte-identical-denial rule TASK-2400 established.
//
// The actor is an authenticated guest with no grants — NOT a non-member. A
// non-member is refused by RequireWorkspaceAccess with a 403 before this
// handler runs, so that fixture would pass vacuously and prove nothing about
// the handler's own ordering or its cache header.
func TestDownload_InvisibleItemIs404NotForbidden(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	f := newDownloadAuthzFixture(t, srv)

	stranger := mkUser(t, srv, "no-grants@test.com")

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		denied := downloadAs(srv, method, f.wsID, f.origID, "", stranger, "guest")
		if denied.Code == http.StatusForbidden {
			t.Fatalf("%s invisible item: got 403 — that confirms the attachment exists", method)
		}
		assertBlobDenied(t, denied, method, "invisible item")

		// Byte-identical to a plain lookup miss: a different error code or
		// message would turn the response into an existence oracle.
		missing := downloadAs(srv, method, f.wsID, "00000000-0000-0000-0000-000000000000", "", stranger, "guest")
		assertBlobDenied(t, missing, method, "nonexistent attachment")

		if got, want := denied.Body.String(), missing.Body.String(); got != want {
			t.Errorf("%s: visibility denial body = %q, lookup-miss body = %q — must be "+
				"byte-identical or the error distinguishes them", method, got, want)
		}
	}
}

// TestDownload_SoftDeletedParentIs404 — DR-13. The read path loads the
// parent with GetItem (not GetItemIncludeDeleted), so archiving an item
// takes its attachments' bytes offline. The DELETE path deliberately
// diverges; TestDeleteAttachment_* covers that side.
func TestDownload_SoftDeletedParentIs404(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	f := newDownloadAuthzFixture(t, srv)

	owner := mkUser(t, srv, "archive-owner@test.com")
	if err := srv.store.AddWorkspaceMember(f.wsID, owner.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}

	// Readable before the archive — otherwise the post-archive 404 could be
	// explained by any unrelated denial.
	rr := downloadAs(srv, http.MethodGet, f.wsID, f.origID, "", owner, "owner")
	assertBlobServed(t, rr, http.MethodGet, f.origBody, "owner pre-archive")

	if err := srv.store.DeleteItem(f.itemID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		for _, tc := range []struct{ label, variant string }{
			{"original", ""},
			{"thumb-sm", models.AttachmentVariantThumbSm},
			{"thumb-md", models.AttachmentVariantThumbMd},
		} {
			rr := downloadAs(srv, method, f.wsID, f.origID, tc.variant, owner, "owner")
			assertBlobDenied(t, rr, method, "soft-deleted parent "+tc.label)
		}
	}
}

// TestDownload_ForeignItemParentIs404 — attachments.item_id has no
// same-workspace constraint, so a row in workspace A can name an item in B.
// A view grant in B must not authorize reading A's bytes. The workspace
// identity check runs before visibility, so this is a 404 either way.
func TestDownload_ForeignItemParentIs404(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	b := newDownloadAuthzFixture(t, srv)

	wsA, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "VictimA"})
	if err != nil {
		t.Fatalf("CreateWorkspace A: %v", err)
	}
	secret := distinctPNG(t, 0x55)
	crossed := putBlob(t, srv, &models.Attachment{
		WorkspaceID: wsA.ID,
		ItemID:      &b.itemID, // foreign parent — the whole point
		Filename:    "crossed.png",
	}, secret)

	attacker := mkUser(t, srv, "cross-ws-read@test.com")
	// Full (unrestricted) VIEWER membership in A. Without it the caller is
	// restricted in A and checkItemVisible denies on its own, so the test
	// would pass with the workspace-identity guard deleted — it has to be
	// the guard, not the visibility filter, doing the work here.
	if err := srv.store.AddWorkspaceMember(wsA.ID, attacker.ID, "viewer"); err != nil {
		t.Fatalf("AddWorkspaceMember A: %v", err)
	}
	// ...plus a real view grant on the B item the crossed row names.
	if _, err := srv.store.CreateItemGrant(b.wsID, b.itemID, attacker.ID, "view", attacker.ID); err != nil {
		t.Fatalf("CreateItemGrant B: %v", err)
	}

	rr := downloadAs(srv, http.MethodGet, wsA.ID, crossed.ID, "", attacker, "viewer")
	if rr.Code == http.StatusOK {
		t.Fatalf("cross-workspace read succeeded: a grant in B must not authorize " +
			"reading an attachment in A")
	}
	assertBlobDenied(t, rr, http.MethodGet, "foreign parent item")
}

// TestDownload_ForeignVariantIsNotServed pins DR-16.
//
// GetAttachmentVariant used to scope on parent_id + variant + deleted_at
// only, so a variant row in ANOTHER workspace carrying this attachment's id
// as its parent would be served to a caller authorized for the parent.
//
// A 200 assertion proves nothing here: when the variant is absent the
// handler documents a fallback that serves the ORIGINAL, so the correct
// behaviour and the broken one both answer 200. The assertion has to be on
// the BYTES — the foreign child's content must never appear.
func TestDownload_ForeignVariantIsNotServed(t *testing.T) {
	srv, _ := testServerWithAttachments(t)

	// Workspace A: the caller is authorized for this original, which has no
	// local thumb-sm of its own.
	wsA, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "VariantVictim"})
	if err != nil {
		t.Fatalf("CreateWorkspace A: %v", err)
	}
	colA, err := srv.store.CreateCollection(wsA.ID, models.CollectionCreate{
		Name: "Tasks", Schema: `{"fields":[]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection A: %v", err)
	}
	itemA, err := srv.store.CreateItem(wsA.ID, colA.ID, models.ItemCreate{
		Title: "Local", Fields: `{}`,
	})
	if err != nil {
		t.Fatalf("CreateItem A: %v", err)
	}
	originalBody := distinctPNG(t, 0x66)
	original := putBlob(t, srv, &models.Attachment{
		WorkspaceID: wsA.ID, ItemID: &itemA.ID, Filename: "local.png",
	}, originalBody)

	// Workspace B: a planted variant whose parent_id points at A's original.
	wsB, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "VariantAttacker"})
	if err != nil {
		t.Fatalf("CreateWorkspace B: %v", err)
	}
	variant := models.AttachmentVariantThumbSm
	foreignBody := distinctPNG(t, 0x77)
	foreign := putBlob(t, srv, &models.Attachment{
		WorkspaceID: wsB.ID,
		ParentID:    &original.ID, // cross-workspace parent — representable today
		Variant:     &variant,
		Filename:    "foreign-thumb.png",
	}, foreignBody)

	reader := mkUser(t, srv, "variant-reader@test.com")
	if err := srv.store.AddWorkspaceMember(wsA.ID, reader.ID, "viewer"); err != nil {
		t.Fatalf("AddWorkspaceMember A: %v", err)
	}

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		rr := downloadAs(srv, method, wsA.ID, original.ID, variant, reader, "viewer")
		if rr.Code != http.StatusOK {
			t.Fatalf("%s ?variant=%s: status = %d, want 200 (fallback to the original); body = %s",
				method, variant, rr.Code, rr.Body.String())
		}
		if method == http.MethodHead {
			continue
		}
		if bytes.Equal(rr.Body.Bytes(), foreignBody) {
			t.Fatalf("%s ?variant=%s served workspace B's planted variant (%s) — "+
				"the variant lookup is not workspace-scoped", method, variant, foreign.ID)
		}
		if !bytes.Equal(rr.Body.Bytes(), originalBody) {
			t.Errorf("%s ?variant=%s: expected the fallback to A's original bytes, got %d bytes",
				method, variant, rr.Body.Len())
		}
	}
}

// TestDownload_OrphanKeepsFlatViewerGate — an orphan carries no item context
// to authorize against, so it keeps the flat viewer-role gate. A guest with a
// real grant on some other item must not reach it.
//
// The denial is a 404, matching every other denial in this handler. The gate
// is exactly as strong as before; only the response shape changed. It has to:
// the attachment row is loaded before the role check now, so a 403 would mean
// "this id names a live orphan here" while a bad id answers 404 — an
// existence oracle for orphan UUIDs that the old pre-lookup gate didn't have.
func TestDownload_OrphanKeepsFlatViewerGate(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	f := newDownloadAuthzFixture(t, srv)

	guest := mkUser(t, srv, "orphan-read@test.com")
	if _, err := srv.store.CreateItemGrant(f.wsID, f.itemID, guest.ID, "view", guest.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}

	rr := downloadAs(srv, http.MethodGet, f.wsID, f.orphanID, "", guest, "guest")
	if rr.Code == http.StatusOK {
		t.Fatalf("orphan read by a guest holding an unrelated item grant succeeded — " +
			"the flat viewer gate must not be weakened")
	}
	assertBlobDenied(t, rr, http.MethodGet, "orphan as grant-only guest")

	// ...and byte-identical to a plain lookup miss, so the denial can't be
	// used to confirm the orphan exists.
	missing := downloadAs(srv, http.MethodGet, f.wsID,
		"00000000-0000-0000-0000-000000000000", "", guest, "guest")
	assertBlobDenied(t, missing, http.MethodGet, "nonexistent attachment as guest")
	if got, want := rr.Body.String(), missing.Body.String(); got != want {
		t.Errorf("orphan denial body = %q, lookup-miss body = %q — must be byte-identical",
			got, want)
	}

	// ...and a real viewer still gets the bytes.
	viewer := mkUser(t, srv, "orphan-viewer@test.com")
	if err := srv.store.AddWorkspaceMember(f.wsID, viewer.ID, "viewer"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	rr = downloadAs(srv, http.MethodGet, f.wsID, f.orphanID, "", viewer, "viewer")
	assertBlobServed(t, rr, http.MethodGet, f.orphanBody, "orphan as viewer")
}

// TestDownload_ViewerStillReadsItemBoundAttachment — the gate replaced a
// role check with a visibility check; an ordinary unrestricted viewer must
// still be served.
func TestDownload_ViewerStillReadsItemBoundAttachment(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	f := newDownloadAuthzFixture(t, srv)

	viewer := mkUser(t, srv, "plain-viewer@test.com")
	if err := srv.store.AddWorkspaceMember(f.wsID, viewer.ID, "viewer"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}

	rr := downloadAs(srv, http.MethodGet, f.wsID, f.origID, "", viewer, "viewer")
	assertBlobServed(t, rr, http.MethodGet, f.origBody, "unrestricted viewer")
}

// TestDownload_CrossWorkspaceAttachmentIs404 — the pre-existing
// wrong-workspace guard, re-pinned with the cache directive the denial now
// carries.
func TestDownload_CrossWorkspaceAttachmentIs404(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	f := newDownloadAuthzFixture(t, srv)

	other, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Other"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	viewer := mkUser(t, srv, "other-ws-viewer@test.com")
	if err := srv.store.AddWorkspaceMember(other.ID, viewer.ID, "viewer"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}

	rr := downloadAs(srv, http.MethodGet, other.ID, f.origID, "", viewer, "viewer")
	assertBlobDenied(t, rr, http.MethodGet, "attachment in another workspace")
}
