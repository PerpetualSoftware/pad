package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The RESPONSE-PARITY MATRIX for the cross-workspace copy — TASK-2370.
//
// The preflight and the copy share one resolution + authorization ladder
// (Server.resolveAuthorizedCopy). This file is the executable statement of
// what that sharing has to mean, and it is deliberately the EXECUTABLE
// acceptance criterion for the extraction: "one implementation, used by
// both" is a code-review property a reviewer can be satisfied by while the
// code drifts a month later.
//
// Be precise about what this does and does not prove. It is a BEHAVIOURAL
// matrix: it fails when a handler's ladder DIVERGES — a different refusal,
// a different code, a different order. A handler that re-inlined the ladder
// and reproduced it exactly would still pass, because from a client's
// vantage point nothing changed. The structural "one implementation"
// property is a code-review property and stays one; what is pinned here is
// the property that actually harms users when it breaks.
//
// Why this is not hypothetical: preflight/copy divergence is empirically the
// number one defect source in PLAN-2357 — five separate divergences were
// found and fixed in Phase 2 alone, and one of them was a disagreement about
// the ORDER of refusals rather than the set. assertPreflightMatchesCopy
// (handlers_items_copy_test.go) pins the FIELD MAPPING half of that
// agreement. This is its sibling for the AUTHORIZATION half.
//
// Each row drives ONE arrangement through BOTH endpoints with a
// byte-identical request and asserts the two answer with the same status
// AND the same error code — including the rows where a naive extraction
// would silently change the answer:
//
//   - the two unresolvable-source variants (404 absent / 409 archived), and
//     the archived-but-hidden case that must NOT report 409;
//   - a malformed body, which is only reachable AFTER source visibility has
//     succeeded — decoding first would leak source existence;
//   - both missing_field refusals, which sit BETWEEN source authorization
//     and any destination lookup;
//   - every destination-workspace denial variant: WriteDenied is not one
//     outcome (500 / permission_denied / forbidden), and its `forbidden`
//     arm is itself reached by three different denial reasons — workspace
//     unresolvable, resolvable-but-no-role, and role-without-edit — which
//     collapse to one response on purpose;
//   - collection_not_found, for a collection that is absent and for one that
//     is merely hidden — byte-identical, or the endpoint enumerates hidden
//     collections one slug at a time;
//   - the no-attributable-actor path, whose POSITION (after the fourth check,
//     before any field handling) is itself a divergence class this feature
//     has been bitten by;
//   - and the SUCCESSFUL resolved snapshot, so the matrix is not satisfiable
//     by a ladder that refuses everything.
//
// MUTATION-VERIFIED: reordering either handler's ladder (e.g. hoisting the
// body decode above the source check, or hoisting the destination lookup
// above the required-field checks) fails rows here. See the note on
// TestCopyAuthorizationParityMatrix_Ordering.

// bothRR is the pair of recorders one arrangement produced.
type bothRR struct {
	pre  *httptest.ResponseRecorder
	copy *httptest.ResponseRecorder
}

// callBothRaw drives handleCopyItemPreflight and handleCopyItem over the
// SAME request bytes, the same context, and the same route params — the only
// difference is the path suffix and the handler. Sharing the construction is
// load-bearing: a difference in how the two are invoked would weaken every
// assertion in this file.
func (f *copyPreflightFixture) callBothRaw(user *models.User, o reqOpts, itemSlug string, raw []byte) bothRR {
	f.t.Helper()

	build := func(path string) *http.Request {
		r := httptest.NewRequest("POST", path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")

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
		rctx.URLParams.Add("itemSlug", itemSlug)
		ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)

		r = r.WithContext(ctx)
		if o.bearer {
			r.Header.Set("Authorization", "Bearer test-token")
		}
		return r
	}

	base := "/api/v1/workspaces/" + f.wsA.Slug + "/items/" + itemSlug
	preRR := httptest.NewRecorder()
	f.srv.handleCopyItemPreflight(preRR, build(base+"/copy/preflight"))
	copyRR := httptest.NewRecorder()
	f.srv.handleCopyItem(copyRR, build(base+"/copy"))
	return bothRR{pre: preRR, copy: copyRR}
}

