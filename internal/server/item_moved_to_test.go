package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Tests for the archived-source "moved to" pointer (PLAN-2357 / TASK-2359).
//
// The gate is the subject, not the decoration: nearly every case below asserts
// that a destination is WITHHELD, and the headline assertion
// (TestMovedTo_DeniedResponseIsByteIdenticalToNoMoveRecord) compares raw
// response BYTES rather than a parsed struct, because a structurally
// distinguishable response is itself the leak this task exists to prevent.

// --- fixture -----------------------------------------------------------

type movedToFixture struct {
	t   *testing.T
	srv *Server

	owner *models.User

	// A is the source workspace; B and C are destinations.
	wsA, wsB, wsC *models.Workspace

	collA  *models.Collection
	source *models.Item

	collB   *models.Collection
	destB   *models.Item
	hiddenB *models.Collection
	destHid *models.Item

	collC *models.Collection
	destC *models.Item
}

func newMovedToFixture(t *testing.T) *movedToFixture {
	t.Helper()
	srv := testServer(t)

	owner := mustUser(t, srv, "movedto-owner@example.com", "movedtoowner", "")
	wsA := mustWorkspace(t, srv, "Source WS", owner.ID)
	wsB := mustWorkspace(t, srv, "Dest WS", owner.ID)
	wsC := mustWorkspace(t, srv, "Other Dest WS", owner.ID)

	collA := mustCollection(t, srv, wsA.ID, "Tasks A")
	source := mustItem(t, srv, wsA.ID, collA.ID, "The Source Item")

	collB := mustCollection(t, srv, wsB.ID, "Tasks B")
	destB := mustItem(t, srv, wsB.ID, collB.ID, "The Copy In B")
	hiddenB := mustCollection(t, srv, wsB.ID, "Secrets B")
	destHid := mustItem(t, srv, wsB.ID, hiddenB.ID, "The Copy In Hidden B")

	collC := mustCollection(t, srv, wsC.ID, "Tasks C")
	destC := mustItem(t, srv, wsC.ID, collC.ID, "The Copy In C")

	return &movedToFixture{
		t: t, srv: srv, owner: owner,
		wsA: wsA, wsB: wsB, wsC: wsC,
		collA: collA, source: source,
		collB: collB, destB: destB, hiddenB: hiddenB, destHid: destHid,
		collC: collC, destC: destC,
	}
}

