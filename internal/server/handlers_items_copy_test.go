package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/webhooks"
)

// Tests for the mutating cross-workspace copy (PLAN-2357 / TASK-2365).
//
// The fixture is the PREFLIGHT's, deliberately and not for convenience: the
// headline deliverable of this task is that the two endpoints agree about
// what a copy persists, and an agreement test run against two different
// fixtures proves nothing. Everything below therefore drives one server, one
// source item and one destination schema through both routes.
//
// Sections:
//
//  1. DR-6 agreement — the preflight's `carried` bucket IS the copy's
//     persisted field map, for the two reconciled divergences and in
//     general.
//  2. DR-14 fanout — the asymmetric matrix, with seq attribution.
//  3. DR-9 / DR-10a / DR-10b — the four denials, and the in-transaction
//     re-check.
//  4. DR-13 — exactly one store invocation, never a retry.
//  5. Caller obligations — storageInfoCache, EnforceItemLimit.

// --- driving the copy endpoint -----------------------------------------

// callCopy issues the MUTATION exactly as the router would, with the same
// context stashing f.call uses for the preflight. Sharing the shape matters:
// a difference in how the two are invoked would weaken every agreement
// assertion below.
func (f *copyPreflightFixture) callCopy(user *models.User, o reqOpts, body map[string]any) *httptest.ResponseRecorder {
	f.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		f.t.Fatalf("marshal body: %v", err)
	}
	r := httptest.NewRequest("POST",
		"/api/v1/workspaces/"+f.wsA.Slug+"/items/"+f.source.Slug+"/copy",
		bytes.NewReader(raw))
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
	rctx.URLParams.Add("itemSlug", f.source.Slug)
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)

	r = r.WithContext(ctx)
	if o.bearer {
		r.Header.Set("Authorization", "Bearer test-token")
	}

	rr := httptest.NewRecorder()
	f.srv.handleCopyItem(rr, r)
	return rr
}

// copyOK issues the copy as the owner and requires a 201.
func (f *copyPreflightFixture) copyOK(body map[string]any) ItemCopyResult {
	f.t.Helper()
	rr := f.callCopy(f.owner, reqOpts{}, body)
	if rr.Code != http.StatusCreated {
		f.t.Fatalf("copy: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var out ItemCopyResult
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		f.t.Fatalf("parse copy result: %v\nbody: %s", err, rr.Body.String())
	}
	return out
}

// resolvableBody is a request the copy can actually satisfy: the two
// needs_value entries the base fixture leaves open are supplied, so the
// preflight comes back valid=true and the copy comes back 201. Anything a
// test wants to vary it layers on top.
func (f *copyPreflightFixture) resolvableBody() map[string]any {
	b := f.baseBody()
	b["field_overrides"] = map[string]any{"ticket": "T-1", "code": "123"}
	return b
}

// persistedFields reads the destination item's fields blob back out of the
// database. Deliberately NOT the response body: the contract being asserted
// is what the copy STORED, and a handler that echoed the right thing while
// writing the wrong thing would pass a response-only check.
func (f *copyPreflightFixture) persistedFields(itemID string) map[string]any {
	f.t.Helper()
	item, err := f.srv.store.GetItem(itemID)
	if err != nil || item == nil {
		f.t.Fatalf("GetItem(%s): %v (nil=%v)", itemID, err, item == nil)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(item.Fields), &out); err != nil {
		f.t.Fatalf("parse persisted fields %q: %v", item.Fields, err)
	}
	return out
}

// --- 1. DR-6: the preview IS the copy ----------------------------------

// assertPreflightMatchesCopy is the general form of this task's headline
// deliverable, and every specific divergence case below is a call to it.
//
// It sends ONE body to BOTH endpoints and asserts that the preflight's
// `carried` bucket — key set AND values — is exactly the field map the copy
// persisted. That is the whole of DR-6 stated as an executable claim: "one
// answer, two consumers". A divergence in either direction fails it, so it
// catches a THIRD disagreement nobody has named yet as readily as the two
// that were.
//
// Values are compared after a JSON round-trip on both sides (the preflight's
// come through the response encoder; the copy's through items.fields), so
// numbers are float64 on both and the comparison is apples to apples.
func assertPreflightMatchesCopy(t *testing.T, f *copyPreflightFixture, label string, body map[string]any) ItemCopyResult {
	t.Helper()

	pre := f.ok(body)
	if !pre.Valid {
		t.Fatalf("%s: preflight is not valid, so the copy cannot be expected to succeed: %+v",
			label, pre.Fields.NeedsValue)
	}
	want := make(map[string]any, len(pre.Fields.Carried))
	for _, c := range pre.Fields.Carried {
		want[c.Key] = c.Value
	}

	res := f.copyOK(body)
	got := f.persistedFields(res.Item.ID)

	if !reflect.DeepEqual(want, got) {
		t.Fatalf("%s: the preflight and the copy disagree about what is persisted.\n"+
			"  preflight carried: %#v\n  copy persisted:    %#v", label, want, got)
	}
	return res
}

// TestCopyEndpoint_PreflightAndCopyAgreeOnNullOverride is divergence #1,
// closed.
//
// Before TASK-2365, Store.migrateCopyFields did `migrated.Fields[k] = v` for
// EVERY override including a nil one. items.ValidateFields treats a nil value
// as absent for the required check but LEAVES IT IN THE MAP, so the copy
// persisted a literal `"key": null` — a value the preflight had just reported
// as unset, and which also suppressed the destination schema's default. The
// store now deletes the key, exactly as the preflight's merge loop does.
//
// Two shapes, because they fail differently:
//
//   - `tier` HAS a destination default, so nulling it must re-apply the
//     default ("a"), not persist null and not leave the key absent;
//   - `priority` is optional with NO default, so nulling it must leave the
//     key genuinely ABSENT — not present-and-null.
func TestCopyEndpoint_PreflightAndCopyAgreeOnNullOverride(t *testing.T) {
	t.Run("defaulted field re-defaults", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		body := f.resolvableBody()
		body["field_overrides"] = map[string]any{"ticket": "T-1", "code": "123", "tier": nil}

		res := assertPreflightMatchesCopy(t, f, "null override on a defaulted field", body)

		got := f.persistedFields(res.Item.ID)
		if v, ok := got["tier"]; !ok || v != "a" {
			t.Fatalf("tier = %v (present=%v), want the destination default %q", v, ok, "a")
		}
	})

	t.Run("undefaulted optional field is absent, not null", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		body := f.resolvableBody()
		body["field_overrides"] = map[string]any{"ticket": "T-1", "code": "123", "priority": nil}

		res := assertPreflightMatchesCopy(t, f, "null override on an undefaulted field", body)

		got := f.persistedFields(res.Item.ID)
		v, present := got["priority"]
		if present {
			t.Fatalf("priority is present as %#v; a null override must DELETE the key, "+
				"not persist a literal null the preview reported as unset", v)
		}

		// The raw blob is checked too. reflect.DeepEqual over the decoded map
		// would also catch `"priority":null` (it decodes to a present nil), but
		// the byte-level assertion is the one that reads as the regression.
		item, err := f.srv.store.GetItem(res.Item.ID)
		if err != nil || item == nil {
			t.Fatalf("GetItem: %v", err)
		}
		if bytes.Contains([]byte(item.Fields), []byte(`"priority":null`)) {
			t.Fatalf("persisted fields carry a literal JSON null: %s", item.Fields)
		}
	})
}

// TestCopyEndpoint_PreflightAndCopyAgreeOnUndeclaredOverride is divergence
// #2, closed.
//
// The preflight always refused an override naming a field the destination
// schema does not declare (400 malformed_override). The copy merged it, and
// items.ValidateFields ignores keys the schema does not declare, so the copy
// PERSISTED an orphan key on the new item. The gate now exists on the store
// side too, and the two refusals must be indistinguishable to a client: same
// status, same code.
//
// The negative half matters as much as the positive: nothing may be created.
// "Refused" and "created it anyway without the key" would both produce a
// non-201 if only the status were checked.
func TestCopyEndpoint_PreflightAndCopyAgreeOnUndeclaredOverride(t *testing.T) {
	f := newCopyPreflightFixture(t)

	body := f.resolvableBody()
	body["field_overrides"] = map[string]any{
		"ticket": "T-1", "code": "123", "not_a_field": "x", "also_not": 7,
	}

	before := f.snapshot()

	preRR := f.call(f.owner, reqOpts{}, body)
	copyRR := f.callCopy(f.owner, reqOpts{}, body)

	if preRR.Code != http.StatusBadRequest {
		t.Fatalf("preflight: expected 400, got %d: %s", preRR.Code, preRR.Body.String())
	}
	if copyRR.Code != preRR.Code {
		t.Fatalf("the copy answered %d where the preflight answered %d — the preview lies about "+
			"whether the request is acceptable:\n preflight: %s\n copy:      %s",
			copyRR.Code, preRR.Code, preRR.Body.String(), copyRR.Body.String())
	}
	if pc, cc := errCode(t, preRR), errCode(t, copyRR); pc != cc {
		t.Fatalf("error codes differ: preflight %q, copy %q", pc, cc)
	}
	if got := errCode(t, copyRR); got != "malformed_override" {
		t.Fatalf("copy error code = %q, want malformed_override", got)
	}

	// Both offending keys are named, so a client can fix its request.
	for _, key := range []string{"also_not", "not_a_field"} {
		if !bytes.Contains(copyRR.Body.Bytes(), []byte(key)) {
			t.Errorf("the refusal does not name the offending key %q: %s", key, copyRR.Body.String())
		}
	}

	// And nothing was written — the store must refuse before the insert, not
	// create the item and drop the key.
	if after := f.snapshot(); after != before {
		t.Fatalf("a refused copy mutated state:\n before: %+v\n after:  %+v", before, after)
	}
}