// callBoth is callBothRaw with a JSON-encodable body.
func (f *copyPreflightFixture) callBoth(user *models.User, o reqOpts, body map[string]any) bothRR {
	f.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		f.t.Fatalf("marshal body: %v", err)
	}
	return f.callBothRaw(user, o, f.source.Slug, raw)
}

// assertRefusalParity is the matrix's single assertion: the two endpoints
// refused with the same status, the same error code, and — since every
// refusal on this path goes through a shared writer — the same bytes.
func assertRefusalParity(t *testing.T, got bothRR, wantStatus int, wantCode string) {
	t.Helper()

	if got.pre.Code != got.copy.Code {
		t.Fatalf("the preview and the copy answered with different statuses — a client that "+
			"previewed successfully can be refused by the copy, or worse:\n preflight: %d %s\n copy:      %d %s",
			got.pre.Code, got.pre.Body.String(), got.copy.Code, got.copy.Body.String())
	}
	if got.pre.Body.String() != got.copy.Body.String() {
		t.Fatalf("the preview and the copy answered with different bodies:\n preflight: %s\n copy:      %s",
			got.pre.Body.String(), got.copy.Body.String())
	}
	if got.copy.Code != wantStatus {
		t.Fatalf("status = %d, want %d: %s", got.copy.Code, wantStatus, got.copy.Body.String())
	}
	if code := errCode(t, got.copy); code != wantCode {
		t.Fatalf("error code = %q, want %q: %s", code, wantCode, got.copy.Body.String())
	}
}