// record commits one provenance row. archivedSource=true makes it a MOVE
// (source_seq is then mandatory), false a plain copy.
func (f *movedToFixture) record(target *models.Item, archivedSource bool, seq int64) *models.ItemWorkspaceMove {
	f.t.Helper()
	m := models.ItemWorkspaceMove{
		SourceWorkspaceID: f.source.WorkspaceID,
		SourceItemID:      f.source.ID,
		TargetWorkspaceID: target.WorkspaceID,
		TargetItemID:      target.ID,
		ArchivedSource:    archivedSource,
		CreatedBy:         f.owner.ID,
	}
	if archivedSource {
		m.SourceSeq = &seq
	}
	tx, err := f.srv.store.DB().Begin()
	if err != nil {
		f.t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck // committed below; rollback is the failure path
	stored, err := f.srv.store.RecordItemWorkspaceMoveTx(tx, m)
	if err != nil {
		f.t.Fatalf("RecordItemWorkspaceMoveTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		f.t.Fatalf("commit: %v", err)
	}
	return stored
}

func (f *movedToFixture) archiveSource() {
	f.t.Helper()
	if err := f.srv.store.DeleteItem(f.source.ID); err != nil {
		f.t.Fatalf("DeleteItem(source): %v", err)
	}
}

func (f *movedToFixture) restoreSource() {
	f.t.Helper()
	if _, err := f.srv.store.RestoreItem(f.source.ID); err != nil {
		f.t.Fatalf("RestoreItem(source): %v", err)
	}
}

// getSource performs GET on the source item exactly as the router would and
// requires a 200. Use rawGetSource when the status is what is under test.
func (f *movedToFixture) getSource(user *models.User, o reqOpts) *httptest.ResponseRecorder {
	f.t.Helper()
	rr := f.rawGetSource(user, o)
	if rr.Code != http.StatusOK {
		f.t.Fatalf("GET source: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	return rr
}

func (f *movedToFixture) rawGetSource(user *models.User, o reqOpts) *httptest.ResponseRecorder {
	f.t.Helper()
	rr := httptest.NewRecorder()
	f.srv.handleGetItem(rr, f.getSourceRequest(user, o))
	return rr
}

// getSourceRequest builds the GET request the router would hand handleGetItem,
// with the caller's auth surface described by o. The workspace-A role and
// resolved ID are stashed the way RequireWorkspaceAccess stashes them; the
// cross-workspace gate must ignore both.
func (f *movedToFixture) getSourceRequest(user *models.User, o reqOpts) *http.Request {
	f.t.Helper()

	r := httptest.NewRequest("GET",
		"/api/v1/workspaces/"+f.wsA.Slug+"/items/"+f.source.Slug, nil)
	ctx := r.Context()
	if !o.noUser && user != nil {
		ctx = WithCurrentUser(ctx, user)
	}
	if o.setAllowed {
		ctx = WithTokenAllowedWorkspaces(ctx, o.allowed)
	}
	if o.tokenWorkspaceID != "" {
		ctx = WithTokenWorkspaceID(ctx, o.tokenWorkspaceID)
	}
	role := o.wsRoleCtx
	if role == "" {
		role = "owner"
	}
	ctx = contextWithWorkspaceRoleForTest(ctx, role)
	ctx = contextWithResolvedWorkspaceIDForTest(ctx, f.wsA.ID)

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("slug", f.wsA.Slug)
	rctx.URLParams.Add("itemSlug", f.source.Slug)
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)

	r = r.WithContext(ctx)
	if o.bearer {
		r.Header.Set("Authorization", "Bearer test-token")
	}
	return r
}

// movedTo parses the response and returns the moved_to block. It also asserts
// the disclosure rule's negative half: when the block is absent the KEY must
// be absent — never `"moved_to": null` and never `"moved_to": []`.
func (f *movedToFixture) movedTo(rr *httptest.ResponseRecorder) []models.ItemMovedTo {
	f.t.Helper()

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		f.t.Fatalf("parse response: %v\nbody: %s", err, rr.Body.String())
	}
	blob, present := raw["moved_to"]
	if !present {
		return nil
	}
	var out []models.ItemMovedTo
	if err := json.Unmarshal(blob, &out); err != nil {
		f.t.Fatalf("parse moved_to: %v", err)
	}
	if len(out) == 0 {
		f.t.Fatalf("moved_to key present but empty (%s) — an empty marker is itself a disclosure; the key must be omitted", string(blob))
	}
	return out
}

func (f *movedToFixture) requireNoPointer(rr *httptest.ResponseRecorder, label string) {
	f.t.Helper()
	if got := f.movedTo(rr); got != nil {
		f.t.Fatalf("%s: expected NO moved_to block, got %+v", label, got)
	}
}

// --- happy path --------------------------------------------------------

// TestMovedTo_ArchivedSourceCallerCanReadDestination is the affirmative case:
// the destination is named in displayable terms, with no UUIDs, so the
// consumer can render a link without a second call.
func TestMovedTo_ArchivedSourceCallerCanReadDestination(t *testing.T) {
	f := newMovedToFixture(t)
	f.record(f.destB, true, 10)
	f.archiveSource()

	got := f.movedTo(f.getSource(f.owner, reqOpts{}))
	if len(got) != 1 {
		t.Fatalf("expected 1 destination, got %d (%+v)", len(got), got)
	}
	d := got[0]
	if d.WorkspaceSlug != f.wsB.Slug {
		t.Errorf("workspace_slug: got %q, want %q", d.WorkspaceSlug, f.wsB.Slug)
	}
	if d.Ref != f.destB.Ref || d.Ref == "" {
		t.Errorf("ref: got %q, want %q", d.Ref, f.destB.Ref)
	}
	if d.ItemSlug != f.destB.Slug {
		t.Errorf("item_slug: got %q, want %q", d.ItemSlug, f.destB.Slug)
	}
	if d.Title != f.destB.Title {
		t.Errorf("title: got %q, want %q", d.Title, f.destB.Title)
	}
	if d.CollectionSlug != f.destB.CollectionSlug {
		t.Errorf("collection_slug: got %q, want %q", d.CollectionSlug, f.destB.CollectionSlug)
	}
	if d.MovedAt == "" {
		t.Error("moved_at empty")
	}

	// No UUIDs anywhere in the block: the whole point of the displayable
	// shape is that internal IDs of another workspace never travel.
	blob, _ := json.Marshal(d)
	for label, id := range map[string]string{
		"destination workspace ID": f.wsB.ID,
		"destination item ID":      f.destB.ID,
		"destination collection":   f.collB.ID,
	} {
		if id != "" && strings.Contains(string(blob), id) {
			t.Errorf("moved_to leaked the %s (%s): %s", label, id, blob)
		}
	}
}

// --- the disclosure rule ----------------------------------------------

// TestMovedTo_DeniedResponseIsByteIdenticalToNoMoveRecord is the acceptance
// criterion the whole task hangs on. A caller who cannot read the destination
// must not be able to tell a moved item from an ordinary archived one — so the
// comparison is on RAW BYTES, taken from the same caller and the same item,
// before and after the provenance row exists. Writing the row touches nothing
// on the item itself (no seq bump, no updated_at), so any difference between
// the two bodies is attributable to the pointer alone. A null, an empty array,
// a reordered key set or a differently-typed field would all fail here.
func TestMovedTo_DeniedResponseIsByteIdenticalToNoMoveRecord(t *testing.T) {
	f := newMovedToFixture(t)
	// Member of A only — a total stranger to B.
	stranger := mustUser(t, f.srv, "stranger@example.com", "strangeruser", "")
	if err := f.srv.store.AddWorkspaceMember(f.wsA.ID, stranger.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember A: %v", err)
	}

	f.archiveSource()
	withoutRow := f.getSource(stranger, reqOpts{wsRoleCtx: "editor"}).Body.Bytes()

	f.record(f.destB, true, 10)
	withRow := f.getSource(stranger, reqOpts{wsRoleCtx: "editor"}).Body.Bytes()
	f.requireNoPointer(f.getSource(stranger, reqOpts{wsRoleCtx: "editor"}), "stranger to B")

	if string(withRow) != string(withoutRow) {
		t.Fatalf("denied response is distinguishable from a never-moved item:\n with row: %s\nwithout row: %s",
			withRow, withoutRow)
	}

	// Control: the same item, same instant, read by someone who CAN see B —
	// proving the bytes above were identical because the gate withheld the
	// block, not because the fixture never had one to withhold.
	if got := f.movedTo(f.getSource(f.owner, reqOpts{})); len(got) != 1 {
		t.Fatalf("control: the owner should see the destination, got %+v", got)
	}
}

// TestMovedTo_WorkspaceAccessAloneIsNotEnough is the reason the gate uses an
// ITEM scope. A restricted member of the destination workspace can read
// workspace B generally while having no right to the copied item's collection;
// a workspace-level check would hand them the pointer anyway.
func TestMovedTo_WorkspaceAccessAloneIsNotEnough(t *testing.T) {
	f := newMovedToFixture(t)
	u := mustUser(t, f.srv, "restricted-b@example.com", "restrictedb", "")
	if err := f.srv.store.AddWorkspaceMember(f.wsA.ID, u.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember A: %v", err)
	}
	// A genuine editor in B — but only for collB, not the hidden collection
	// the item was copied into.
	if err := f.srv.store.AddWorkspaceMember(f.wsB.ID, u.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember B: %v", err)
	}
	if err := f.srv.store.SetMemberCollectionAccess(f.wsB.ID, u.ID, "specific", []string{f.collB.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}

	// Sanity: the workspace-only scope — the check a naive implementation
	// would have made — passes for this caller.
	probe := f.request(u)
	if acc := f.srv.AuthorizeCrossWorkspaceRead(probe, f.wsB.Slug, CrossWorkspaceWorkspaceOnlyScope()); !acc.Allowed {
		t.Fatalf("precondition: caller should have workspace-level access to B, got %q", acc.Reason)
	}

	f.record(f.destHid, true, 10)
	f.archiveSource()
	f.requireNoPointer(f.getSource(u, reqOpts{wsRoleCtx: "editor"}),
		"restricted member of B, destination in a hidden collection")
}

// request builds a bare cross-workspace probe request for the fixture's caller.
func (f *movedToFixture) request(user *models.User) *http.Request {
	f.t.Helper()
	r := httptest.NewRequest("GET", "/api/v1/workspaces/"+f.wsA.Slug+"/items/x", nil)
	ctx := WithCurrentUser(r.Context(), user)
	ctx = contextWithWorkspaceRoleForTest(ctx, "editor")
	ctx = contextWithResolvedWorkspaceIDForTest(ctx, f.wsA.ID)
	return r.WithContext(ctx)
}

// TestMovedTo_GuestWithUnrelatedItemGrantInDestination is the second half of
// the same trap: a guest holding one item grant in B has a role there
// ("guest") and passes a workspace-level check, but the granted item is not
// the copied one.
func TestMovedTo_GuestWithUnrelatedItemGrantInDestination(t *testing.T) {
	f := newMovedToFixture(t)
	u := mustUser(t, f.srv, "guest-b@example.com", "guestb", "")
	if err := f.srv.store.AddWorkspaceMember(f.wsA.ID, u.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember A: %v", err)
	}
	// One grant in B, on an item unrelated to the copy.
	unrelated := mustItem(t, f.srv, f.wsB.ID, f.collB.ID, "Unrelated B Item")
	if _, err := f.srv.store.CreateItemGrant(f.wsB.ID, unrelated.ID, u.ID, "view", f.owner.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}

	f.record(f.destB, true, 10)
	f.archiveSource()
	f.requireNoPointer(f.getSource(u, reqOpts{wsRoleCtx: "editor"}),
		"guest holding only an unrelated item grant in B")

	// ...and the grant on the ACTUAL destination does reveal it, so the
	// denial above is the scope working rather than the guest role being
	// blanket-refused.
	if _, err := f.srv.store.CreateItemGrant(f.wsB.ID, f.destB.ID, u.ID, "view", f.owner.ID); err != nil {
		t.Fatalf("CreateItemGrant on destination: %v", err)
	}
	if got := f.movedTo(f.getSource(u, reqOpts{wsRoleCtx: "editor"})); len(got) != 1 {
		t.Fatalf("guest granted the destination item should see it, got %+v", got)
	}
}

// TestMovedTo_BearerTokenConsentedToSourceOnly: consent is enforced
// automatically only for the workspace in the URL. A token scoped to A must
// not learn about B even though its owner is a full member there.
func TestMovedTo_BearerTokenConsentedToSourceOnly(t *testing.T) {
	f := newMovedToFixture(t)
	u := mustUser(t, f.srv, "consent@example.com", "consentuser", "")
	for _, ws := range []*models.Workspace{f.wsA, f.wsB} {
		if err := f.srv.store.AddWorkspaceMember(ws.ID, u.ID, "editor"); err != nil {
			t.Fatalf("AddWorkspaceMember: %v", err)
		}
	}

	f.record(f.destB, true, 10)
	f.archiveSource()

	// Consent covers A only.
	f.requireNoPointer(
		f.getSource(u, reqOpts{
			wsRoleCtx: "editor", bearer: true,
			setAllowed: true, allowed: []string{f.wsA.Slug},
		}),
		"bearer consented to A only")

	// Same caller, same membership, consent widened to include B.
	if got := f.movedTo(f.getSource(u, reqOpts{
		wsRoleCtx: "editor", bearer: true,
		setAllowed: true, allowed: []string{f.wsA.Slug, f.wsB.Slug},
	})); len(got) != 1 {
		t.Fatalf("bearer consented to A and B should see the destination, got %+v", got)
	}
}

// TestMovedTo_BearerPlatformAdminIsNotAShortcut: the cookie-vs-bearer admin
// split (BUG-1616/1617) must hold here too — a platform admin over a bearer
// surface who is not a member of B learns nothing.
func TestMovedTo_BearerPlatformAdminIsNotAShortcut(t *testing.T) {
	f := newMovedToFixture(t)
	admin := mustUser(t, f.srv, "admin-movedto@example.com", "adminmovedto", "admin")
	if err := f.srv.store.AddWorkspaceMember(f.wsA.ID, admin.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember A: %v", err)
	}

	f.record(f.destB, true, 10)
	f.archiveSource()

	f.requireNoPointer(f.getSource(admin, reqOpts{wsRoleCtx: "editor", bearer: true}),
		"bearer-borne platform admin, not a member of B")
	if got := f.movedTo(f.getSource(admin, reqOpts{wsRoleCtx: "editor"})); len(got) != 1 {
		t.Fatalf("cookie-session platform admin should see the destination, got %+v", got)
	}
}

// --- multiples ---------------------------------------------------------

// TestMovedTo_MultipleDestinationsFilteredPerDestination: the forward lookup is
// a SET, the filter is per destination, and the loop must not short-circuit on
// the first denial or the first hit.
//
// This is also the concrete case that rules out a "newest archived row wins"
// first-match implementation: the caller here may read the OLDER destination
// and not the newer one, so first-match would hand them nothing at all rather
// than the smaller true answer. See the SET note in item_moved_to.go.
func TestMovedTo_MultipleDestinationsFilteredPerDestination(t *testing.T) {
	f := newMovedToFixture(t)
	u := mustUser(t, f.srv, "multi@example.com", "multiuser", "")
	if err := f.srv.store.AddWorkspaceMember(f.wsA.ID, u.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember A: %v", err)
	}
	// Member of C only. C's row is recorded FIRST so the denied B row is
	// encountered before it — a short-circuit on the first denial would drop
	// the destination the caller may legitimately see.
	if err := f.srv.store.AddWorkspaceMember(f.wsC.ID, u.ID, "viewer"); err != nil {
		t.Fatalf("AddWorkspaceMember C: %v", err)
	}

	f.record(f.destC, true, 10)
	f.record(f.destB, true, 11) // newest → first in the store's ordering
	f.archiveSource()

	got := f.movedTo(f.getSource(u, reqOpts{wsRoleCtx: "editor"}))
	if len(got) != 1 {
		t.Fatalf("expected exactly the readable destination, got %d (%+v)", len(got), got)
	}
	if got[0].WorkspaceSlug != f.wsC.Slug {
		t.Errorf("expected workspace C, got %q", got[0].WorkspaceSlug)
	}

	// The owner sees both, newest first.
	all := f.movedTo(f.getSource(f.owner, reqOpts{}))
	if len(all) != 2 {
		t.Fatalf("owner should see both destinations, got %d (%+v)", len(all), all)
	}
	if all[0].WorkspaceSlug != f.wsB.Slug || all[1].WorkspaceSlug != f.wsC.Slug {
		t.Errorf("expected newest-first [B, C], got [%s, %s]", all[0].WorkspaceSlug, all[1].WorkspaceSlug)
	}
}

// --- DR-2a: copies are not moves ---------------------------------------

func TestMovedTo_PlainCopyNeverClaimsAMove(t *testing.T) {
	f := newMovedToFixture(t)
	f.record(f.destB, false, 0) // plain copy: archived_source = false
	f.archiveSource()

	f.requireNoPointer(f.getSource(f.owner, reqOpts{}),
		"archived source whose only provenance row is a plain copy")
}

// A source copied to C and then MOVED to B advertises only B.
func TestMovedTo_MoveRowWinsOverCopyRow(t *testing.T) {
	f := newMovedToFixture(t)
	f.record(f.destC, false, 0)
	f.record(f.destB, true, 11)
	f.archiveSource()

	got := f.movedTo(f.getSource(f.owner, reqOpts{}))
	if len(got) != 1 || got[0].WorkspaceSlug != f.wsB.Slug {
		t.Fatalf("expected only the move destination (B), got %+v", got)
	}
}

// --- the restore decision ----------------------------------------------

// TestMovedTo_RestoredSourceOmitsTheBlock pins TASK-2359's restore decision:
// the block is OMITTED for a non-archived source. Restoring leaves two live
// items with the same content in two workspaces — legitimate — but the source
// has not moved anywhere, so the response must stop asserting that it did.
// Past-tense provenance is the back-pointer question and applies equally to
// plain copies, which this field must never claim as moves.
func TestMovedTo_RestoredSourceOmitsTheBlock(t *testing.T) {
	f := newMovedToFixture(t)
	f.record(f.destB, true, 10)
	f.archiveSource()

	if got := f.movedTo(f.getSource(f.owner, reqOpts{})); len(got) != 1 {
		t.Fatalf("precondition: archived source should advertise the destination, got %+v", got)
	}

	f.restoreSource()
	f.requireNoPointer(f.getSource(f.owner, reqOpts{}), "restored (live) source")

	// Archiving again brings it back — the omission is a function of the
	// source's current state, not a one-way latch. Archive → restore → move
	// again is the legal sequence DR-2a's source_seq exists for, so the second
	// move lands in a different workspace with its own destination item
	// (uq_item_workspace_moves_target forbids reusing the first one).
	f.record(f.destC, true, 12)
	f.archiveSource()
	got := f.movedTo(f.getSource(f.owner, reqOpts{}))
	if len(got) != 2 {
		t.Fatalf("re-archived source should advertise both moves, got %+v", got)
	}
	if got[0].WorkspaceSlug != f.wsC.Slug {
		t.Errorf("newest move (C) should lead, got %q", got[0].WorkspaceSlug)
	}
}

// --- destination lifecycle ---------------------------------------------

// A destination that has itself been archived is not advertised: pointing a
// banner at an archived item is worse than saying nothing.
func TestMovedTo_ArchivedDestinationIsNotAdvertised(t *testing.T) {
	f := newMovedToFixture(t)
	f.record(f.destB, true, 10)
	f.archiveSource()
	if err := f.srv.store.DeleteItem(f.destB.ID); err != nil {
		t.Fatalf("DeleteItem(dest): %v", err)
	}

	f.requireNoPointer(f.getSource(f.owner, reqOpts{}), "archived destination")
}

// --- source-side gate --------------------------------------------------

// TestMovedTo_SourceItemGrantGuestNeverSeesPointer is the negative test
// PLAN-2357 names explicitly, alongside the share-link one: "a share link on
// the source, or a guest holding only a source-item grant, must never see
// moved_to". Delegated access to one item is not access to that item's
// cross-workspace provenance.
//
// The teeth are in the second half: the same guest is a full OWNER of the
// destination workspace, so the destination gate would happily clear them. The
// pointer is withheld anyway, because the question this gate answers is what
// the SOURCE grant conveys.
func TestMovedTo_SourceItemGrantGuestNeverSeesPointer(t *testing.T) {
	f := newMovedToFixture(t)
	guest := mustUser(t, f.srv, "source-guest@example.com", "sourceguest", "")
	// No membership in A at all — the sole claim is one item grant on the
	// source, which is what makes RequireWorkspaceAccess call them a guest.
	if _, err := f.srv.store.CreateItemGrant(f.wsA.ID, f.source.ID, guest.ID, "view", f.owner.ID); err != nil {
		t.Fatalf("CreateItemGrant on source: %v", err)
	}

	// Make them an OWNER of the destination workspace too, so the destination
	// gate would happily clear them. A gate that only asked "can this caller
	// read the destination?" emits the pointer here.
	if err := f.srv.store.AddWorkspaceMember(f.wsB.ID, guest.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember B: %v", err)
	}
	f.record(f.destB, true, 10)
	f.archiveSource()

	archived, err := f.srv.store.ResolveItemIncludeDeleted(f.wsA.ID, f.source.Slug)
	if err != nil || archived == nil {
		t.Fatalf("ResolveItemIncludeDeleted: %v", err)
	}
	if acc := f.srv.AuthorizeCrossWorkspaceRead(
		f.request(guest), f.wsB.Slug, CrossWorkspaceItemScope(f.destB)); !acc.Allowed {
		t.Fatalf("precondition: the guest should pass the DESTINATION gate, got %q", acc.Reason)
	}

	// The gate itself, driven directly. This is the assertion with teeth:
	// delete the workspaceRole(r) == "guest" check and it fails, because every
	// later gate passes for this caller.
	guestReq := f.getSourceRequest(guest, reqOpts{wsRoleCtx: "guest"})
	if got := f.srv.movedToDestinations(guestReq, archived); got != nil {
		t.Fatalf("a guest holding only a source-item grant was handed the destination: %+v", got)
	}

	// A real membership in workspace A — however minimal — restores it, so the
	// refusal above is about what the delegated SOURCE claim conveys and not a
	// blanket denial of this user.
	if err := f.srv.store.AddWorkspaceMember(f.wsA.ID, guest.ID, "viewer"); err != nil {
		t.Fatalf("AddWorkspaceMember A: %v", err)
	}
	if got := f.movedTo(f.getSource(guest, reqOpts{wsRoleCtx: "viewer"})); len(got) != 1 {
		t.Fatalf("a genuine member of both workspaces should see the destination, got %+v", got)
	}

	// Belt and braces, and worth recording: today the guest cannot reach the
	// archived source over HTTP at all — grants on a soft-deleted item yield no
	// visibility (the same rule TestCrossWorkspace_GrantOnArchivedItemGivesNoRole
	// pins), so the route 404s before the pointer is ever computed. That makes
	// the gate above defense in depth rather than the only thing standing
	// between a source-grant guest and a destination — but it is exactly the
	// kind of upstream behavior that a future change to grant semantics could
	// relax without anyone thinking about this file.
	revoked := mustUser(t, f.srv, "source-guest-2@example.com", "sourceguest2", "")
	if _, err := f.srv.store.CreateItemGrant(f.wsA.ID, f.source.ID, revoked.ID, "view", f.owner.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}
	rr := f.rawGetSource(revoked, reqOpts{wsRoleCtx: "guest"})
	if rr.Code != http.StatusNotFound {
		t.Errorf("grant-only guest reading an archived source: got %d, want 404 — "+
			"if this behavior changed, the guest gate in movedToDestinations is now load-bearing on its own",
			rr.Code)
	}
}

// TestMovedTo_ArchivedSourceCollectionWithholdsPointer: requireItemVisible,
// which handleGetItem already ran, does NOT establish that the source's own
// collection is live — soft-deleting a collection leaves its items fetchable.
// That is pre-existing behavior for the item body and stays that way; what
// must not happen is a cross-workspace disclosure being layered on top of it.
// The destination side applies the identical rule
// (crossWorkspaceLiveCollection); this is its source-side mirror.
func TestMovedTo_ArchivedSourceCollectionWithholdsPointer(t *testing.T) {
	f := newMovedToFixture(t)
	f.record(f.destB, true, 10)
	f.archiveSource()

	if got := f.movedTo(f.getSource(f.owner, reqOpts{})); len(got) != 1 {
		t.Fatalf("precondition: the pointer should be visible before the collection is archived, got %+v", got)
	}

	if err := f.srv.store.DeleteCollection(f.collA.ID, ""); err != nil {
		t.Fatalf("DeleteCollection: %v", err)
	}

	// The item body is still served — that is the pre-existing behavior this
	// change deliberately leaves alone — but the pointer is gone.
	f.requireNoPointer(f.getSource(f.owner, reqOpts{}), "source under a soft-deleted collection")
}

// TestMovedTo_RefusesForeignWorkspaceRequestContext pins the precondition
// movedToDestinations enforces rather than assumes: the request must have
// resolved to the SOURCE item's own workspace.
//
// handleGetItem satisfies that trivially and is the only caller today, so this
// is a guard against the NEXT caller — an item-ID-addressed route, a resolver,
// a list handler. Given a request scoped to workspace B and a source from
// workspace A, the guest gate would test the caller's role in B while the
// source lives in A, and a grants-only guest of A who owns B would slip
// through the one gate written to stop them.
func TestMovedTo_RefusesForeignWorkspaceRequestContext(t *testing.T) {
	f := newMovedToFixture(t)
	f.record(f.destB, true, 10)
	f.archiveSource()

	archived, err := f.srv.store.ResolveItemIncludeDeleted(f.wsA.ID, f.source.Slug)
	if err != nil || archived == nil {
		t.Fatalf("ResolveItemIncludeDeleted: %v", err)
	}

	// The honest context: request resolved to A, source lives in A.
	if got := f.srv.movedToDestinations(f.getSourceRequest(f.owner, reqOpts{}), archived); len(got) != 1 {
		t.Fatalf("precondition: the correctly-scoped call should return the destination, got %+v", got)
	}

	// The same everything, with the request claiming a different workspace.
	r := f.getSourceRequest(f.owner, reqOpts{})
	r = r.WithContext(contextWithResolvedWorkspaceIDForTest(r.Context(), f.wsB.ID))
	if got := f.srv.movedToDestinations(r, archived); got != nil {
		t.Fatalf("a request scoped to a foreign workspace was served the pointer: %+v", got)
	}

	// And a request with no resolved workspace at all — a caller that skipped
	// RequireWorkspaceAccess — fails closed rather than treating the empty
	// role as "not a guest".
	bare := httptest.NewRequest("GET", "/api/v1/items/"+f.source.Slug, nil)
	bare = bare.WithContext(WithCurrentUser(bare.Context(), f.owner))
	if got := f.srv.movedToDestinations(bare, archived); got != nil {
		t.Fatalf("a request with no resolved workspace was served the pointer: %+v", got)
	}
}

// --- surface isolation -------------------------------------------------

// TestMovedTo_ShareLinkNeverCarriesPointer guards the public share DTO's
// isolation DELIBERATELY. handlers_share_links.go hand-rolls its item payload,
// so it does not pick the block up today — but that is an accident of the
// current code, and a refactor to `writeJSON(w, ..., item)` would start
// publishing destinations to anonymous visitors on every share link. Pinning
// the exact key set makes that refactor fail loudly.
func TestMovedTo_ShareLinkNeverCarriesPointer(t *testing.T) {
	f := newMovedToFixture(t)

	// The load-bearing half: hand the projection an item whose MovedTo is
	// ALREADY populated and assert it does not survive. Driving this
	// end-to-end instead would pass vacuously — the share route resolves only
	// live items, and nothing on that path ever populates MovedTo, so a
	// refactor to `writeJSON(w, ..., item)` would sail through an e2e-only
	// test while publishing destinations on every public share link.
	privileged := *f.destB
	privileged.MovedTo = []models.ItemMovedTo{{
		WorkspaceSlug: f.wsC.Slug,
		Ref:           f.destC.Ref,
		ItemSlug:      f.destC.Slug,
		Title:         f.destC.Title,
	}}
	dto := publicShareItemDTO(&privileged)
	if _, present := dto["moved_to"]; present {
		t.Fatal("the public share DTO carries moved_to — a destination pointer is now visible to anonymous visitors")
	}
	// The DTO is an explicit allow-list; freeze it so an accidental wholesale
	// swap to the full item model is caught rather than inferred. Any new key
	// here is a disclosure decision and wants its own review.
	want := map[string]bool{
		"title": true, "content": true, "fields": true,
		"ref": true, "collection_name": true, "collection_icon": true,
	}
	for k := range dto {
		if !want[k] {
			t.Errorf("public share item DTO gained an unexpected key %q — re-review it for disclosure before widening this list", k)
		}
	}
	for k := range want {
		if _, present := dto[k]; !present {
			t.Errorf("public share item DTO lost the expected key %q", k)
		}
	}

	// ...and the wired-up route really does use that projection, so the
	// assertion above is about live code rather than a parallel helper.
	link, err := f.srv.store.CreateShareLink(f.wsB.ID, "item", f.destB.ID, "view", f.owner.ID, nil)
	if err != nil {
		t.Fatalf("CreateShareLink: %v", err)
	}
	rr := doRequest(f.srv, "GET", "/api/v1/s/"+link.Token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("resolve share link: got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Item map[string]json.RawMessage `json:"item"`
	}
	parseJSON(t, rr, &resp)
	if len(resp.Item) != len(want) {
		t.Errorf("share route payload has %d item keys, projection has %d — the route no longer uses publicShareItemDTO",
			len(resp.Item), len(want))
	}
	for k := range resp.Item {
		if !want[k] {
			t.Errorf("share route payload carries unexpected key %q", k)
		}
	}
}

// The block belongs to the single-item GET alone. Mutation handlers reuse
// enrichItemForResponse and return a full item, so a future move of the
// population call into that helper — or a well-meaning copy-paste into another
// handler — would silently widen the surface. Driven through the real HTTP
// handlers, not through enrichItemForResponse directly, so it fails whichever
// way the widening arrives.
//
// RESTORE is the load-bearing one: it is the single mutation whose response
// describes an item that, one instant earlier, was an archived source WITH a
// live move row and therefore genuinely had a pointer to emit.
func TestMovedTo_MutationResponsesDoNotCarryPointer(t *testing.T) {
	f := newMovedToFixture(t)
	f.record(f.destB, true, 10)
	f.archiveSource()

	if got := f.movedTo(f.getSource(f.owner, reqOpts{})); len(got) != 1 {
		t.Fatalf("precondition: the archived source should have a pointer to withhold, got %+v", got)
	}

	// POST .../restore — the response is an item that was moved-out a
	// microsecond ago.
	restoreRR := f.callItemHandler(f.srv.handleRestoreItem, "POST", "/restore", nil, f.owner)
	if restoreRR.Code != http.StatusOK {
		t.Fatalf("restore: got %d: %s", restoreRR.Code, restoreRR.Body.String())
	}
	requireNoMovedToKey(t, restoreRR, "restore response")

	// PATCH the now-live item.
	updateRR := f.callItemHandler(f.srv.handleUpdateItem, "PATCH", "",
		map[string]interface{}{"title": "Renamed Source"}, f.owner)
	if updateRR.Code != http.StatusOK {
		t.Fatalf("update: got %d: %s", updateRR.Code, updateRR.Body.String())
	}
	requireNoMovedToKey(t, updateRR, "update response")

	// And the helper every mutation shares stays clean, so the population
	// cannot be smuggled in one level down.
	fresh, err := f.srv.store.GetItem(f.source.ID)
	if err != nil || fresh == nil {
		t.Fatalf("GetItem: %v", err)
	}
	if fresh.MovedTo != nil {
		t.Fatal("store-level item carries MovedTo; it must only ever be set by handleGetItem")
	}
	if err := f.srv.enrichItemForResponse(fresh, nil); err != nil {
		t.Fatalf("enrichItemForResponse: %v", err)
	}
	if fresh.MovedTo != nil {
		t.Fatal("enrichItemForResponse populated MovedTo — it runs on create/update/restore/move too; keep the population in handleGetItem")
	}
}

func requireNoMovedToKey(t *testing.T, rr *httptest.ResponseRecorder, label string) {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("%s: parse: %v\nbody: %s", label, err, rr.Body.String())
	}
	// Guard against the assertion going vacuous: these handlers return the
	// item at the TOP level, so if that ever changes to an envelope the
	// moved_to check below would be looking in the wrong place and pass for
	// the wrong reason.
	if _, present := raw["id"]; !present {
		t.Fatalf("%s is not a top-level item payload (%s); re-point this assertion before trusting it",
			label, rr.Body.String())
	}
	if blob, present := raw["moved_to"]; present {
		t.Fatalf("%s carries moved_to (%s) — the pointer belongs to the single-item GET alone", label, string(blob))
	}
}

// callItemHandler drives one item-scoped handler with the same synthetic
// request shape getSource builds, for a suffix under the item's route.
func (f *movedToFixture) callItemHandler(
	h http.HandlerFunc, method, suffix string, body interface{}, user *models.User,
) *httptest.ResponseRecorder {
	f.t.Helper()

	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			f.t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(data)
	}
	r := httptest.NewRequest(method,
		"/api/v1/workspaces/"+f.wsA.Slug+"/items/"+f.source.Slug+suffix, reader)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}

	ctx := WithCurrentUser(r.Context(), user)
	ctx = contextWithWorkspaceRoleForTest(ctx, "owner")
	ctx = contextWithResolvedWorkspaceIDForTest(ctx, f.wsA.ID)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("slug", f.wsA.Slug)
	rctx.URLParams.Add("itemSlug", f.source.Slug)
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)

	rr := httptest.NewRecorder()
	h(rr, r.WithContext(ctx))
	return rr
}