// TestCopyEndpoint_PreflightAndCopyAgreeGenerally sweeps the agreement
// assertion across the request shapes a dialog can produce, so a THIRD
// divergence has to survive more than the two cases that were named.
//
// Each body is run against a FRESH fixture: the copy mutates, and a shared
// one would let an earlier case's write (a destination item that changes the
// unique-slug space, say) decide a later one.
func TestCopyEndpoint_PreflightAndCopyAgreeGenerally(t *testing.T) {
	cases := []struct {
		name      string
		overrides map[string]any
		archive   bool
	}{
		{
			name:      "minimum resolvable set",
			overrides: map[string]any{"ticket": "T-1", "code": "123"},
		},
		{
			name: "override replaces a value that would have migrated",
			// priority carries "low" from the source; the override must win,
			// and both endpoints must agree it did.
			overrides: map[string]any{"ticket": "T-1", "code": "123", "priority": "high"},
		},
		{
			name: "override supplies a key the destination defaults",
			// tier has a default; an explicit value must beat it.
			overrides: map[string]any{"ticket": "T-1", "code": "123", "tier": "b"},
		},
		{
			name: "override targets a key migration DROPPED",
			// count is number→select and drops. Overriding it with a legal
			// destination value must produce a carried field, identically on
			// both paths.
			overrides: map[string]any{"ticket": "T-1", "code": "123", "count": "y"},
		},
		{
			name:      "the move path",
			overrides: map[string]any{"ticket": "T-1", "code": "123"},
			archive:   true,
		},
		{
			name: "every override at once, including a null",
			overrides: map[string]any{
				"ticket": "T-1", "code": "456", "priority": "high",
				"count": "x", "tier": nil, "status": "done",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCopyPreflightFixture(t)
			body := f.baseBody()
			body["field_overrides"] = tc.overrides
			body["archive_source"] = tc.archive
			assertPreflightMatchesCopy(t, f, tc.name, body)
		})
	}
}

// TestCopyEndpoint_PreflightAndCopyAgreeWithNoAttributableActor is the THIRD
// divergence, found by hunting for one (Codex round 4) rather than because it
// was already documented.
//
// The preflight used to fall back to a literal `"preflight"` actor when there
// was no authenticated user and the source item had no creator. That is
// harmless on a path that writes nothing — and that is exactly why it was
// wrong: the preview happily described a copy the store would refuse outright
// for want of an actor, which the handler would then have reported as the
// ambiguous "it may or may not have landed" 500 for an operation that never
// began. Both endpoints now resolve the actor through one function and refuse
// identically.
//
// Reachable only on a fresh install (no users at all, everything open until
// `pad auth setup`) holding an item whose created_by is empty — which no
// current write path produces, since CreateItem defaults it to "user". Narrow,
// but the failure mode it produced was a lie about a mutation.
//
// This is also the coverage for the PREFLIGHT's actor_required branch: it
// drives handleCopyItemPreflight directly and requires its answer to be
// byte-identical to the copy's, which is a stronger assertion than a separate
// preflight-only test would make (Codex round 10).
func TestCopyEndpoint_PreflightAndCopyAgreeWithNoAttributableActor(t *testing.T) {
	// A FRESH INSTALL: zero users, so crossWorkspaceRole synthesises "owner"
	// for a nil-user caller and the four-check ladder passes. That is the only
	// state in which the actor can be unresolvable — with any user row
	// present, a nil-user caller is denied long before this matters.
	srv := testServer(t)
	mustFreshWS := func(name string) *models.Workspace {
		// CreateWorkspace WITHOUT the membership row mustWorkspace adds: with
		// no users there is nobody to be a member, and workspace_members has a
		// foreign key onto users.
		ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: name})
		if err != nil {
			t.Fatalf("CreateWorkspace(%s): %v", name, err)
		}
		return ws
	}
	wsA := mustFreshWS("Fresh Source WS")
	wsB := mustFreshWS("Fresh Dest WS")
	collA := mustSchemaCollection(t, srv, wsA.ID, "Fresh A", srcSchemaJSON)
	collB := mustSchemaCollection(t, srv, wsB.ID, "Fresh B", srcSchemaJSON)

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
	// has to be forced. That is the point of the case: it is legacy/corrupt
	// data, not something a write path produces.
	if _, err := srv.store.DB().Exec(
		srv.store.D().Rebind(`UPDATE items SET created_by = '' WHERE id = ?`), source.ID,
	); err != nil {
		t.Fatalf("blank created_by: %v", err)
	}

	callWith := func(path string, h http.HandlerFunc, overrides map[string]any) *httptest.ResponseRecorder {
		payload := map[string]any{
			"target_workspace": wsB.Slug, "target_collection": collB.Slug,
		}
		if overrides != nil {
			payload["field_overrides"] = overrides
		}
		body, mErr := json.Marshal(payload)
		if mErr != nil {
			t.Fatalf("marshal: %v", mErr)
		}
		r := httptest.NewRequest("POST", path, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		ctx := contextWithWorkspaceRoleForTest(r.Context(), "owner")
		ctx = contextWithResolvedWorkspaceIDForTest(ctx, wsA.ID)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("slug", wsA.Slug)
		rctx.URLParams.Add("itemSlug", source.Slug)
		ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
		rr := httptest.NewRecorder()
		h(rr, r.WithContext(ctx))
		return rr
	}
	call := func(path string, h http.HandlerFunc) *httptest.ResponseRecorder {
		return callWith(path, h, nil)
	}

	var itemsBefore int
	if err := srv.store.DB().QueryRow(
		srv.store.D().Rebind(`SELECT COUNT(*) FROM items`)).Scan(&itemsBefore); err != nil {
		t.Fatalf("count: %v", err)
	}

	base := "/api/v1/workspaces/" + wsA.Slug + "/items/" + source.Slug
	preRR := call(base+"/copy/preflight", srv.handleCopyItemPreflight)
	copyRR := call(base+"/copy", srv.handleCopyItem)

	if preRR.Code != copyRR.Code || preRR.Body.String() != copyRR.Body.String() {
		t.Fatalf("the preview and the copy answer differently when there is nobody to attribute to:\n"+
			" preflight: %d %s\n copy:      %d %s",
			preRR.Code, preRR.Body.String(), copyRR.Code, copyRR.Body.String())
	}
	if copyRR.Code != http.StatusForbidden || errCode(t, copyRR) != "actor_required" {
		t.Fatalf("got %d %s, want 403 actor_required", copyRR.Code, copyRR.Body.String())
	}
	// Never the ambiguous 500: nothing was attempted, so telling the user to
	// go check the destination would be false.
	if bytes.Contains(copyRR.Body.Bytes(), []byte("check the destination")) {
		t.Errorf("an unattempted copy produced the ambiguous-outcome message: %s", copyRR.Body.String())
	}

	var itemsAfter int
	if err := srv.store.DB().QueryRow(
		srv.store.D().Rebind(`SELECT COUNT(*) FROM items`)).Scan(&itemsAfter); err != nil {
		t.Fatalf("count: %v", err)
	}
	if itemsAfter != itemsBefore {
		t.Errorf("a refused copy created %d item(s)", itemsAfter-itemsBefore)
	}

	// The two must also agree on the ORDER of refusals, not just the set. A
	// request that fails BOTH ways — no actor AND an override naming a field
	// the destination does not declare — must get the same answer from both,
	// or a dialog fixes the override and is then told about the actor by only
	// one of them (Codex round 5).
	bad := map[string]any{"not_a_field": "x"}
	preBoth := callWith(base+"/copy/preflight", srv.handleCopyItemPreflight, bad)
	copyBoth := callWith(base+"/copy", srv.handleCopyItem, bad)
	if preBoth.Code != copyBoth.Code || preBoth.Body.String() != copyBoth.Body.String() {
		t.Fatalf("the preview and the copy refuse a doubly-invalid request differently:\n"+
			" preflight: %d %s\n copy:      %d %s",
			preBoth.Code, preBoth.Body.String(), copyBoth.Code, copyBoth.Body.String())
	}

	// CONTROL: with a real creator on the row, the same fresh-install caller
	// copies successfully — so the 403 above is about the missing actor and
	// not about the fresh-install state itself.
	if _, err := srv.store.DB().Exec(
		srv.store.D().Rebind(`UPDATE items SET created_by = 'user' WHERE id = ?`), source.ID,
	); err != nil {
		t.Fatalf("restore created_by: %v", err)
	}
	if rr := call(base+"/copy", srv.handleCopyItem); rr.Code != http.StatusCreated {
		t.Fatalf("control copy: got %d, want 201: %s", rr.Code, rr.Body.String())
	}
}