// TestCopyAuthorizationParityMatrix is the matrix proper. Each subtest
// arranges one refusal and requires both endpoints to produce it identically.
func TestCopyAuthorizationParityMatrix(t *testing.T) {
	// --- unresolvable source, both variants -----------------------------
	//
	// Resolution runs BEFORE the body is decoded in both handlers. If it did
	// not, a caller who cannot see the item would learn whether their JSON
	// was well-formed and a caller who can see it would not — which is the
	// leak the ordering exists to prevent.

	t.Run("source absent", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		raw, _ := json.Marshal(f.resolvableBody())
		got := f.callBothRaw(f.owner, reqOpts{}, "definitely-not-an-item", raw)
		assertRefusalParity(t, got, http.StatusNotFound, "not_found")
	})

	t.Run("source archived and visible to the caller", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		if err := f.srv.store.DeleteItem(f.source.ID); err != nil {
			t.Fatalf("DeleteItem: %v", err)
		}
		// writeItemResolveError's 404-vs-409 split. Both endpoints must keep
		// it: an archived source is neither absent nor copyable, and a client
		// can say something useful about the difference.
		assertRefusalParity(t, f.callBoth(f.owner, reqOpts{}, f.resolvableBody()),
			http.StatusConflict, "archived")
	})

	t.Run("source archived but hidden from the caller", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		if err := f.srv.store.DeleteItem(f.source.ID); err != nil {
			t.Fatalf("DeleteItem: %v", err)
		}
		otherA := mustSchemaCollection(t, f.srv, f.wsA.ID, "Other A", srcSchemaJSON)
		u := f.restrictedEditor("parity-arch-hidden@example.com", "parityarchhidden",
			[]string{otherA.ID}, []string{f.collB.ID})
		// The OTHER half of the split: a caller who could not see the row must
		// not learn it is archived. Both endpoints collapse to the bare 404.
		assertRefusalParity(t, f.callBoth(u, reqOpts{wsRoleCtx: "editor"}, f.resolvableBody()),
			http.StatusNotFound, "not_found")
	})

	// --- source authorization, checks 1 and 2 ---------------------------

	t.Run("source item not visible", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		otherA := mustSchemaCollection(t, f.srv, f.wsA.ID, "Other A", srcSchemaJSON)
		u := f.restrictedEditor("parity-hidden-src@example.com", "parityhiddensrc",
			[]string{otherA.ID}, []string{f.collB.ID})
		assertRefusalParity(t, f.callBoth(u, reqOpts{wsRoleCtx: "editor"}, f.resolvableBody()),
			http.StatusNotFound, "not_found")
	})

	t.Run("source edit denied", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		viewer := mustUser(t, f.srv, "parity-viewer-a@example.com", "parityviewera", "")
		if err := f.srv.store.AddWorkspaceMember(f.wsA.ID, viewer.ID, "viewer"); err != nil {
			t.Fatalf("AddWorkspaceMember A: %v", err)
		}
		if err := f.srv.store.AddWorkspaceMember(f.wsB.ID, viewer.ID, "editor"); err != nil {
			t.Fatalf("AddWorkspaceMember B: %v", err)
		}
		assertRefusalParity(t, f.callBoth(viewer, reqOpts{wsRoleCtx: "viewer"}, f.resolvableBody()),
			http.StatusNotFound, "not_found")
	})

	// --- decode and required fields, BETWEEN the two halves -------------
	//
	// These sit after source authorization and before any destination
	// lookup. Hoisting the destination lookup earlier would disclose
	// destination state to a caller sending a bad body.

	t.Run("malformed body", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		got := f.callBothRaw(f.owner, reqOpts{}, f.source.Slug, []byte("{not json"))
		assertRefusalParity(t, got, http.StatusBadRequest, "invalid_body")
	})

	t.Run("missing target_workspace", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		assertRefusalParity(t,
			f.callBoth(f.owner, reqOpts{}, map[string]any{"target_collection": f.collB.Slug}),
			http.StatusBadRequest, "missing_field")
	})

	t.Run("missing target_collection", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		assertRefusalParity(t,
			f.callBoth(f.owner, reqOpts{}, map[string]any{"target_workspace": f.wsB.Slug}),
			http.StatusBadRequest, "missing_field")
	})

	// --- destination workspace denial: all THREE variants ---------------
	//
	// WriteDenied is not one outcome. An extraction that collapsed it to a
	// single denial shape would be both a behaviour change and a disclosure
	// change.

	// WriteDenied's default arm is reached by three DIFFERENT denial
	// reasons, and all three are rows here. They collapse to one response on
	// purpose — that collapse is the disclosure posture — so the only way to
	// know each reason still lands on it is to arrange each reason.

	t.Run("destination workspace unresolvable (by slug)", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		outsider := mustUser(t, f.srv, "parity-outsider@example.com", "parityoutsider", "")
		wsC := mustWorkspace(t, f.srv, "Parity Unreachable WS", outsider.ID)

		// Slug resolution is ACL-scoped, so this never reaches the role check:
		// the workspace resolves to nil and the reason is
		// CrossWorkspaceWorkspaceNotFound. Distinct from the UUID row below,
		// which DOES reach the role check — and the whole point is that a
		// caller cannot tell the two apart.
		body := f.resolvableBody()
		body["target_workspace"] = wsC.Slug
		assertRefusalParity(t, f.callBoth(f.owner, reqOpts{}, body),
			http.StatusForbidden, "forbidden")
	})

	t.Run("destination workspace resolvable but caller has no role (by UUID)", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		outsider := mustUser(t, f.srv, "parity-uuid-outsider@example.com", "parityuuidoutsider", "")
		wsC := mustWorkspace(t, f.srv, "Parity UUID WS", outsider.ID)

		// resolveWorkspace's UUID branch is NOT ACL-scoped, so the workspace
		// resolves and crossWorkspaceRole returns "" —
		// CrossWorkspaceNoWorkspaceAccess. Byte-identical to the slug row.
		body := f.resolvableBody()
		body["target_workspace"] = wsC.ID
		assertRefusalParity(t, f.callBoth(f.owner, reqOpts{}, body),
			http.StatusForbidden, "forbidden")
	})

	t.Run("destination workspace edit denied", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		u := mustUser(t, f.srv, "parity-viewer-b@example.com", "parityviewerb", "")
		if err := f.srv.store.AddWorkspaceMember(f.wsA.ID, u.ID, "editor"); err != nil {
			t.Fatalf("AddWorkspaceMember A: %v", err)
		}
		if err := f.srv.store.AddWorkspaceMember(f.wsB.ID, u.ID, "viewer"); err != nil {
			t.Fatalf("AddWorkspaceMember B: %v", err)
		}
		// The third reason: the caller has a role in the destination but it
		// is not a writing one — CrossWorkspaceInsufficientPermission, refused
		// at the WORKSPACE-only stage, before the collection is ever named.
		//
		// Note this is where a read-only destination member is stopped, not at
		// check 4: a caller who clears the workspace-only edit gate has a base
		// role of editor or better, which clears the collection-scoped edit
		// gate too. The collection stage's reachable denial is the visibility
		// collapse — collection_not_found, covered by the two rows below.
		assertRefusalParity(t, f.callBoth(u, reqOpts{wsRoleCtx: "editor"}, f.resolvableBody()),
			http.StatusForbidden, "forbidden")
	})

	t.Run("destination workspace outside the token allow-list", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		got := f.callBoth(f.owner, reqOpts{
			bearer: true, setAllowed: true, allowed: []string{f.wsA.Slug},
		}, f.resolvableBody())
		assertRefusalParity(t, got, http.StatusForbidden, "permission_denied")
	})

	t.Run("destination workspace lookup failure", func(t *testing.T) {
		f := newCopyPreflightFixture(t)

		// A caller who is a full-access member of the SOURCE workspace and a
		// stranger to the destination, naming the destination by UUID.
		//
		// That combination is what isolates the failure to the destination
		// half. resolveWorkspace's UUID branch is not ACL-scoped, so the
		// destination reaches crossWorkspaceRole, which — finding no
		// membership row — asks UserHasGrantsInWorkspace. The SOURCE half
		// never gets there: a member with collection_access="all" is answered
		// out of workspace_members, and the base-role branch of
		// crossWorkspaceEditAllowed short-circuits before any grant lookup.
		// So breaking the grant tables breaks exactly one of the two lookups.
		u := mustUser(t, f.srv, "parity-lookup@example.com", "paritylookup", "")
		if err := f.srv.store.AddWorkspaceMember(f.wsA.ID, u.ID, "editor"); err != nil {
			t.Fatalf("AddWorkspaceMember A: %v", err)
		}

		body := f.resolvableBody()
		body["target_workspace"] = f.wsB.ID

		// CONTROL, before anything is broken: the same request is an ordinary
		// 403 forbidden. Without this the 500 below could be any failure at
		// all rather than the WriteDenied branch under test.
		assertRefusalParity(t, f.callBoth(u, reqOpts{wsRoleCtx: "editor"}, body),
			http.StatusForbidden, "forbidden")

		f.breakGrantLookups()
		assertRefusalParity(t, f.callBoth(u, reqOpts{wsRoleCtx: "editor"}, body),
			http.StatusInternalServerError, "internal_error")
	})

	// --- destination collection, checks 3 and 4 -------------------------

	t.Run("destination collection absent", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		body := f.resolvableBody()
		body["target_collection"] = "definitely-not-a-collection"
		assertRefusalParity(t, f.callBoth(f.owner, reqOpts{}, body),
			http.StatusNotFound, crossWorkspaceCollectionNotFoundCode)
	})

	t.Run("destination collection hidden", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		u := f.restrictedEditor("parity-hidden-dst@example.com", "parityhiddendst",
			[]string{f.collA.ID}, []string{f.collB.ID})
		body := f.resolvableBody()
		body["target_collection"] = f.hiddenB.Slug
		// Same status AND same code as "absent" above, on both endpoints —
		// otherwise a restricted member enumerates the destination's hidden
		// collections one slug at a time.
		assertRefusalParity(t, f.callBoth(u, reqOpts{wsRoleCtx: "editor"}, body),
			http.StatusNotFound, crossWorkspaceCollectionNotFoundCode)
	})

	// --- source before destination --------------------------------------

	t.Run("source verdict wins when both halves would refuse", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		otherA := mustSchemaCollection(t, f.srv, f.wsA.ID, "Other A", srcSchemaJSON)
		u := mustUser(t, f.srv, "parity-both-fail@example.com", "paritybothfail", "")
		if err := f.srv.store.AddWorkspaceMember(f.wsA.ID, u.ID, "editor"); err != nil {
			t.Fatalf("AddWorkspaceMember A: %v", err)
		}
		if err := f.srv.store.SetMemberCollectionAccess(f.wsA.ID, u.ID, "specific", []string{otherA.ID}); err != nil {
			t.Fatalf("SetMemberCollectionAccess: %v", err)
		}
		// The caller can see neither the source item nor the destination
		// workspace. The SOURCE's non-disclosing 404 must win on both
		// endpoints — a destination verdict built for a caller who could not
		// read the source is itself a disclosure.
		assertRefusalParity(t, f.callBoth(u, reqOpts{wsRoleCtx: "editor"}, f.resolvableBody()),
			http.StatusNotFound, "not_found")
	})
}

