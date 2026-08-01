//go:build !libvips

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// Transform authorization tests for PLAN-2391 / TASK-2402.
//
// handleTransformAttachment used to open with a flat requireMinRole("editor")
// and never look at the attachment's parent item, so a restricted editor
// could transform an attachment on an item they cannot see — reading the
// source blob and getting output metadata plus a new row back. It also
// checked nothing about the parent at insert time, so an item archived
// mid-transform still got a quota-counted live attachment whose bytes DR-13
// then refuses to serve.
//
// Coverage: the visibility gate and its byte-identical denials, the foreign
// and malformed parents, the archived parent (sequential AND raced), the
// grant-based editor who must still be able to transform, and the orphan
// row's flat gate.

// transformFixture is one workspace with a hidden collection + item + a
// transformable attachment, a second visible collection to make restricted
// access meaningful, and an orphan attachment.
type transformFixture struct {
	wsID string

	hiddenColID  string
	visibleColID string
	itemID       string

	attID    string
	orphanID string
}

func newTransformFixture(t *testing.T, srv *Server) transformFixture {
	t.Helper()

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "TransformAuthz"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	hidden, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Secrets", Schema: `{"fields":[]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection hidden: %v", err)
	}
	visible, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Schema: `{"fields":[]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection visible: %v", err)
	}
	item, err := srv.store.CreateItem(ws.ID, hidden.ID, models.ItemCreate{
		Title: "Hidden", Fields: `{}`,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	f := transformFixture{
		wsID:         ws.ID,
		hiddenColID:  hidden.ID,
		visibleColID: visible.ID,
		itemID:       item.ID,
	}
	f.attID = putBlob(t, srv, &models.Attachment{
		WorkspaceID: ws.ID, ItemID: &item.ID, Filename: "shot.png",
	}, makeIntegrationPNG(t, 20, 10)).ID
	f.orphanID = putBlob(t, srv, &models.Attachment{
		WorkspaceID: ws.ID, Filename: "orphan.png",
	}, makeIntegrationPNG(t, 20, 10)).ID
	return f
}

// transformAs POSTs a rotate as a specific user + workspace role, bypassing
// the auth middleware the same way downloadAs does.
func transformAs(srv *Server, wsID, attachmentID string, user *models.User, role string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(transformRequestBody{Operation: "rotate", Degrees: 90})
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/workspaces/x/attachments/"+attachmentID+"/transform", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:1234"

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("slug", "x")
	rctx.URLParams.Add("attachmentID", attachmentID)

	ctx := req.Context()
	ctx = context.WithValue(ctx, ctxResolvedWorkspaceID, wsID)
	ctx = context.WithValue(ctx, ctxCurrentUser, user)
	ctx = context.WithValue(ctx, ctxWorkspaceRole, role)
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	srv.handleTransformAttachment(rr, req)
	return rr
}

// assertTransformDenied pins the shared denial: 404 with the exact body every
// other attachment denial writes.
func assertTransformDenied(t *testing.T, rr *httptest.ResponseRecorder, label string) {
	t.Helper()
	if rr.Code == http.StatusForbidden {
		t.Fatalf("%s: got 403 — that confirms the attachment exists", label)
	}
	if rr.Code != http.StatusNotFound {
		t.Fatalf("%s: status = %d, want 404; body = %s", label, rr.Code, rr.Body.String())
	}
}

// liveAttachmentCount counts the workspace's live, non-derived attachment
// rows — the quota-counted surface a refused transform must not grow.
func liveAttachmentCount(t *testing.T, srv *Server, wsID string) int {
	t.Helper()
	_, total, err := srv.store.WorkspaceAttachments(wsID, store.AttachmentListFilters{})
	if err != nil {
		t.Fatalf("WorkspaceAttachments: %v", err)
	}
	return total
}

// TestTransform_InvisibleItemIs404 is the core TASK-2402 regression: a
// workspace EDITOR whose collection access excludes the parent item must not
// be able to transform its attachment, and the denial must be
// indistinguishable from a plain lookup miss.
//
// The actor is a full editor, not a guest — a guest would be denied by any
// role gate, so the test would pass with the visibility check deleted and
// prove nothing.
func TestTransform_InvisibleItemIs404(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	defer srv.Stop()
	f := newTransformFixture(t, srv)

	restricted := mkUser(t, srv, "restricted-editor@test.com")
	if err := srv.store.AddWorkspaceMember(f.wsID, restricted.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	// Access to the OTHER collection only — the parent item is invisible.
	if err := srv.store.SetMemberCollectionAccess(f.wsID, restricted.ID, "specific",
		[]string{f.visibleColID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}

	before := liveAttachmentCount(t, srv, f.wsID)

	denied := transformAs(srv, f.wsID, f.attID, restricted, "editor")
	assertTransformDenied(t, denied, "restricted editor, invisible item")

	// Byte-identical to a lookup miss: a distinguishable code or message
	// would turn the response into an existence oracle for attachment ids.
	missing := transformAs(srv, f.wsID, "00000000-0000-0000-0000-000000000000", restricted, "editor")
	assertTransformDenied(t, missing, "nonexistent attachment")
	if got, want := denied.Body.String(), missing.Body.String(); got != want {
		t.Errorf("visibility denial body = %q, lookup-miss body = %q — must be byte-identical", got, want)
	}

	if after := liveAttachmentCount(t, srv, f.wsID); after != before {
		t.Errorf("live attachment rows = %d, want %d — the refused transform still wrote a row", after, before)
	}
}

// TestTransform_UnrestrictedEditorStillWorks — the gate replaced a role check
// with visibility + edit permission; the ordinary case must be unaffected.
func TestTransform_UnrestrictedEditorStillWorks(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	defer srv.Stop()
	f := newTransformFixture(t, srv)

	editor := mkUser(t, srv, "plain-editor@test.com")
	if err := srv.store.AddWorkspaceMember(f.wsID, editor.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}

	rr := transformAs(srv, f.wsID, f.attID, editor, "editor")
	if rr.Code != http.StatusCreated {
		t.Fatalf("unrestricted editor transform: status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

// TestTransform_ItemGrantEditorCanTransform pins the deliberate widening: the
// gate is requireEditPermission, not the flat editor role, so a guest holding
// an item EDIT grant — who can already attach to that item (BUG-1661) — can
// rotate what they uploaded. A view-only grant must not be enough.
func TestTransform_ItemGrantEditorCanTransform(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	defer srv.Stop()
	f := newTransformFixture(t, srv)

	viewer := mkUser(t, srv, "grant-viewer@test.com")
	if _, err := srv.store.CreateItemGrant(f.wsID, f.itemID, viewer.ID, "view", viewer.ID); err != nil {
		t.Fatalf("CreateItemGrant view: %v", err)
	}
	rr := transformAs(srv, f.wsID, f.attID, viewer, "guest")
	if rr.Code != http.StatusForbidden {
		t.Errorf("view-grant guest: status = %d, want 403 (visible but not editable)", rr.Code)
	}

	editor := mkUser(t, srv, "grant-editor@test.com")
	if _, err := srv.store.CreateItemGrant(f.wsID, f.itemID, editor.ID, "edit", editor.ID); err != nil {
		t.Fatalf("CreateItemGrant edit: %v", err)
	}
	rr = transformAs(srv, f.wsID, f.attID, editor, "guest")
	if rr.Code != http.StatusCreated {
		t.Fatalf("edit-grant guest: status = %d, want 201; body = %s", rr.Code, rr.Body.String())
	}
}

// TestTransform_ForeignItemParentIs404 — attachments.item_id has no
// same-workspace constraint, so a row in workspace A can name an item in B. A
// grant in B must not authorize transforming A's bytes.
func TestTransform_ForeignItemParentIs404(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	defer srv.Stop()
	b := newTransformFixture(t, srv)

	wsA, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "TransformVictim"})
	if err != nil {
		t.Fatalf("CreateWorkspace A: %v", err)
	}
	crossed := putBlob(t, srv, &models.Attachment{
		WorkspaceID: wsA.ID,
		ItemID:      &b.itemID, // foreign parent — the whole point
		Filename:    "crossed.png",
	}, makeIntegrationPNG(t, 20, 10))

	attacker := mkUser(t, srv, "cross-ws-transform@test.com")
	// Full, UNRESTRICTED editor in A: without this the caller is restricted
	// in A and checkItemVisible denies on its own, so the test would pass
	// with the workspace-identity guard deleted.
	if err := srv.store.AddWorkspaceMember(wsA.ID, attacker.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember A: %v", err)
	}
	// ...plus a real edit grant on the B item the crossed row names.
	if _, err := srv.store.CreateItemGrant(b.wsID, b.itemID, attacker.ID, "edit", attacker.ID); err != nil {
		t.Fatalf("CreateItemGrant B: %v", err)
	}

	rr := transformAs(srv, wsA.ID, crossed.ID, attacker, "editor")
	assertTransformDenied(t, rr, "foreign parent item")
}

// TestTransform_MalformedParentIs404 — a non-null item_id that resolves to
// nothing at all is rejected too, not silently treated as an orphan.
func TestTransform_MalformedParentIs404(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	defer srv.Stop()
	f := newTransformFixture(t, srv)

	ghost := "00000000-0000-0000-0000-0000000000ff"
	malformed := putBlob(t, srv, &models.Attachment{
		WorkspaceID: f.wsID, ItemID: &ghost, Filename: "malformed.png",
	}, makeIntegrationPNG(t, 20, 10))

	owner := mkUser(t, srv, "malformed-owner@test.com")
	if err := srv.store.AddWorkspaceMember(f.wsID, owner.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}

	before := liveAttachmentCount(t, srv, f.wsID)
	rr := transformAs(srv, f.wsID, malformed.ID, owner, "owner")
	assertTransformDenied(t, rr, "malformed non-null parent")
	if after := liveAttachmentCount(t, srv, f.wsID); after != before {
		t.Errorf("live attachment rows = %d, want %d", after, before)
	}
}

// TestTransform_SoftDeletedParentIs404 — DR-14's sequential half. Archiving
// the item takes its attachments out of the transform surface, so no quota is
// burned producing bytes DR-13 refuses to serve.
func TestTransform_SoftDeletedParentIs404(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	defer srv.Stop()
	f := newTransformFixture(t, srv)

	owner := mkUser(t, srv, "transform-archive-owner@test.com")
	if err := srv.store.AddWorkspaceMember(f.wsID, owner.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}

	// Transformable before the archive, so the post-archive 404 can't be
	// explained by an unrelated denial.
	rr := transformAs(srv, f.wsID, f.attID, owner, "owner")
	if rr.Code != http.StatusCreated {
		t.Fatalf("pre-archive transform: status = %d, body = %s", rr.Code, rr.Body.String())
	}

	if err := srv.store.DeleteItem(f.itemID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}

	before := liveAttachmentCount(t, srv, f.wsID)
	rr = transformAs(srv, f.wsID, f.attID, owner, "owner")
	assertTransformDenied(t, rr, "soft-deleted parent")
	if after := liveAttachmentCount(t, srv, f.wsID); after != before {
		t.Errorf("live attachment rows = %d, want %d", after, before)
	}
}

// hookedProcessor runs fn inside Encode, once, and ONLY while armed. Encode
// sits between the handler's up-front parent check and its insert, so an armed
// hook is a deterministic harness for the check-then-work window DR-14
// describes.
//
// Arming is not ceremony. The fixture's upload spawns background thumbnail
// derivation, which calls Encode too — an unconditional sync.Once fires on
// whichever Encode wins, and under load that is the derivation goroutine. The
// hook then archives the item on a thread the test is not synchronised with,
// leaving the transform to run start-to-finish against a still-live row: a
// 201, a green SQLite run, and an intermittent Postgres failure whose message
// points at the production fix rather than at the harness. Arm immediately
// before the request under test so only the transform's own Encode can fire.
type hookedProcessor struct {
	attachments.Processor
	mu    sync.Mutex
	armed bool
	fired bool
	fn    func()
}

// arm allows the next Encode to fire fn. Call it directly before the request
// whose window is under test.
func (p *hookedProcessor) arm() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.armed = true
}

// didFire reports whether fn ran, under the same lock that sets it, so the
// assertion is not a data race against a background derivation goroutine.
func (p *hookedProcessor) didFire() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fired
}

func (p *hookedProcessor) Encode(img image.Image, format string, w io.Writer) error {
	p.mu.Lock()
	run := p.armed && !p.fired
	if run {
		p.fired = true
	}
	p.mu.Unlock()
	if run {
		p.fn()
	}
	return p.Processor.Encode(img, format, w)
}

// TestTransform_ParentArchivedMidFlightWritesNoRow is the raced half of
// DR-14, and the reason the insert takes an item lock rather than trusting
// the up-front check.
//
// The item is archived DURING the transform — after the handler validated the
// parent, before it inserts. Without the re-check inside the write, the
// handler inserts a live, quota-counted attachment row hanging off an already
// archived item, whose bytes the read gate then refuses to serve.
func TestTransform_ParentArchivedMidFlightWritesNoRow(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	defer srv.Stop()
	f := newTransformFixture(t, srv)

	owner := mkUser(t, srv, "midflight-owner@test.com")
	if err := srv.store.AddWorkspaceMember(f.wsID, owner.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}

	var archiveMu sync.Mutex
	var archiveErr error
	hook := &hookedProcessor{
		Processor: attachments.NewProcessor(),
		fn: func() {
			archiveMu.Lock()
			defer archiveMu.Unlock()
			archiveErr = srv.store.DeleteItem(f.itemID)
		},
	}
	srv.SetImageProcessor(hook)

	before := liveAttachmentCount(t, srv, f.wsID)
	// Arm only now, so the fixture's background thumbnail derivation cannot
	// consume the hook and archive the item on a thread this test is not
	// synchronised with. fn runs synchronously inside the transform's own
	// Encode, so archiveErr is settled by the time the request returns.
	hook.arm()
	rr := transformAs(srv, f.wsID, f.attID, owner, "owner")
	archiveMu.Lock()
	deleteErr := archiveErr
	archiveMu.Unlock()
	if deleteErr != nil {
		t.Fatalf("mid-flight DeleteItem: %v", deleteErr)
	}
	// Without this the test passes vacuously: any earlier 404 — a broken
	// fixture, a denial from the gate above — would satisfy the assertions
	// below while the mid-flight window was never entered at all.
	if !hook.didFire() {
		t.Fatal("the archive hook never ran — the request was refused before Encode, " +
			"so this exercised the up-front check rather than the mid-flight window")
	}
	assertTransformDenied(t, rr, "parent archived mid-transform")

	if after := liveAttachmentCount(t, srv, f.wsID); after != before {
		t.Errorf("live attachment rows = %d, want %d — the transform inserted a row "+
			"against an item archived while it ran", after, before)
	}
}

// TestTransform_OrphanKeepsFlatEditorGate — an orphan carries no item context
// to authorize against, so it keeps the flat workspace editor gate.
//
// A caller below viewer gets the shared 404, not a 403: the row is loaded
// before the role check now, so a 403 would confirm that this id names a live
// orphan here while a bad id answers 404. A viewer, who can already READ
// orphans, gets the honest 403.
func TestTransform_OrphanKeepsFlatEditorGate(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	defer srv.Stop()
	f := newTransformFixture(t, srv)

	guest := mkUser(t, srv, "orphan-transform-guest@test.com")
	if _, err := srv.store.CreateItemGrant(f.wsID, f.itemID, guest.ID, "edit", guest.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}
	denied := transformAs(srv, f.wsID, f.orphanID, guest, "guest")
	assertTransformDenied(t, denied, "orphan as grant-only guest")
	missing := transformAs(srv, f.wsID, "00000000-0000-0000-0000-000000000000", guest, "guest")
	assertTransformDenied(t, missing, "nonexistent attachment as guest")
	if got, want := denied.Body.String(), missing.Body.String(); got != want {
		t.Errorf("orphan denial body = %q, lookup-miss body = %q — must be byte-identical", got, want)
	}

	viewer := mkUser(t, srv, "orphan-transform-viewer@test.com")
	if err := srv.store.AddWorkspaceMember(f.wsID, viewer.ID, "viewer"); err != nil {
		t.Fatalf("AddWorkspaceMember viewer: %v", err)
	}
	if rr := transformAs(srv, f.wsID, f.orphanID, viewer, "viewer"); rr.Code != http.StatusForbidden {
		t.Errorf("orphan as viewer: status = %d, want 403", rr.Code)
	}

	// A RESTRICTED editor is refused with the shared 404, matching the
	// DELETE path: the storage listing hides orphans from them, so the
	// transform must not confirm one exists.
	restricted := mkUser(t, srv, "orphan-transform-restricted@test.com")
	if err := srv.store.AddWorkspaceMember(f.wsID, restricted.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember restricted: %v", err)
	}
	if err := srv.store.SetMemberCollectionAccess(f.wsID, restricted.ID, "specific",
		[]string{f.visibleColID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}
	rr := transformAs(srv, f.wsID, f.orphanID, restricted, "editor")
	assertTransformDenied(t, rr, "orphan as restricted editor")
	if got, want := rr.Body.String(), missing.Body.String(); got != want {
		t.Errorf("restricted-editor orphan denial body = %q, lookup-miss body = %q — "+
			"must be byte-identical", got, want)
	}

	editor := mkUser(t, srv, "orphan-transform-editor@test.com")
	if err := srv.store.AddWorkspaceMember(f.wsID, editor.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember editor: %v", err)
	}
	if rr := transformAs(srv, f.wsID, f.orphanID, editor, "editor"); rr.Code != http.StatusCreated {
		t.Fatalf("orphan as editor: status = %d, body = %s", rr.Code, rr.Body.String())
	}
}