// TestCopyEndpoint_PreflightWarningsMatchTheCopy checks the OTHER half of
// the shared answer: the attachment numbers. The planner is shared precisely
// so the preview's byte total is the copy's byte total, and a client shows
// the user that number before they agree.
func TestCopyEndpoint_PreflightWarningsMatchTheCopy(t *testing.T) {
	f := newCopyPreflightFixture(t)

	att := &models.Attachment{
		WorkspaceID: f.wsA.ID, UploadedBy: f.owner.ID,
		StorageKey: "fs:deadbeef", ContentHash: "deadbeef",
		MimeType: "image/png", SizeBytes: 4096, Filename: "shared.png",
	}
	if err := f.srv.store.CreateAttachment(att); err != nil {
		t.Fatalf("CreateAttachment: %v", err)
	}
	// One resolvable ref and one that resolves to nothing, so both counters
	// are exercised.
	content := "![](pad-attachment:" + att.ID + ") and ![](pad-attachment:00000000-0000-4000-8000-000000000000)"
	if _, err := f.srv.store.UpdateItem(f.source.ID, models.ItemUpdate{Content: &content}); err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}

	body := f.resolvableBody()
	pre := f.ok(body)
	res := f.copyOK(body)

	if pre.Warnings.AttachmentCount != res.Warnings.AttachmentCount {
		t.Errorf("attachment_count: preflight %d, copy %d",
			pre.Warnings.AttachmentCount, res.Warnings.AttachmentCount)
	}
	if pre.Warnings.AttachmentBytes != res.Warnings.AttachmentBytes {
		t.Errorf("attachment_bytes: preflight %d, copy %d",
			pre.Warnings.AttachmentBytes, res.Warnings.AttachmentBytes)
	}
	if pre.Warnings.UnresolvableRefCount != res.Warnings.UnresolvableRefCount {
		t.Errorf("unresolvable_ref_count: preflight %d, copy %d",
			pre.Warnings.UnresolvableRefCount, res.Warnings.UnresolvableRefCount)
	}
	if res.Warnings.AttachmentCount != 1 || res.Warnings.UnresolvableRefCount != 1 {
		t.Fatalf("fixture did not exercise both counters: %+v", res.Warnings)
	}
}

// --- 2. DR-14: the fanout matrix ---------------------------------------

// copyFanoutObserver captures all three emission channels for BOTH
// workspaces at once. The whole point of DR-14 is an ASYMMETRY, and an
// assertion that only watches the destination cannot see it.
type copyFanoutObserver struct {
	t   *testing.T
	f   *copyPreflightFixture
	chA chan events.Event
	chB chan events.Event

	hooks    chan webhookDelivery
	recvA    *httptest.Server
	recvB    *httptest.Server
	baseline map[string]int
}

type webhookDelivery struct {
	workspace string // "A" or "B"
	event     string
}