// breakGrantLookups renames the two grant tables out from under the store so
// UserHasGrantsInWorkspace fails, and restores them at the end of the test.
//
// This is the only way to reach CrossWorkspaceLookupFailed end to end: a
// store failure is not otherwise inducible from a request, which is exactly
// why WriteDenied's own doc comment says an attacker cannot provoke one. The
// arm still has to be covered, because it is one of the three shapes
// WriteDenied emits and a "one denial" extraction would swallow it.
func (f *copyPreflightFixture) breakGrantLookups() {
	f.t.Helper()
	renames := [][2]string{
		{"collection_grants", "collection_grants_parity_broken"},
		{"item_grants", "item_grants_parity_broken"},
	}

	// Cleanup is registered BEFORE the first rename and restores only what
	// actually got renamed. Registering it after the loop would leave the
	// first table renamed if the second DDL failed — the fixture database is
	// per-test so nothing else would notice, but a half-restored schema turns
	// one failure into a confusing cascade inside this test.
	var done [][2]string
	f.t.Cleanup(func() {
		for _, rn := range done {
			if _, err := f.srv.store.DB().Exec(`ALTER TABLE ` + rn[1] + ` RENAME TO ` + rn[0]); err != nil {
				f.t.Errorf("restore %s: %v", rn[0], err)
			}
		}
	})
	for _, rn := range renames {
		if _, err := f.srv.store.DB().Exec(`ALTER TABLE ` + rn[0] + ` RENAME TO ` + rn[1]); err != nil {
			f.t.Fatalf("rename %s: %v", rn[0], err)
		}
		done = append(done, rn)
	}
}

// TestCopyAuthorizationParityMatrix_ResolvedSnapshot is the matrix's
// positive row, and it is not optional: without it every assertion above is
// satisfied by a ladder that refuses everything.
//
// The two endpoints differ in status by design — 200 for a preview, 201 for
// a creation — so what is compared is the RESOLVED SNAPSHOT the shared step
// produced: the same source item in the same source workspace, the same
// destination workspace and collection, and the same archive_source reading.
// A ladder that resolved a different destination on one path would fail here
// even though every denial row still passed.
func TestCopyAuthorizationParityMatrix_ResolvedSnapshot(t *testing.T) {
	f := newCopyPreflightFixture(t)
	body := f.resolvableBody()

	got := f.callBoth(f.owner, reqOpts{}, body)
	if got.pre.Code != http.StatusOK {
		t.Fatalf("preflight: got %d, want 200: %s", got.pre.Code, got.pre.Body.String())
	}
	if got.copy.Code != http.StatusCreated {
		t.Fatalf("copy: got %d, want 201: %s", got.copy.Code, got.copy.Body.String())
	}

	var pre ItemCopyPreflight
	if err := json.Unmarshal(got.pre.Body.Bytes(), &pre); err != nil {
		t.Fatalf("parse preflight: %v", err)
	}
	var res ItemCopyResult
	if err := json.Unmarshal(got.copy.Body.Bytes(), &res); err != nil {
		t.Fatalf("parse copy: %v", err)
	}

	for _, c := range []struct {
		what      string
		pre, copy string
	}{
		{"source workspace", pre.Source.WorkspaceSlug, res.Source.WorkspaceSlug},
		{"source ref", pre.Source.Ref, res.Source.Ref},
		{"destination workspace", pre.Destination.WorkspaceSlug, res.Destination.WorkspaceSlug},
		{"destination collection", pre.Destination.CollectionSlug, res.Destination.CollectionSlug},
	} {
		if c.pre != c.copy {
			t.Errorf("the preview and the copy resolved a different %s: %q vs %q", c.what, c.pre, c.copy)
		}
	}
	if pre.Destination.WorkspaceSlug != f.wsB.Slug || pre.Destination.CollectionSlug != f.collB.Slug {
		t.Errorf("resolved destination = %s/%s, want %s/%s",
			pre.Destination.WorkspaceSlug, pre.Destination.CollectionSlug, f.wsB.Slug, f.collB.Slug)
	}
	if pre.ArchiveSource != res.Source.Archived {
		t.Errorf("archive_source disagreement: preview echoed %v, copy archived = %v",
			pre.ArchiveSource, res.Source.Archived)
	}
}