func newCopyFanoutObserver(t *testing.T, f *copyPreflightFixture) *copyFanoutObserver {
	t.Helper()
	o := &copyFanoutObserver{
		t: t, f: f,
		chA:   f.bus.Subscribe(f.wsA.ID),
		chB:   f.bus.Subscribe(f.wsB.ID),
		hooks: make(chan webhookDelivery, 32),
	}
	t.Cleanup(func() {
		f.bus.Unsubscribe(o.chA)
		f.bus.Unsubscribe(o.chB)
	})

	d := webhooks.NewDispatcher(f.srv.store)
	d.SkipSSRF = true // loopback httptest receivers
	f.srv.SetWebhookDispatcher(d)

	for _, side := range []struct {
		label string
		ws    *models.Workspace
		dst   **httptest.Server
	}{{"A", f.wsA, &o.recvA}, {"B", f.wsB, &o.recvB}} {
		label := side.label
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var payload struct {
				Event string `json:"event"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			o.hooks <- webhookDelivery{workspace: label, event: payload.Event}
			w.WriteHeader(http.StatusOK)
		}))
		*side.dst = srv
		t.Cleanup(srv.Close)
		if _, err := f.srv.store.CreateWebhook(side.ws.ID, models.WebhookCreate{
			URL: srv.URL, Events: `["*"]`,
		}); err != nil {
			t.Fatalf("CreateWebhook(%s): %v", label, err)
		}
	}

	o.baseline = o.activityCounts()
	return o
}

// activityCounts returns per-workspace "<workspace>:<action>" counts, so an
// assertion can name exactly which row it expects where.
func (o *copyFanoutObserver) activityCounts() map[string]int {
	o.t.Helper()
	out := map[string]int{}
	for label, ws := range map[string]*models.Workspace{"A": o.f.wsA, "B": o.f.wsB} {
		rows, err := o.f.srv.store.ListWorkspaceActivity(ws.ID, models.ActivityListParams{Limit: 500})
		if err != nil {
			o.t.Fatalf("ListWorkspaceActivity(%s): %v", label, err)
		}
		for _, a := range rows {
			out[label+":"+a.Action]++
		}
	}
	return out
}

// newActivity is the activity delta since the observer was created.
func (o *copyFanoutObserver) newActivity() map[string]int {
	o.t.Helper()
	now := o.activityCounts()
	delta := map[string]int{}
	for k, v := range now {
		if d := v - o.baseline[k]; d > 0 {
			delta[k] = d
		}
	}
	return delta
}

// drainSSE collects everything published to a workspace's channel, giving
// stragglers a short window to arrive.
func drainSSE(ch chan events.Event) []events.Event {
	var out []events.Event
	deadline := time.After(fanoutQuietWindow)
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
		case <-deadline:
			return out
		}
	}
}

// drainWebhooks collects deliveries over one quiet window. The window is a
// wait for something that must NOT arrive in half these assertions, so it is
// generous rather than tight; it only costs wall-clock time when passing.
func (o *copyFanoutObserver) drainWebhooks() []webhookDelivery {
	var out []webhookDelivery
	deadline := time.After(fanoutQuietWindow)
	for {
		select {
		case d := <-o.hooks:
			out = append(out, d)
		case <-deadline:
			return out
		}
	}
}

const fanoutQuietWindow = 750 * time.Millisecond

func eventTypes(evs []events.Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, string(e.Type))
	}
	sort.Strings(out)
	return out
}

func webhookEventsFor(ds []webhookDelivery, workspace string) []string {
	var out []string
	for _, d := range ds {
		if d.workspace == workspace {
			out = append(out, d.event)
		}
	}
	sort.Strings(out)
	return out
}

// TestCopyEndpoint_PlainCopyEmitsNothingInTheSource is the strictest clause
// of DR-14, and the one an implementation is most likely to get wrong by
// being helpful: a plain copy touches NOTHING in workspace A, so it must
// emit nothing there. Not a "copied" activity row, not an ItemUpdated. A
// spurious event would tell A's watchers the source changed when it did not,
// and would push a delta cursor that is legitimately still valid.
func TestCopyEndpoint_PlainCopyEmitsNothingInTheSource(t *testing.T) {
	f := newCopyPreflightFixture(t)

	// Push B's counter ahead of A's so the seq assertion below is capable of
	// failing: with both workspaces on their first item the two seqs are
	// equal and a crossed pair would be invisible (Codex round 24, the same
	// trap round 20 found in the move test).
	for i := 0; i < 3; i++ {
		if _, err := f.srv.store.CreateItem(f.wsB.ID, f.collB.ID, models.ItemCreate{
			Title:  fmt.Sprintf("Filler %d", i),
			Fields: `{"status":"open","ticket":"T-0"}`,
		}); err != nil {
			t.Fatalf("CreateItem(filler %d): %v", i, err)
		}
	}

	o := newCopyFanoutObserver(t, f)

	seqABefore := f.maxSeq(f.wsA.ID)
	res := f.copyOK(f.resolvableBody())

	// --- destination: all three, and only those three ---
	if got := o.newActivity(); !reflect.DeepEqual(got, map[string]int{"B:created": 1}) {
		t.Errorf("activity delta = %v, want exactly {B:created:1}", got)
	}
	evB := drainSSE(o.chB)
	if got := eventTypes(evB); !reflect.DeepEqual(got, []string{string(events.ItemCreated)}) {
		t.Fatalf("workspace B SSE = %v, want exactly [%s]", got, events.ItemCreated)
	}
	// ...carrying B's OWN committed seq. The matrix is only half-asserted
	// without this: DR-14 specifies the emission set AND the attribution,
	// and a plain copy is the case where the source has no seq to cross with.
	if wantB := f.maxSeq(f.wsB.ID); evB[0].Seq != wantB {
		t.Errorf("workspace B's create event carries seq %d, want B's committed seq %d",
			evB[0].Seq, wantB)
	}
	if evB[0].Seq == seqABefore {
		t.Errorf("B's create event carries seq %d, which is also A's — fixture went vacuous", evB[0].Seq)
	}
	if res.Destination.Seq != evB[0].Seq {
		t.Errorf("destination.seq %d disagrees with the event's %d", res.Destination.Seq, evB[0].Seq)
	}
	hooks := o.drainWebhooks()
	if got := webhookEventsFor(hooks, "B"); !reflect.DeepEqual(got, []string{"item.created"}) {
		t.Errorf("workspace B webhooks = %v, want exactly [item.created]", got)
	}

	// --- source: nothing at all, on any channel ---
	if got := drainSSE(o.chA); len(got) != 0 {
		t.Errorf("a PLAIN copy published %d SSE event(s) in the source workspace: %+v", len(got), got)
	}
	if got := webhookEventsFor(hooks, "A"); len(got) != 0 {
		t.Errorf("a PLAIN copy dispatched webhooks in the source workspace: %v", got)
	}

	// ...and A's delta cursor must not have moved either (DR-14 seq clause).
	if got := f.maxSeq(f.wsA.ID); got != seqABefore {
		t.Errorf("a plain copy advanced workspace A's seq from %d to %d", seqABefore, got)
	}
	if res.Source.Archived {
		t.Error("a plain copy reported source.archived = true")
	}
	if res.Source.Seq != 0 {
		t.Errorf("a plain copy reported a source seq (%d); A never wrote", res.Source.Seq)
	}
}

// TestCopyEndpoint_MoveEmitsInBothWorkspaces is the other half of the
// matrix, plus the seq-ATTRIBUTION clause: each event carries its OWN
// workspace's committed seq, never the other's.
func TestCopyEndpoint_MoveEmitsInBothWorkspaces(t *testing.T) {
	f := newCopyPreflightFixture(t)

	// FORCE THE TWO SEQ SPACES APART before anything is observed. Without
	// this the assertion below is VACUOUS: the source is workspace A's first
	// item and the copy is workspace B's first item, so both seqs are 1 and
	// crossing them changes nothing (Codex round 20). Filling B's counter
	// makes the destination's seq unmistakably B's.
	for i := 0; i < 3; i++ {
		if _, err := f.srv.store.CreateItem(f.wsB.ID, f.collB.ID, models.ItemCreate{
			Title:  fmt.Sprintf("Filler %d", i),
			Fields: `{"status":"open","ticket":"T-0"}`,
		}); err != nil {
			t.Fatalf("CreateItem(filler %d): %v", i, err)
		}
	}

	o := newCopyFanoutObserver(t, f)

	body := f.resolvableBody()
	body["archive_source"] = true
	res := f.copyOK(body)

	if got := o.newActivity(); !reflect.DeepEqual(got, map[string]int{"B:created": 1, "A:archived": 1}) {
		t.Errorf("activity delta = %v, want exactly {B:created:1, A:archived:1}", got)
	}

	evB := drainSSE(o.chB)
	if got := eventTypes(evB); !reflect.DeepEqual(got, []string{string(events.ItemCreated)}) {
		t.Fatalf("workspace B SSE = %v, want exactly [%s]", got, events.ItemCreated)
	}
	evA := drainSSE(o.chA)
	if got := eventTypes(evA); !reflect.DeepEqual(got, []string{string(events.ItemArchived)}) {
		t.Fatalf("workspace A SSE = %v, want exactly [%s]", got, events.ItemArchived)
	}

	hooks := o.drainWebhooks()
	if got := webhookEventsFor(hooks, "B"); !reflect.DeepEqual(got, []string{"item.created"}) {
		t.Errorf("workspace B webhooks = %v, want [item.created]", got)
	}
	if got := webhookEventsFor(hooks, "A"); !reflect.DeepEqual(got, []string{"item.deleted"}) {
		t.Errorf("workspace A webhooks = %v, want [item.deleted]", got)
	}

	// --- seq attribution ---
	//
	// The two seq spaces are independent counters, so a crossed pair is not
	// merely wrong, it is PLAUSIBLE — both numbers are small integers that
	// look like they belong. Each is checked against the row it actually
	// describes, read back from the database, and the filler items above
	// guarantee the two values DIFFER so that crossing them fails.
	wantB := f.maxSeq(f.wsB.ID)
	var srcSeq int64
	if err := f.srv.store.DB().QueryRow(
		f.srv.store.D().Rebind(`SELECT seq FROM items WHERE id = ?`), f.source.ID,
	).Scan(&srcSeq); err != nil {
		t.Fatalf("read archived source seq: %v", err)
	}
	if wantB == srcSeq {
		t.Fatalf("fixture precondition: both workspaces' seqs are %d, so a crossed pair would "+
			"pass every assertion below", wantB)
	}
	if evB[0].Seq != wantB {
		t.Errorf("workspace B's create event carries seq %d, want B's committed seq %d "+
			"(A's is %d — check for a crossed pair)", evB[0].Seq, wantB, srcSeq)
	}
	if evA[0].Seq != srcSeq {
		t.Errorf("workspace A's archive event carries seq %d, want A's committed archive seq %d "+
			"(B's is %d — check for a crossed pair)", evA[0].Seq, srcSeq, wantB)
	}
	// The RESPONSE carries the same two, and must not cross them either.
	if res.Destination.Seq != wantB {
		t.Errorf("destination.seq = %d, want B's %d", res.Destination.Seq, wantB)
	}
	if evA[0].ItemID != f.source.ID {
		t.Errorf("A's event names item %s, want the SOURCE %s", evA[0].ItemID, f.source.ID)
	}
	if evB[0].ItemID != res.Item.ID {
		t.Errorf("B's event names item %s, want the COPY %s", evB[0].ItemID, res.Item.ID)
	}

	// The response carries what a client needs to navigate away from the
	// source it just archived.
	if !res.Source.Archived || res.Source.Seq != srcSeq {
		t.Errorf("source block = %+v, want archived=true seq=%d", res.Source, srcSeq)
	}
	if res.Destination.Ref == "" || res.Destination.WorkspaceSlug != f.wsB.Slug {
		t.Errorf("destination block = %+v, want a ref and workspace %q", res.Destination, f.wsB.Slug)
	}
}

// TestCopyEndpoint_RolledBackCopyEmitsNothing — the reason all fanout is
// post-commit. A refused copy must leave both workspaces exactly as they
// were and publish nothing anywhere; an implementation that emitted from
// inside the transaction would announce an item that does not exist.
//
// Two rollback shapes are exercised: one the store refuses before it writes
// (validation), and one the handler sees as an opaque store failure after
// the call (the DR-13 ambiguity path), because a fanout misplaced into a
// `defer` would survive the first and not the second.
func TestCopyEndpoint_RolledBackCopyEmitsNothing(t *testing.T) {
	t.Run("store refuses the request", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		o := newCopyFanoutObserver(t, f)
		before := f.snapshot()

		// No overrides: `ticket` is required in the destination with no
		// default, so the in-tx validation refuses.
		body := f.baseBody()
		body["archive_source"] = true
		rr := f.callCopy(f.owner, reqOpts{}, body)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for an unsatisfiable field map, got %d: %s", rr.Code, rr.Body.String())
		}

		assertNoFanout(t, f, o, before)
	})

	t.Run("store fails opaquely", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		o := newCopyFanoutObserver(t, f)
		before := f.snapshot()

		f.srv.copyItemFn = func(store.CrossWorkspaceCopyRequest) (*store.CrossWorkspaceCopyResult, error) {
			return nil, errors.New("connection reset mid-commit")
		}
		rr := f.callCopy(f.owner, reqOpts{}, f.resolvableBody())
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500, got %d: %s", rr.Code, rr.Body.String())
		}

		assertNoFanout(t, f, o, before)
	})
}

func assertNoFanout(t *testing.T, f *copyPreflightFixture, o *copyFanoutObserver, before worldSnapshot) {
	t.Helper()
	if after := f.snapshot(); after != before {
		t.Errorf("a rolled-back copy mutated state:\n before: %+v\n after:  %+v", before, after)
	}
	if got := o.newActivity(); len(got) != 0 {
		t.Errorf("a rolled-back copy logged activity: %v", got)
	}
	if got := drainSSE(o.chA); len(got) != 0 {
		t.Errorf("a rolled-back copy published in workspace A: %+v", got)
	}
	if got := drainSSE(o.chB); len(got) != 0 {
		t.Errorf("a rolled-back copy published in workspace B: %+v", got)
	}
	if got := o.drainWebhooks(); len(got) != 0 {
		t.Errorf("a rolled-back copy dispatched webhooks: %+v", got)
	}
}

// TestCopyEndpoint_FanoutHarnessIsRealPositiveControl — the negative
// assertions above are only worth anything if the harness CAN observe an
// emission. Without this, a receiver that never worked would make every
// "emitted nothing" check pass vacuously.
func TestCopyEndpoint_FanoutHarnessIsRealPositiveControl(t *testing.T) {
	f := newCopyPreflightFixture(t)
	o := newCopyFanoutObserver(t, f)

	f.srv.dispatchWebhook(f.wsA.ID, "item.created", map[string]string{"probe": "A"})
	f.srv.dispatchWebhook(f.wsB.ID, "item.created", map[string]string{"probe": "B"})

	got := o.drainWebhooks()
	if len(webhookEventsFor(got, "A")) == 0 || len(webhookEventsFor(got, "B")) == 0 {
		t.Fatalf("the webhook harness cannot observe deliveries on both sides (%+v); "+
			"every negative fanout assertion in this file would pass vacuously", got)
	}
}

// --- 3. authorization ---------------------------------------------------

// TestCopyEndpoint_AuthorizationDenials runs the four-check ladder against
// the MUTATION. It is the preflight's set, re-asserted here rather than
// assumed: the two handlers share no code for this, only a shape, and the
// mutation is the one where getting it wrong writes data.
//
// EVERY subtest snapshots the world and asserts it is unchanged, not just
// that the status is right. A denial that returns 403 after having written
// the item is the failure that matters, and a status-only assertion cannot
// see it (Codex round 6).
func TestCopyEndpoint_AuthorizationDenials(t *testing.T) {
	t.Run("source item not visible", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		otherA := mustSchemaCollection(t, f.srv, f.wsA.ID, "Other A", srcSchemaJSON)
		u := f.restrictedEditor("copy-hidden-src@example.com", "copyhiddensrc",
			[]string{otherA.ID}, []string{f.collB.ID})

		before := f.snapshot()
		rr := f.callCopy(u, reqOpts{wsRoleCtx: "editor"}, f.resolvableBody())
		if rr.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
		}
		assertDisclosesNothing(t, rr, f)
		if after := f.snapshot(); after != before {
			t.Fatal("a refused copy wrote something")
		}
	})

	t.Run("source edit denied", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		viewer := mustUser(t, f.srv, "copy-viewer-a@example.com", "copyviewera", "")
		if err := f.srv.store.AddWorkspaceMember(f.wsA.ID, viewer.ID, "viewer"); err != nil {
			t.Fatalf("AddWorkspaceMember A: %v", err)
		}
		if err := f.srv.store.AddWorkspaceMember(f.wsB.ID, viewer.ID, "editor"); err != nil {
			t.Fatalf("AddWorkspaceMember B: %v", err)
		}
		before := f.snapshot()
		rr := f.callCopy(viewer, reqOpts{wsRoleCtx: "viewer"}, f.resolvableBody())
		if rr.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
		}
		assertDisclosesNothing(t, rr, f)
		if after := f.snapshot(); after != before {
			t.Fatal("a refused copy wrote something")
		}
	})

	t.Run("destination collection not visible is indistinguishable from absent", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		u := f.restrictedEditor("copy-hidden-dst@example.com", "copyhiddendst",
			[]string{f.collA.ID}, []string{f.collB.ID})

		before := f.snapshot()
		body := f.resolvableBody()
		body["target_collection"] = f.hiddenB.Slug
		hiddenRR := f.callCopy(u, reqOpts{wsRoleCtx: "editor"}, body)

		body = f.resolvableBody()
		body["target_collection"] = "definitely-not-a-collection"
		absentRR := f.callCopy(u, reqOpts{wsRoleCtx: "editor"}, body)

		if after := f.snapshot(); after != before {
			t.Fatal("a refused copy wrote something")
		}
		if hiddenRR.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d: %s", hiddenRR.Code, hiddenRR.Body.String())
		}
		if hiddenRR.Code != absentRR.Code || hiddenRR.Body.String() != absentRR.Body.String() {
			t.Fatalf("hidden and absent destination collections are distinguishable:\n hidden: %d %s\n absent: %d %s",
				hiddenRR.Code, hiddenRR.Body.String(), absentRR.Code, absentRR.Body.String())
		}
	})

	t.Run("destination edit denied", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		u := mustUser(t, f.srv, "copy-viewer-b@example.com", "copyviewerb", "")
		if err := f.srv.store.AddWorkspaceMember(f.wsA.ID, u.ID, "editor"); err != nil {
			t.Fatalf("AddWorkspaceMember A: %v", err)
		}
		if err := f.srv.store.AddWorkspaceMember(f.wsB.ID, u.ID, "viewer"); err != nil {
			t.Fatalf("AddWorkspaceMember B: %v", err)
		}
		before := f.snapshot()
		rr := f.callCopy(u, reqOpts{wsRoleCtx: "editor"}, f.resolvableBody())
		if rr.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d: %s", rr.Code, rr.Body.String())
		}
		if after := f.snapshot(); after != before {
			t.Fatal("a refused copy wrote something")
		}
	})

	t.Run("source check runs before destination", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		otherA := mustSchemaCollection(t, f.srv, f.wsA.ID, "Other A", srcSchemaJSON)
		u := mustUser(t, f.srv, "copy-both-fail@example.com", "copybothfail", "")
		if err := f.srv.store.AddWorkspaceMember(f.wsA.ID, u.ID, "editor"); err != nil {
			t.Fatalf("AddWorkspaceMember A: %v", err)
		}
		if err := f.srv.store.SetMemberCollectionAccess(f.wsA.ID, u.ID, "specific", []string{otherA.ID}); err != nil {
			t.Fatalf("SetMemberCollectionAccess: %v", err)
		}
		before := f.snapshot()
		rr := f.callCopy(u, reqOpts{wsRoleCtx: "editor"}, f.resolvableBody())
		// The caller is a stranger to workspace B, so the DESTINATION check
		// would refuse too — with a 403 and a different body. Getting the
		// source's 404 is therefore evidence about ORDER, not merely about
		// the set of checks: the two verdicts are distinguishable.
		if rr.Code != http.StatusNotFound {
			t.Fatalf("expected the SOURCE's 404 to win, got %d: %s", rr.Code, rr.Body.String())
		}
		assertDisclosesNothing(t, rr, f)
		if after := f.snapshot(); after != before {
			t.Fatal("a refused copy wrote something")
		}
	})

	t.Run("token allow-list is enforced on the destination", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		before := f.snapshot()
		rr := f.callCopy(f.owner, reqOpts{
			bearer: true, setAllowed: true, allowed: []string{f.wsA.Slug},
		}, f.resolvableBody())
		if rr.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d: %s", rr.Code, rr.Body.String())
		}
		if got := errCode(t, rr); got != "permission_denied" {
			t.Errorf("error code = %q, want permission_denied", got)
		}
		if after := f.snapshot(); after != before {
			t.Fatal("a consent-denied copy wrote something")
		}
	})

	t.Run("archived source reports 409", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		if err := f.srv.store.DeleteItem(f.source.ID); err != nil {
			t.Fatalf("DeleteItem: %v", err)
		}
		before := f.snapshot()
		rr := f.callCopy(f.owner, reqOpts{}, f.resolvableBody())
		if rr.Code != http.StatusConflict || errCode(t, rr) != "archived" {
			t.Fatalf("expected 409 archived, got %d %s", rr.Code, rr.Body.String())
		}
		if after := f.snapshot(); after != before {
			t.Fatal("a refused copy wrote something")
		}
	})
}

// TestCopyEndpoint_InTransactionReCheckRefuses is DR-9's re-check, driven
// END TO END rather than by calling the closure directly.
//
// The race is made deterministic with the copyItemFn seam: the source item
// is moved into a DIFFERENT collection after the handler's four-check ladder
// has already passed and before the store is entered, which is precisely the
// interleaving the re-check exists for. Without the store invoking PreCheck
// — or invoking it before it re-reads the source under the locks, or after
// the first write — the copy would go through and this test fails on both
// the status and the destination's item count (Codex round 6: the previous
// version called the closure directly and proved nothing about the store's
// integration).
//
// The item is moved into a collection the caller CANNOT see, so the refusal
// is also the real DR-10b escalation and not merely a bookkeeping mismatch.
func TestCopyEndpoint_InTransactionReCheckRefuses(t *testing.T) {
	f := newCopyPreflightFixture(t)

	hiddenA := mustSchemaCollection(t, f.srv, f.wsA.ID, "Hidden A", srcSchemaJSON)
	u := f.restrictedEditor("copy-recheck@example.com", "copyrecheck",
		[]string{f.collA.ID}, []string{f.collB.ID})

	// Baseline: this caller CAN copy, so the refusal below is about the
	// re-check and not about the caller.
	if rr := f.callCopy(u, reqOpts{wsRoleCtx: "editor"}, f.resolvableBody()); rr.Code != http.StatusCreated {
		t.Fatalf("baseline copy should succeed, got %d: %s", rr.Code, rr.Body.String())
	}

	// Counted rather than snapshotted: the interleaved MoveItem below is a
	// deliberate write of its own (it bumps workspace A's seq), so a
	// whole-world snapshot would flag the setup instead of the copy. What
	// must not change is what the COPY would have produced.
	destItems := func() int {
		return f.countRows(`SELECT COUNT(*) FROM items WHERE workspace_id = '` + f.wsB.ID + `'`)
	}
	beforeDest := destItems()
	beforeMoves := f.countRows(`SELECT COUNT(*) FROM item_workspace_moves`)

	f.srv.copyItemFn = func(req store.CrossWorkspaceCopyRequest) (*store.CrossWorkspaceCopyResult, error) {
		// Lands AFTER the handler authorized the item in collA and BEFORE the
		// store takes its locks and re-reads it.
		if _, err := f.srv.store.MoveItem(f.source.ID, hiddenA.ID, f.source.Fields); err != nil {
			return nil, err
		}
		return f.srv.store.CopyItemAcrossWorkspaces(req)
	}

	rr := f.callCopy(u, reqOpts{wsRoleCtx: "editor"}, f.resolvableBody())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("a source that moved into a hidden collection under the lock was copied anyway "+
			"(status %d): %s", rr.Code, rr.Body.String())
	}
	// The SOURCE side's non-disclosing refusal, not a 500 and not a message
	// that names the collection it moved into.
	if errCode(t, rr) != "not_found" {
		t.Errorf("error code = %q, want the source side's not_found", errCode(t, rr))
	}
	assertDisclosesNothing(t, rr, f)

	// Nothing landed. This is the assertion that fails if the store stopped
	// calling PreCheck, called it before re-reading the source under the
	// lock, or called it after the first write.
	if got := destItems(); got != beforeDest {
		t.Fatalf("the refused copy created %d item(s) in the destination", got-beforeDest)
	}
	if got := f.countRows(`SELECT COUNT(*) FROM item_workspace_moves`); got != beforeMoves {
		t.Fatalf("the refused copy wrote %d provenance row(s)", got-beforeMoves)
	}
}

// TestCopyEndpoint_ReCheckRefusesAForeignDestinationCollection is the other
// half of the invariant: the destination collection the copy locks must be
// the one that was authorized, in the workspace that was authorized.
func TestCopyEndpoint_ReCheckRefusesAForeignDestinationCollection(t *testing.T) {
	f := newCopyPreflightFixture(t)

	precheck := copyResourceInvariantPreCheck(copyAuthorizedResources{
		sourceItemID:       f.source.ID,
		sourceWorkspaceID:  f.wsA.ID,
		sourceCollectionID: f.collA.ID,
		targetCollectionID: f.collB.ID,
		targetWorkspaceID:  f.wsB.ID,
	})

	if err := precheck(nil, f.source, f.collB); err != nil {
		t.Fatalf("the unchanged case must pass: %v", err)
	}
	for _, tc := range []struct {
		name string
		coll *models.Collection
		side string
	}{
		{"a different collection entirely", f.hiddenB, "destination"},
		{"the right id in the wrong workspace", &models.Collection{ID: f.collB.ID, WorkspaceID: f.wsA.ID}, "destination"},
		{"nil", nil, "destination"},
	} {
		err := precheck(nil, f.source, tc.coll)
		if err == nil {
			t.Fatalf("%s: the re-check accepted it", tc.name)
		}
		// Returned bare — the STORE wraps it in CopyPreCheckError, which is
		// what makes it a caller-facing rejection rather than a logged
		// incident. That wrapping is pinned on the store side by
		// TestCopyAcrossWorkspaces_PreCheckRefusalIsWrapped.
		var denial *copyPreCheckDenial
		if !errors.As(err, &denial) || denial.side != tc.side {
			t.Fatalf("%s: denial = %+v, want side %q", tc.name, denial, tc.side)
		}
	}
}

// TestCopyEndpoint_PreCheckIsActuallyWired guards the wiring rather than the
// logic: a PreCheck that is built and never passed to the store would leave
// every assertion above true and the guarantee absent.
func TestCopyEndpoint_PreCheckIsActuallyWired(t *testing.T) {
	f := newCopyPreflightFixture(t)

	var got store.CrossWorkspaceCopyRequest
	f.srv.copyItemFn = func(req store.CrossWorkspaceCopyRequest) (*store.CrossWorkspaceCopyResult, error) {
		got = req
		return f.srv.store.CopyItemAcrossWorkspaces(req)
	}
	f.copyOK(f.resolvableBody())

	if got.PreCheck == nil {
		t.Fatal("the copy request carries no PreCheck; DR-9's in-transaction re-check is not wired")
	}
	// ...and the one that was wired is bound to THIS request's resources, not
	// a permissive stub: hand it a foreign item and it must refuse.
	if err := got.PreCheck(nil, &models.Item{
		ID: "someone-elses-item", WorkspaceID: f.wsA.ID, CollectionID: f.collA.ID,
	}, f.collB); err == nil {
		t.Error("the wired PreCheck accepts an item other than the one that was authorized")
	}
	// Caller obligation from CrossWorkspaceCopyResult: s.cloudMode, never an
	// unconditional true. testServer is self-hosted, so this must be false —
	// forcing it on would apply free-tier item caps to self-hosted users.
	if f.srv.cloudMode {
		t.Fatal("fixture precondition: testServer should not be in cloud mode")
	}
	if got.EnforceItemLimit {
		t.Error("EnforceItemLimit is true on a SELF-HOSTED server; it must track s.cloudMode")
	}
	// Left empty so the copy cannot refuse something its own preview
	// accepted — the preflight does not set it either.
	if got.TargetBackend != "" {
		t.Errorf("TargetBackend = %q; the preflight leaves it empty and the two must agree", got.TargetBackend)
	}
}

// TestCopyEndpoint_EnforceItemLimitFollowsCloudMode is the other half of the
// same obligation.
func TestCopyEndpoint_EnforceItemLimitFollowsCloudMode(t *testing.T) {
	f := newCopyPreflightFixture(t)
	f.srv.cloudMode = true

	var got store.CrossWorkspaceCopyRequest
	f.srv.copyItemFn = func(req store.CrossWorkspaceCopyRequest) (*store.CrossWorkspaceCopyResult, error) {
		got = req
		return f.srv.store.CopyItemAcrossWorkspaces(req)
	}
	f.copyOK(f.resolvableBody())

	if !got.EnforceItemLimit {
		t.Error("EnforceItemLimit is false in CLOUD mode; the destination quota would not be enforced")
	}
}

// --- 4. DR-13: never retry ----------------------------------------------

// TestCopyEndpoint_DoesNotRetryOnAmbiguousError is the executable form of
// DR-13. There is no idempotency key in v1, so a retry after a failure that
// may have committed produces a DUPLICATE item — and a client that timed out
// cannot tell the two apart. The store op must therefore be invoked exactly
// once per request, whatever comes back.
//
// Error shapes are varied on purpose: a plausible "transient" error is the
// one a future maintainer is most likely to add a retry for, so it is named
// here.
func TestCopyEndpoint_DoesNotRetryOnAmbiguousError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"looks transient", errors.New("driver: bad connection")},
		{"looks like a deadlock", errors.New("ERROR: deadlock detected (SQLSTATE 40P01)")},
		{"commit failed ambiguously", fmt.Errorf("copy item across workspaces: commit: %w", errors.New("i/o timeout"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCopyPreflightFixture(t)
			calls := 0
			f.srv.copyItemFn = func(store.CrossWorkspaceCopyRequest) (*store.CrossWorkspaceCopyResult, error) {
				calls++
				return nil, tc.err
			}

			rr := f.callCopy(f.owner, reqOpts{}, f.resolvableBody())
			if calls != 1 {
				t.Fatalf("the store op was invoked %d times; a mutating copy must never be retried "+
					"(there is no idempotency key, so a retry duplicates the item)", calls)
			}
			if rr.Code != http.StatusInternalServerError {
				t.Fatalf("expected 500, got %d: %s", rr.Code, rr.Body.String())
			}
			if got := errCode(t, rr); got != "copy_failed" {
				t.Errorf("error code = %q, want copy_failed", got)
			}
			// The message must send the user to LOOK, not to retry: the copy
			// may well have landed.
			if !bytes.Contains(rr.Body.Bytes(), []byte("check the destination")) {
				t.Errorf("the ambiguous-failure message does not tell the user to check the destination: %s",
					rr.Body.String())
			}
			// And it must not leak the underlying error to the client.
			if bytes.Contains(rr.Body.Bytes(), []byte("SQLSTATE")) ||
				bytes.Contains(rr.Body.Bytes(), []byte("bad connection")) {
				t.Errorf("the 500 echoed the driver error: %s", rr.Body.String())
			}
		})
	}
}

// TestCopyEndpoint_TypedStoreErrorsMapToStatuses covers the mapping
// obligations CrossWorkspaceCopyResult documents, for the two that cannot be
// produced from a real fixture on a self-hosted SQLite server.
func TestCopyEndpoint_TypedStoreErrorsMapToStatuses(t *testing.T) {
	f := newCopyPreflightFixture(t)

	t.Run("item limit renders the plan-limit payload", func(t *testing.T) {
		f.srv.copyItemFn = func(store.CrossWorkspaceCopyRequest) (*store.CrossWorkspaceCopyResult, error) {
			return nil, &store.ItemLimitError{Result: &store.LimitResult{
				Feature: "items_per_workspace", Limit: 100, Current: 100, Plan: "free",
			}}
		}
		rr := f.callCopy(f.owner, reqOpts{}, f.resolvableBody())
		if rr.Code != http.StatusForbidden || errCode(t, rr) != "plan_limit_exceeded" {
			t.Fatalf("got %d %s, want 403 plan_limit_exceeded", rr.Code, rr.Body.String())
		}
	})

	t.Run("cross-backend attachments is a clear 4xx", func(t *testing.T) {
		f.srv.copyItemFn = func(store.CrossWorkspaceCopyRequest) (*store.CrossWorkspaceCopyResult, error) {
			return nil, store.ErrCopyCrossBackendAttachments
		}
		rr := f.callCopy(f.owner, reqOpts{}, f.resolvableBody())
		if rr.Code < 400 || rr.Code >= 500 {
			t.Fatalf("got %d, want a 4xx: %s", rr.Code, rr.Body.String())
		}
		if errCode(t, rr) != "cross_backend_attachments" {
			t.Fatalf("error code = %q, want cross_backend_attachments", errCode(t, rr))
		}
	})

	// MAPPING ONLY, and the subtest names now say so. These INJECT the store's
	// sentinels rather than reproducing a real lock-time disappearance, which
	// no test can schedule deterministically; DETECTION is covered on the
	// store side by TestCopyAcrossWorkspaces_CollectionMissingSentinels, which
	// soft-deletes each collection for real and asserts the sentinel with
	// errors.Is (Codex round 7 — the claim used to be unsupported).
	// What is asserted here is the half the handler owns: a
	// pre-write rejection with a precise answer must never fall through to
	// the ambiguous 500 and send the user looking for an item that provably
	// does not exist (Codex rounds 1 and 6).
	t.Run("ErrCopyTargetCollectionMissing maps to collection_not_found", func(t *testing.T) {
		f.srv.copyItemFn = func(store.CrossWorkspaceCopyRequest) (*store.CrossWorkspaceCopyResult, error) {
			return nil, store.ErrCopyTargetCollectionMissing
		}
		rr := f.callCopy(f.owner, reqOpts{}, f.resolvableBody())
		if rr.Code != http.StatusNotFound || errCode(t, rr) != crossWorkspaceCollectionNotFoundCode {
			t.Fatalf("got %d %s, want 404 %s", rr.Code, rr.Body.String(), crossWorkspaceCollectionNotFoundCode)
		}
		// Byte-identical to the answer for a collection that never existed —
		// otherwise the timing distinguishes hidden from absent.
		body := f.resolvableBody()
		body["target_collection"] = "definitely-not-a-collection"
		f.srv.copyItemFn = nil
		absent := f.callCopy(f.owner, reqOpts{}, body)
		if absent.Body.String() != rr.Body.String() {
			t.Errorf("vanished and absent destination collections are distinguishable:\n vanished: %s\n absent:   %s",
				rr.Body.String(), absent.Body.String())
		}
	})

	t.Run("ErrCopySourceCollectionMissing maps to the source's bare 404", func(t *testing.T) {
		f.srv.copyItemFn = func(store.CrossWorkspaceCopyRequest) (*store.CrossWorkspaceCopyResult, error) {
			return nil, store.ErrCopySourceCollectionMissing
		}
		rr := f.callCopy(f.owner, reqOpts{}, f.resolvableBody())
		if rr.Code != http.StatusNotFound || errCode(t, rr) != "not_found" {
			t.Fatalf("got %d %s, want 404 not_found", rr.Code, rr.Body.String())
		}
		if bytes.Contains(rr.Body.Bytes(), []byte("collection")) {
			t.Errorf("the source-side refusal names a collection: %s", rr.Body.String())
		}
	})

	t.Run("sql.ErrNoRows maps to a 404", func(t *testing.T) {
		f.srv.copyItemFn = func(store.CrossWorkspaceCopyRequest) (*store.CrossWorkspaceCopyResult, error) {
			return nil, sql.ErrNoRows
		}
		rr := f.callCopy(f.owner, reqOpts{}, f.resolvableBody())
		if rr.Code != http.StatusNotFound {
			t.Fatalf("got %d, want 404: %s", rr.Code, rr.Body.String())
		}
	})
}

// TestCopyEndpoint_UsesLockedCollectionSnapshots — the store re-reads both
// collection rows under a FOR UPDATE pin and returns them precisely so the
// caller does not build its answer from a pre-transaction read that can be
// stale (Codex round 1 P2).
//
// Staleness is forced rather than raced: the copyItemFn seam RENAMES the
// source's collection between the handler's read and the store call, which is
// what a concurrent collection edit looks like from the handler's point of
// view. The rename is chosen over a move deliberately — a move is refused
// outright by the in-tx invariant re-check, so the only observable staleness
// left is a slug that changed under a collection whose identity did not.
func TestCopyEndpoint_UsesLockedCollectionSnapshots(t *testing.T) {
	f := newCopyPreflightFixture(t)
	o := newCopyFanoutObserver(t, f)

	const renamed = "renamed-under-the-lock"
	f.srv.copyItemFn = func(req store.CrossWorkspaceCopyRequest) (*store.CrossWorkspaceCopyResult, error) {
		if _, err := f.srv.store.DB().Exec(
			f.srv.store.D().Rebind(`UPDATE collections SET slug = ? WHERE id = ?`), renamed, f.collA.ID,
		); err != nil {
			return nil, err
		}
		return f.srv.store.CopyItemAcrossWorkspaces(req)
	}

	body := f.resolvableBody()
	body["archive_source"] = true
	res := f.copyOK(body)

	if res.Source.CollectionSlug != renamed {
		t.Errorf("source.collection_slug = %q, want the under-lock %q — the response was built "+
			"from a pre-transaction read", res.Source.CollectionSlug, renamed)
	}
	evA := drainSSE(o.chA)
	if len(evA) != 1 {
		t.Fatalf("workspace A SSE = %+v, want exactly one archive event", evA)
	}
	if evA[0].Collection != renamed {
		t.Errorf("A's archive event is attributed to collection %q, want the under-lock %q — "+
			"an SSE consumer routing on the collection would file the tombstone in the wrong place",
			evA[0].Collection, renamed)
	}
}

// --- 5. caller obligations ----------------------------------------------

// TestCopyEndpoint_InvalidatesDestinationStorageCache — CrossWorkspaceCopy
// Result states that storageInfoCache invalidation belongs to the caller
// because the store has no handle on it. Without it the destination's
// storage page reports stale usage for the cache's 30-second window, right
// after the user watched the bytes land.
//
// The SOURCE's entry must survive: the copy adds rows in B and removes
// nothing from A, so invalidating A would be a needless recompute
// masquerading as correctness.
func TestCopyEndpoint_InvalidatesDestinationStorageCache(t *testing.T) {
	f := newCopyPreflightFixture(t)

	att := &models.Attachment{
		WorkspaceID: f.wsA.ID, UploadedBy: f.owner.ID,
		StorageKey: "fs:feedface", ContentHash: "feedface",
		MimeType: "image/png", SizeBytes: 2048, Filename: "s.png",
	}
	if err := f.srv.store.CreateAttachment(att); err != nil {
		t.Fatalf("CreateAttachment: %v", err)
	}
	content := "![](pad-attachment:" + att.ID + ")"
	if _, err := f.srv.store.UpdateItem(f.source.ID, models.ItemUpdate{Content: &content}); err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}

	// Prime both entries with sentinel values a recompute could never
	// produce, so "still cached" and "recomputed to the same number" are
	// distinguishable.
	f.srv.storageInfoCache.set(f.wsA.ID, &store.WorkspaceStorageInfo{UsedBytes: 111111})
	f.srv.storageInfoCache.set(f.wsB.ID, &store.WorkspaceStorageInfo{UsedBytes: 222222})

	res := f.copyOK(f.resolvableBody())
	if res.Warnings.AttachmentCount == 0 {
		t.Fatal("fixture precondition: the copy cloned no attachments, so nothing would be invalidated")
	}

	if f.srv.storageInfoCache.get(f.wsB.ID) != nil {
		t.Error("the destination's storage cache entry survived a copy that added attachment rows")
	}
	if f.srv.storageInfoCache.get(f.wsA.ID) == nil {
		t.Error("the SOURCE's storage cache entry was invalidated; the copy changed nothing in A")
	}
}

// TestCopyEndpoint_PostCommitPanicStillReturns201 — by the time the
// post-commit work runs, the copy is committed and IRREVERSIBLE, but the
// response has not been written. A panic escaping into chi's recoverer would
// turn a succeeded copy into a 500, and DR-13 forbids the client from
// resolving that ambiguity by retrying — so the retry would duplicate the
// item, or the user would be told a copy failed that plainly did not.
//
// The panic is induced in the CACHE INVALIDATION rather than the fanout,
// because that is the step Codex round 3 found sitting outside the guard.
func TestCopyEndpoint_PostCommitPanicStillReturns201(t *testing.T) {
	f := newCopyPreflightFixture(t)

	att := &models.Attachment{
		WorkspaceID: f.wsA.ID, UploadedBy: f.owner.ID,
		StorageKey: "fs:c0ffee", ContentHash: "c0ffee",
		MimeType: "image/png", SizeBytes: 64, Filename: "p.png",
	}
	if err := f.srv.store.CreateAttachment(att); err != nil {
		t.Fatalf("CreateAttachment: %v", err)
	}
	content := "![](pad-attachment:" + att.ID + ")"
	if _, err := f.srv.store.UpdateItem(f.source.ID, models.ItemUpdate{Content: &content}); err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}

	// A nil cache makes invalidate() nil-deref. Crude on purpose: the point
	// is that ANY panic in post-commit work is contained, not that this
	// particular one is expected.
	f.srv.storageInfoCache = nil

	rr := f.callCopy(f.owner, reqOpts{}, f.resolvableBody())
	if rr.Code != http.StatusCreated {
		t.Fatalf("a panic in post-commit work changed the response to %d: %s", rr.Code, rr.Body.String())
	}

	// And the copy really is there — "201 with nothing behind it" would be a
	// worse outcome than the 500.
	var out ItemCopyResult
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if item, err := f.srv.store.GetItem(out.Item.ID); err != nil || item == nil {
		t.Fatalf("the committed copy is not readable: %v", err)
	}
}

// TestCopyEndpoint_NoAttachmentsLeavesTheCacheAlone — the invalidation is
// conditional on attachments actually being copied, so the common case does
// not pay for a recompute.
func TestCopyEndpoint_NoAttachmentsLeavesTheCacheAlone(t *testing.T) {
	f := newCopyPreflightFixture(t)
	f.srv.storageInfoCache.set(f.wsB.ID, &store.WorkspaceStorageInfo{UsedBytes: 222222})

	res := f.copyOK(f.resolvableBody())
	if res.Warnings.AttachmentCount != 0 {
		t.Fatalf("fixture precondition: expected no attachments, got %d", res.Warnings.AttachmentCount)
	}
	if f.srv.storageInfoCache.get(f.wsB.ID) == nil {
		t.Error("an attachment-free copy invalidated the destination's storage cache")
	}
}

// --- response contract --------------------------------------------------

// TestCopyEndpoint_ResponseIsNavigable pins the acceptance criterion that
// TASK-2366 (the CLI) and Phase 3 (the dialog) build against: enough to
// reach the copy, and on a move enough to leave the source.
func TestCopyEndpoint_ResponseIsNavigable(t *testing.T) {
	f := newCopyPreflightFixture(t)
	res := f.copyOK(f.resolvableBody())

	if res.Destination.WorkspaceSlug != f.wsB.Slug {
		t.Errorf("destination.workspace_slug = %q, want %q", res.Destination.WorkspaceSlug, f.wsB.Slug)
	}
	if res.Destination.CollectionSlug != f.collB.Slug {
		t.Errorf("destination.collection_slug = %q, want %q", res.Destination.CollectionSlug, f.collB.Slug)
	}
	if res.Destination.Ref == "" || res.Destination.Slug == "" {
		t.Errorf("destination block is not navigable: %+v", res.Destination)
	}
	if res.Item == nil || res.Item.ID == "" || res.Item.WorkspaceID != f.wsB.ID {
		t.Fatalf("item block = %+v, want the destination item", res.Item)
	}
	if res.Item.Ref != res.Destination.Ref {
		t.Errorf("item.ref %q disagrees with destination.ref %q", res.Item.Ref, res.Destination.Ref)
	}
	if res.Source.WorkspaceSlug != f.wsA.Slug || res.Source.Slug != f.source.Slug {
		t.Errorf("source block = %+v, want workspace %q slug %q", res.Source, f.wsA.Slug, f.source.Slug)
	}
	if res.ArchiveSource {
		t.Error("archive_source echoed true for a plain copy")
	}
	if res.Warnings.DroppedFields == nil {
		t.Error("dropped_fields is null on the wire; it must be [] like every other list in this pair")
	}
	// The source is still live after a plain copy.
	if live, err := f.srv.store.GetItem(f.source.ID); err != nil || live == nil {
		t.Errorf("a plain copy removed the source: %v", err)
	}
}

// TestCopyEndpoint_ReportsCanonicalWorkspaceSlugs — /workspaces/{slug} also
// accepts a workspace UUID, so a response that echoes the URL parameter hands
// a client a "workspace_slug" that is not a slug and a link that does not
// resolve (Codex round 14). Both endpoints report the RESOLVED canonical slug,
// and both are checked here because they must agree.
func TestCopyEndpoint_ReportsCanonicalWorkspaceSlugs(t *testing.T) {
	f := newCopyPreflightFixture(t)

	// Address the source workspace by UUID, exactly as the router permits.
	body, err := json.Marshal(f.resolvableBody())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	newReq := func() *http.Request {
		r := httptest.NewRequest("POST",
			"/api/v1/workspaces/"+f.wsA.ID+"/items/"+f.source.Slug+"/copy",
			bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		ctx := WithCurrentUser(r.Context(), f.owner)
		ctx = contextWithWorkspaceRoleForTest(ctx, "owner")
		ctx = contextWithResolvedWorkspaceIDForTest(ctx, f.wsA.ID)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("slug", f.wsA.ID) // the UUID form
		rctx.URLParams.Add("itemSlug", f.source.Slug)
		return r.WithContext(context.WithValue(ctx, chi.RouteCtxKey, rctx))
	}

	preRR := httptest.NewRecorder()
	f.srv.handleCopyItemPreflight(preRR, newReq())
	if preRR.Code != http.StatusOK {
		t.Fatalf("preflight: %d %s", preRR.Code, preRR.Body.String())
	}
	var pre ItemCopyPreflight
	if err := json.Unmarshal(preRR.Body.Bytes(), &pre); err != nil {
		t.Fatalf("parse preflight: %v", err)
	}
	if pre.Source.WorkspaceSlug != f.wsA.Slug {
		t.Errorf("preflight source.workspace_slug = %q, want the canonical slug %q",
			pre.Source.WorkspaceSlug, f.wsA.Slug)
	}

	copyRR := httptest.NewRecorder()
	f.srv.handleCopyItem(copyRR, newReq())
	if copyRR.Code != http.StatusCreated {
		t.Fatalf("copy: %d %s", copyRR.Code, copyRR.Body.String())
	}
	var res ItemCopyResult
	if err := json.Unmarshal(copyRR.Body.Bytes(), &res); err != nil {
		t.Fatalf("parse copy: %v", err)
	}
	if res.Source.WorkspaceSlug != f.wsA.Slug {
		t.Errorf("copy source.workspace_slug = %q, want the canonical slug %q",
			res.Source.WorkspaceSlug, f.wsA.Slug)
	}
	if res.Source.WorkspaceSlug != pre.Source.WorkspaceSlug {
		t.Errorf("the preview and the copy report different source workspaces: %q vs %q",
			pre.Source.WorkspaceSlug, res.Source.WorkspaceSlug)
	}
}

// TestCopyEndpoint_MoveArchivesTheSource — the other half, at the data
// layer rather than the response: the source is gone from live reads and the
// provenance row records where it went.
func TestCopyEndpoint_MoveArchivesTheSource(t *testing.T) {
	f := newCopyPreflightFixture(t)
	body := f.resolvableBody()
	body["archive_source"] = true
	res := f.copyOK(body)

	if live, err := f.srv.store.GetItem(f.source.ID); err != nil || live != nil {
		t.Errorf("the source is still live after a move (err=%v)", err)
	}
	moves, err := f.srv.store.ListArchivedItemWorkspaceMovesBySource(f.source.ID, 10)
	if err != nil {
		t.Fatalf("ListArchivedItemWorkspaceMovesBySource: %v", err)
	}
	if len(moves) != 1 || moves[0].TargetItemID != res.Item.ID || !moves[0].ArchivedSource {
		t.Fatalf("provenance rows = %+v, want one archived pointer at %s", moves, res.Item.ID)
	}
}

// TestCopyEndpoint_RouteIsWired drives the real router, so a handler that
// works in isolation but was never mounted fails here.
func TestCopyEndpoint_RouteIsWired(t *testing.T) {
	srv := testServer(t)
	wsA := createWSForTest(t, srv)
	wsARow, err := srv.store.GetWorkspaceBySlug(wsA)
	if err != nil || wsARow == nil {
		t.Fatalf("GetWorkspaceBySlug: %v", err)
	}
	collA := mustSchemaCollection(t, srv, wsARow.ID, "Route Src", srcSchemaJSON)
	collB := mustSchemaCollection(t, srv, wsARow.ID, "Route Dst", srcSchemaJSON)
	item := createItem(t, srv, wsA, collA.Slug, map[string]interface{}{
		"title": "Routed", "fields": `{"status":"open"}`,
	})

	rr := doRequest(srv, "POST",
		"/api/v1/workspaces/"+wsA+"/items/"+item.Slug+"/copy",
		map[string]interface{}{
			"target_workspace":  wsA,
			"target_collection": collB.Slug,
		})
	if rr.Code != http.StatusCreated {
		t.Fatalf("routed copy: got %d, want 201: %s", rr.Code, rr.Body.String())
	}
	var resp ItemCopyResult
	parseJSON(t, rr, &resp)
	if resp.Destination.CollectionSlug != collB.Slug || resp.Item == nil {
		t.Fatalf("routed copy response = %+v", resp)
	}
	// Same-workspace copy is a legal degenerate case and lands a second,
	// independent item — the endpoint is not a move-within-a-workspace
	// shortcut and must not be mistaken for one.
	if resp.Item.ID == item.ID {
		t.Error("the copy returned the SOURCE item; nothing was created")
	}
}

// BUG-2674 / Codex round 2 P2-3. The scope rule was proved at the MigrateFields
// unit level and at the preflight, but nothing proved the MUTATING copy honours
// it — so a call site passing the wrong scope, which is exactly the mistake the
// required argument exists to prevent, would have shipped green.
//
// Both directions in one test, deliberately: the interesting property is the
// DIFFERENCE between them, and either half alone is satisfiable by a constant.
func TestCopyEndpoint_ReferentialMetadataTravelsOnlyWithinItsWorkspace(t *testing.T) {
	const pr = `{"number":42,"url":"https://example.invalid/42","state":"OPEN","repo":"acme/source-repo"}`
	const notes = `[{"id":"note-1","summary":"history is true anywhere"}]`

	seed := func(f *copyPreflightFixture) {
		t.Helper()
		fields := `{"status":"open","priority":"low","impact":"large","count":7,"code":"abc",` +
			`"github_pr":` + pr + `,"implementation_notes":` + notes + `}`
		if _, err := f.srv.store.UpdateItem(f.source.ID, models.ItemUpdate{Fields: &fields}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	t.Run("cross-workspace copy leaves the PR link behind", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		seed(f)

		res := f.copyOK(f.resolvableBody()) // baseBody targets wsB
		got := f.persistedFields(res.Item.ID)

		if _, ok := got[models.ItemFieldGitHubPR]; ok {
			t.Errorf("github_pr must not follow an item into another workspace — "+
				"it names the SOURCE project's repo and renders as a live link on the copy: %#v", got)
		}
		if _, ok := got[models.ItemFieldImplementationNotes]; !ok {
			t.Errorf("implementation_notes describes the item, not its surroundings, and must travel: %#v", got)
		}
	})

	t.Run("same-workspace copy keeps it", func(t *testing.T) {
		f := newCopyPreflightFixture(t)
		seed(f)

		// Same workspace, copied into its OWN collection — the repo context
		// around the item is unchanged, so the referent still describes
		// something true. No field_overrides: resolvableBody's are keyed to
		// the DESTINATION schema of the cross-workspace fixture (collB), and
		// the copy correctly refuses an override the destination does not
		// declare. Migrating collA to collA needs none.
		body := f.baseBody()
		body["target_workspace"] = f.wsA.Slug
		body["target_collection"] = f.collA.Slug

		res := f.copyOK(body)
		got := f.persistedFields(res.Item.ID)

		if _, ok := got[models.ItemFieldGitHubPR]; !ok {
			t.Errorf("github_pr must survive a copy that never leaves the workspace; "+
				"dropping it here would destroy metadata on the strength of a scope that did not change: %#v", got)
		}
	})
}