// TestCopyAuthorizationParityMatrix_NoAttributableActor is the matrix's
// actor row. It needs a whole different world — a FRESH INSTALL with zero
// users, the only state in which the actor can be unresolvable — so it does
// not fit the fixture the rest of the matrix shares.
//
// The row matters because actor resolution's POSITION is itself a divergence
// class this feature has already been bitten by: an earlier preflight
// resolved it down by the attachment planner, so a request that fails BOTH
// ways (no actor AND a malformed override) got the override from one endpoint
// and the actor from the other. TestCopyEndpoint_PreflightAndCopyAgreeWith-
// NoAttributableActor covers that ordering interaction in depth; this row
// keeps the plain case inside the matrix so the matrix is complete on its
// own terms.
func TestCopyAuthorizationParityMatrix_NoAttributableActor(t *testing.T) {
	srv := testServer(t)
	// CreateWorkspace WITHOUT a membership row: with no users there is nobody
	// to be a member, and workspace_members has a foreign key onto users.
	mustFreshWS := func(name string) *models.Workspace {
		ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: name})
		if err != nil {
			t.Fatalf("CreateWorkspace(%s): %v", name, err)
		}
		return ws
	}
	wsA := mustFreshWS("Parity Fresh Source")
	wsB := mustFreshWS("Parity Fresh Dest")
	collA := mustSchemaCollection(t, srv, wsA.ID, "Parity Fresh A", srcSchemaJSON)
	collB := mustSchemaCollection(t, srv, wsB.ID, "Parity Fresh B", srcSchemaJSON)

	source, err := srv.store.CreateItem(wsA.ID, collA.ID, models.ItemCreate{
		Title: "Unattributable", Fields: `{"status":"open"}`,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if n, err := srv.store.UserCount(); err != nil || n != 0 {
		t.Fatalf("fixture precondition: want a fresh install with 0 users, got %d (%v)", n, err)
	}
	// CreateItem defaults created_by to "user", so the unattributable state
	// has to be forced. That is the point of the case: legacy/corrupt data,
	// not something a write path produces.
	if _, err := srv.store.DB().Exec(
		srv.store.D().Rebind(`UPDATE items SET created_by = '' WHERE id = ?`), source.ID,
	); err != nil {
		t.Fatalf("blank created_by: %v", err)
	}

	raw, err := json.Marshal(map[string]any{
		"target_workspace": wsB.Slug, "target_collection": collB.Slug,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	build := func(path string) *http.Request {
		r := httptest.NewRequest("POST", path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		ctx := contextWithWorkspaceRoleForTest(r.Context(), "owner")
		ctx = contextWithResolvedWorkspaceIDForTest(ctx, wsA.ID)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("slug", wsA.Slug)
		rctx.URLParams.Add("itemSlug", source.Slug)
		return r.WithContext(context.WithValue(ctx, chi.RouteCtxKey, rctx))
	}

	base := "/api/v1/workspaces/" + wsA.Slug + "/items/" + source.Slug
	preRR := httptest.NewRecorder()
	srv.handleCopyItemPreflight(preRR, build(base+"/copy/preflight"))
	copyRR := httptest.NewRecorder()
	srv.handleCopyItem(copyRR, build(base+"/copy"))

	assertRefusalParity(t, bothRR{pre: preRR, copy: copyRR},
		http.StatusForbidden, "actor_required")
}

// TestCopyAuthorizationParityMatrix_Ordering states the ordering properties
// the matrix exists to catch, as claims about a SINGLE endpoint's response
// rather than about the pair. They are what a reordering mutation breaks
// first, and they are here so the failure message names the ordering rather
// than leaving a reader to infer it from a parity mismatch.
//
// HOW TO VERIFY THE MATRIX BITES (the harness this task requires, kept in
// the file it exercises): move the decodeJSON block in resolveAuthorizedCopy
// above the ResolveItem call, or move the destination lookup above the
// required-field checks. Either mutation fails this test and several rows of
// the matrix above; revert it afterwards.
func TestCopyAuthorizationParityMatrix_Ordering(t *testing.T) {
	// Resolve BEFORE decode: a malformed body sent for an item the caller
	// cannot see must report the ITEM, not the body. Reporting invalid_body
	// here tells a stranger their JSON reached a real resolution step.
	t.Run("resolve runs before decode", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		otherA := mustSchemaCollection(t, f.srv, f.wsA.ID, "Other A", srcSchemaJSON)
		u := f.restrictedEditor("parity-order-decode@example.com", "parityorderdecode",
			[]string{otherA.ID}, []string{f.collB.ID})

		got := f.callBothRaw(u, reqOpts{wsRoleCtx: "editor"}, f.source.Slug, []byte("{not json"))
		assertRefusalParity(t, got, http.StatusNotFound, "not_found")
	})

	// Required-field checks BEFORE the destination lookup: a body missing
	// target_collection while naming a destination workspace the caller
	// cannot reach must report missing_field, not the destination's verdict.
	t.Run("required fields precede the destination lookup", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		outsider := mustUser(t, f.srv, "parity-order-dest@example.com", "parityorderdest", "")
		wsC := mustWorkspace(t, f.srv, "Parity Order WS", outsider.ID)

		got := f.callBoth(f.owner, reqOpts{}, map[string]any{"target_workspace": wsC.Slug})
		assertRefusalParity(t, got, http.StatusBadRequest, "missing_field")
	})

	// Source authorization BEFORE the body is even considered: a caller who
	// cannot edit the source and sends a body naming an unreachable
	// destination gets the source's 404.
	t.Run("source authorization precedes everything downstream", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		viewer := mustUser(t, f.srv, "parity-order-src@example.com", "parityordersrc", "")
		if err := f.srv.store.AddWorkspaceMember(f.wsA.ID, viewer.ID, "viewer"); err != nil {
			t.Fatalf("AddWorkspaceMember A: %v", err)
		}
		got := f.callBothRaw(viewer, reqOpts{wsRoleCtx: "viewer"}, f.source.Slug, []byte("{not json"))
		assertRefusalParity(t, got, http.StatusNotFound, "not_found")
	})
}
