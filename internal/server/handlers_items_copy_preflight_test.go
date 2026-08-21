package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/webhooks"
)

// Tests for the cross-workspace copy preflight (PLAN-2357 / TASK-2364).
//
// Three properties carry the weight here and each has its own section:
//
//  1. DR-12 ordering — overrides are applied BEFORE validation, so an
//     override that satisfies a required field clears it and an override
//     with an invalid value is type-checked and refused.
//  2. DR-10a/DR-10b — all four authorization checks, in order, on the
//     DRY-RUN. A preflight that confirms a hidden source item exists, or
//     echoes a hidden destination's schema, is itself the leak.
//  3. DR-15 non-mutation — nothing is written, no seq moves, nothing is
//     emitted, and repeated calls are byte-identical.

// --- fixture -----------------------------------------------------------

type copyPreflightFixture struct {
	t   *testing.T
	srv *Server
	bus *events.MemoryBus

	owner *models.User

	wsA, wsB *models.Workspace
	collA    *models.Collection
	collB    *models.Collection
	// hiddenB is a collection in B that restricted members cannot see.
	hiddenB *models.Collection

	source *models.Item
}

// srcSchema / dstSchema are built to exercise every bucket at once:
//
//	status  — carries (select value present in both option sets)
//	priority— carries; also the field an INVALID override targets
//	impact  — dropped, no_target_field (absent from the destination)
//	count   — dropped, incompatible_type (number → select)
//	code    — needs_value/invalid_value: text→text carries the source's
//	          value, which the destination's pattern then rejects
//	ticket  — needs_value/missing_required (required, no default)
//	tier    — carries with from="default" (destination-only default)
const srcSchemaJSON = `{"fields":[
	{"key":"status","label":"Status","type":"select","options":["open","done"]},
	{"key":"priority","label":"Priority","type":"select","options":["low","high"]},
	{"key":"impact","label":"Impact","type":"text"},
	{"key":"count","label":"Count","type":"number"},
	{"key":"code","label":"Code","type":"text"}
]}`

const dstSchemaJSON = `{"fields":[
	{"key":"status","label":"Status","type":"select","options":["open","done"],"required":true},
	{"key":"priority","label":"Priority","type":"select","options":["low","high"]},
	{"key":"count","label":"Count","type":"select","options":["x","y"]},
	{"key":"code","label":"Code","type":"text","pattern":"^[0-9]+$"},
	{"key":"ticket","label":"Ticket","type":"text","required":true},
	{"key":"tier","label":"Tier","type":"select","options":["a","b"],"default":"a"}
]}`

func newCopyPreflightFixture(t *testing.T) *copyPreflightFixture {
	t.Helper()
	srv := testServer(t)
	bus := events.New()
	srv.SetEventBus(bus)
	t.Cleanup(bus.Close)

	owner := mustUser(t, srv, "copy-owner@example.com", "copyowner", "")
	wsA := mustWorkspace(t, srv, "Copy Source WS", owner.ID)
	wsB := mustWorkspace(t, srv, "Copy Dest WS", owner.ID)

	collA := mustSchemaCollection(t, srv, wsA.ID, "Tasks A", srcSchemaJSON)
	collB := mustSchemaCollection(t, srv, wsB.ID, "Tasks B", dstSchemaJSON)
	hiddenB := mustSchemaCollection(t, srv, wsB.ID, "Secrets B", dstSchemaJSON)

	source, err := srv.store.CreateItem(wsA.ID, collA.ID, models.ItemCreate{
		Title:     "The Source",
		Content:   "body",
		Fields:    `{"status":"open","priority":"low","impact":"large","count":7,"code":"abc"}`,
		CreatedBy: owner.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem(source): %v", err)
	}

	return &copyPreflightFixture{
		t: t, srv: srv, bus: bus, owner: owner,
		wsA: wsA, wsB: wsB, collA: collA, collB: collB, hiddenB: hiddenB,
		source: source,
	}
}

func mustSchemaCollection(t *testing.T, srv *Server, wsID, name, schema string) *models.Collection {
	t.Helper()
	c, err := srv.store.CreateCollection(wsID, models.CollectionCreate{Name: name, Schema: schema})
	if err != nil {
		t.Fatalf("CreateCollection(%s): %v", name, err)
	}
	return c
}

// call issues the preflight exactly as the router would, with the caller's
// auth surface described by o. The workspace-A role and resolved ID are
// stashed the way RequireWorkspaceAccess stashes them — the destination
// half of the handler must ignore both.
func (f *copyPreflightFixture) call(user *models.User, o reqOpts, body map[string]any) *httptest.ResponseRecorder {
	f.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		f.t.Fatalf("marshal body: %v", err)
	}
	r := httptest.NewRequest("POST",
		"/api/v1/workspaces/"+f.wsA.Slug+"/items/"+f.source.Slug+"/copy/preflight",
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
	f.srv.handleCopyItemPreflight(rr, r)
	return rr
}

// callRaw is call() with a body the caller controls byte for byte, for the
// undecodable-JSON case that a map[string]any cannot express.
func (f *copyPreflightFixture) callRaw(user *models.User, raw []byte) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest("POST",
		"/api/v1/workspaces/"+f.wsA.Slug+"/items/"+f.source.Slug+"/copy/preflight",
		bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")

	ctx := r.Context()
	if user != nil {
		ctx = WithCurrentUser(ctx, user)
	}
	ctx = contextWithWorkspaceRoleForTest(ctx, "owner")
	ctx = contextWithResolvedWorkspaceIDForTest(ctx, f.wsA.ID)

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("slug", f.wsA.Slug)
	rctx.URLParams.Add("itemSlug", f.source.Slug)
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)

	rr := httptest.NewRecorder()
	f.srv.handleCopyItemPreflight(rr, r.WithContext(ctx))
	return rr
}

// ok issues the preflight as the owner and requires a 200.
func (f *copyPreflightFixture) ok(body map[string]any) ItemCopyPreflight {
	f.t.Helper()
	rr := f.call(f.owner, reqOpts{}, body)
	if rr.Code != http.StatusOK {
		f.t.Fatalf("preflight: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var out ItemCopyPreflight
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		f.t.Fatalf("parse preflight: %v\nbody: %s", err, rr.Body.String())
	}
	return out
}

func (f *copyPreflightFixture) baseBody() map[string]any {
	return map[string]any{
		"target_workspace":  f.wsB.Slug,
		"target_collection": f.collB.Slug,
	}
}

func errCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse error envelope: %v\nbody: %s", err, rr.Body.String())
	}
	return env.Error.Code
}

func carriedByKey(t *testing.T, resp ItemCopyPreflight, key string) ItemCopyPreflightCarried {
	t.Helper()
	for _, c := range resp.Fields.Carried {
		if c.Key == key {
			return c
		}
	}
	t.Fatalf("expected %q in carried, got %+v", key, resp.Fields.Carried)
	return ItemCopyPreflightCarried{}
}

func droppedByKey(resp ItemCopyPreflight, key string) (ItemCopyPreflightDropped, bool) {
	for _, d := range resp.Fields.Dropped {
		if d.Key == key {
			return d, true
		}
	}
	return ItemCopyPreflightDropped{}, false
}

func needsValueByKey(resp ItemCopyPreflight, key string) (ItemCopyPreflightNeedsValue, bool) {
	for _, n := range resp.Fields.NeedsValue {
		if n.Key == key {
			return n, true
		}
	}
	return ItemCopyPreflightNeedsValue{}, false
}

// --- bucketing ---------------------------------------------------------

// TestCopyPreflight_Buckets is the DR-15 bucket-correctness case: a field
// that carries, one with no counterpart, one whose value the destination
// cannot take, and one required-and-unsatisfiable.
func TestCopyPreflight_Buckets(t *testing.T) {
	f := newCopyPreflightFixture(t)
	resp := f.ok(f.baseBody())

	if resp.Valid {
		t.Errorf("expected valid=false while needs_value is non-empty: %+v", resp.Fields.NeedsValue)
	}

	// carries, straight from the source
	if got := carriedByKey(t, resp, "status"); got.Value != "open" || got.From != "migrated" {
		t.Errorf("status: got %+v, want value=open from=migrated", got)
	}
	if got := carriedByKey(t, resp, "priority"); got.Value != "low" || got.From != "migrated" {
		t.Errorf("priority: got %+v, want value=low from=migrated", got)
	}
	// carries, filled in from the DESTINATION schema's default
	if got := carriedByKey(t, resp, "tier"); got.Value != "a" || got.From != "default" {
		t.Errorf("tier: got %+v, want value=a from=default", got)
	}

	// dropped — no counterpart in the destination schema at all
	d, ok := droppedByKey(resp, "impact")
	if !ok || d.Reason != "no_target_field" || d.Kind != "field" {
		t.Errorf("impact: got %+v (present=%v), want kind=field reason=no_target_field", d, ok)
	}
	if d.Label != "Impact" {
		t.Errorf("impact: label = %q, want the source schema's label", d.Label)
	}
	// dropped — the key exists downstream but the value cannot convert
	d, ok = droppedByKey(resp, "count")
	if !ok || d.Reason != "incompatible_type" {
		t.Errorf("count: got %+v (present=%v), want reason=incompatible_type", d, ok)
	}

	// needs_value — required in the destination, unsatisfiable from the source
	n, ok := needsValueByKey(resp, "ticket")
	if !ok || n.Reason != "missing_required" || !n.Required {
		t.Errorf("ticket: got %+v (present=%v), want reason=missing_required required=true", n, ok)
	}
	// needs_value — a value DID carry, and the destination schema rejects it
	n, ok = needsValueByKey(resp, "code")
	if !ok || n.Reason != "invalid_value" {
		t.Errorf("code: got %+v (present=%v), want reason=invalid_value", n, ok)
	}
	if n.Message == "" {
		t.Error("code: needs_value entry carries no message to display")
	}

	// A field in needs_value must NOT also appear in carried — the buckets
	// are a partition, and a client rendering both would show a value it is
	// simultaneously being asked to supply.
	for _, c := range resp.Fields.Carried {
		if c.Key == "ticket" || c.Key == "code" {
			t.Errorf("%s appears in BOTH carried and needs_value", c.Key)
		}
	}

	// Identity blocks, so a dialog can render a header without a second call.
	if resp.Source.Ref == "" || resp.Source.Title != "The Source" {
		t.Errorf("source block = %+v", resp.Source)
	}
	if resp.Destination.WorkspaceSlug != f.wsB.Slug || resp.Destination.CollectionSlug != f.collB.Slug {
		t.Errorf("destination block = %+v", resp.Destination)
	}
}

// TestCopyPreflight_OverrideClearsNeedsValue is the DR-12 bug in one test:
// MigrateFields computes its Errors BEFORE any override exists, so an
// implementation that reads them still reports `ticket` as missing after
// the caller has supplied it.
func TestCopyPreflight_OverrideClearsNeedsValue(t *testing.T) {
	f := newCopyPreflightFixture(t)

	body := f.baseBody()
	body["field_overrides"] = map[string]any{"ticket": "T-1", "code": "123"}
	resp := f.ok(body)

	if _, ok := needsValueByKey(resp, "ticket"); ok {
		t.Error("override supplied a value for `ticket` but it is still in needs_value (DR-12: overrides are applied BEFORE validation)")
	}
	if _, ok := needsValueByKey(resp, "code"); ok {
		t.Error("override replaced the invalid `code` value but it is still in needs_value")
	}
	if len(resp.Fields.NeedsValue) != 0 {
		t.Errorf("needs_value should be empty, got %+v", resp.Fields.NeedsValue)
	}
	if !resp.Valid {
		t.Error("valid should be true once needs_value is empty")
	}
	if got := carriedByKey(t, resp, "ticket"); got.Value != "T-1" || got.From != "override" {
		t.Errorf("ticket: got %+v, want value=T-1 from=override", got)
	}
	if got := carriedByKey(t, resp, "code"); got.Value != "123" || got.From != "override" {
		t.Errorf("code: got %+v, want value=123 from=override", got)
	}
}

// TestCopyPreflight_InvalidOverrideRejected is DR-12's other half: the
// override is type-checked, which the pre-fix move path never did.
func TestCopyPreflight_InvalidOverrideRejected(t *testing.T) {
	f := newCopyPreflightFixture(t)

	body := f.baseBody()
	// "urgent" is not one of the destination's priority options.
	body["field_overrides"] = map[string]any{"ticket": "T-1", "code": "123", "priority": "urgent"}

	rr := f.call(f.owner, reqOpts{}, body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an invalid override value, got %d: %s", rr.Code, rr.Body.String())
	}
	if code := errCode(t, rr); code != "invalid_override" {
		t.Errorf("error code = %q, want invalid_override", code)
	}
}

// TestCopyPreflight_MalformedOverrideRejected — an override naming a field
// the destination schema does not declare is refused rather than silently
// dropped, so a dialog cannot show a value that will never land.
func TestCopyPreflight_MalformedOverrideRejected(t *testing.T) {
	f := newCopyPreflightFixture(t)

	body := f.baseBody()
	body["field_overrides"] = map[string]any{"not_a_field": "x"}

	rr := f.call(f.owner, reqOpts{}, body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if code := errCode(t, rr); code != "malformed_override" {
		t.Errorf("error code = %q, want malformed_override", code)
	}
}

// TestCopyPreflight_NullOverrideUnsetsAndReappears pins the null semantics:
// an explicit null removes the key, which for a REQUIRED destination field
// means it comes back as needs_value rather than being written as nil.
func TestCopyPreflight_NullOverrideUnsetsAndReappears(t *testing.T) {
	f := newCopyPreflightFixture(t)

	body := f.baseBody()
	body["field_overrides"] = map[string]any{"ticket": "T-1", "code": "123", "status": nil}
	resp := f.ok(body)

	n, ok := needsValueByKey(resp, "status")
	if !ok || n.Reason != "missing_required" {
		t.Errorf("status: got %+v (present=%v), want reason=missing_required after a null override", n, ok)
	}
	if resp.Valid {
		t.Error("valid should be false while status is unsatisfied")
	}
}

// --- warnings ----------------------------------------------------------

// TestCopyPreflight_WarningsAllReported asserts every DR-15 warning with a
// non-zero value in one scenario, because the point of the set is that
// none of it may be silent (DR-17).
func TestCopyPreflight_WarningsAllReported(t *testing.T) {
	f := newCopyPreflightFixture(t)
	s := f.srv.store

	// A parent in A — the copy is unparented (DR-17).
	parent := mustItem(t, f.srv, f.wsA.ID, f.collA.ID, "The Parent")
	if _, err := s.SetParentLink(f.wsA.ID, f.source.ID, parent.ID, f.owner.ID); err != nil {
		t.Fatalf("SetParentLink: %v", err)
	}
	// A child in A — never copied, and orphaned in place on the move path.
	child := mustItem(t, f.srv, f.wsA.ID, f.collA.ID, "The Child")
	if _, err := s.SetParentLink(f.wsA.ID, child.ID, f.source.ID, f.owner.ID); err != nil {
		t.Fatalf("SetParentLink(child): %v", err)
	}
	// Dependency edges in both directions.
	blockee := mustItem(t, f.srv, f.wsA.ID, f.collA.ID, "Blockee")
	blocker := mustItem(t, f.srv, f.wsA.ID, f.collA.ID, "Blocker")
	if _, err := s.CreateItemLink(f.wsA.ID,
		models.ItemLinkCreate{TargetID: blockee.ID, LinkType: "blocks", CreatedBy: f.owner.ID}, f.source.ID); err != nil {
		t.Fatalf("CreateItemLink(outgoing): %v", err)
	}
	if _, err := s.CreateItemLink(f.wsA.ID,
		models.ItemLinkCreate{TargetID: f.source.ID, LinkType: "blocks", CreatedBy: f.owner.ID}, blocker.ID); err != nil {
		t.Fatalf("CreateItemLink(incoming): %v", err)
	}

	// An assignee who is a member of A but a stranger to B (DR-8), and an
	// agent role, which is workspace-local and never carries.
	stranger := mustUser(t, f.srv, "stranger@example.com", "strangeruser", "")
	if err := s.AddWorkspaceMember(f.wsA.ID, stranger.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	role, err := s.CreateAgentRole(f.wsA.ID, models.AgentRoleCreate{Name: "Reviewer"})
	if err != nil {
		t.Fatalf("CreateAgentRole: %v", err)
	}
	if _, err := s.UpdateItem(f.source.ID, models.ItemUpdate{
		AssignedUserID: &stranger.ID, AgentRoleID: &role.ID,
	}); err != nil {
		t.Fatalf("UpdateItem(assignment): %v", err)
	}

	// One live attachment referenced from the body, plus a reference that
	// resolves to nothing under the DR-11a scope.
	att := &models.Attachment{
		WorkspaceID: f.wsA.ID, UploadedBy: f.owner.ID,
		StorageKey: "fs:deadbeef", ContentHash: "deadbeef",
		MimeType: "image/png", SizeBytes: 4096, Filename: "shot.png",
	}
	if err := s.CreateAttachment(att); err != nil {
		t.Fatalf("CreateAttachment: %v", err)
	}
	newContent := "see ![](pad-attachment:" + att.ID + ") and ![](pad-attachment:00000000-0000-4000-8000-000000000000)"
	if _, err := s.UpdateItem(f.source.ID, models.ItemUpdate{Content: &newContent}); err != nil {
		t.Fatalf("UpdateItem(content): %v", err)
	}

	body := f.baseBody()
	body["archive_source"] = true
	w := f.ok(body).Warnings

	if w.ChildCount != 1 {
		t.Errorf("child_count = %d, want 1", w.ChildCount)
	}
	if !w.ChildrenOrphaned {
		t.Error("children_orphaned should be true on the archive_source (move) path")
	}
	if !w.DroppedParent {
		t.Error("dropped_parent should be true — the source has a parent and the copy is unparented")
	}
	if w.OutgoingLinks["blocks"] != 1 {
		t.Errorf("outgoing_links = %v, want blocks:1", w.OutgoingLinks)
	}
	if w.IncomingLinks["blocks"] != 1 {
		t.Errorf("incoming_links = %v, want blocks:1", w.IncomingLinks)
	}
	// The hierarchy edges are reported by child_count / dropped_parent and
	// must not be double-counted into the dependency maps.
	for _, hk := range []string{"parent", "implements", "plan"} {
		if _, ok := w.OutgoingLinks[hk]; ok {
			t.Errorf("outgoing_links leaked hierarchy type %q: %v", hk, w.OutgoingLinks)
		}
		if _, ok := w.IncomingLinks[hk]; ok {
			t.Errorf("incoming_links leaked hierarchy type %q: %v", hk, w.IncomingLinks)
		}
	}
	if !w.DroppedAssignee {
		t.Error("dropped_assignee should be true — the assignee is not a member of the destination")
	}
	if !w.DroppedAgentRole {
		t.Error("dropped_agent_role should be true — role slugs are workspace-local")
	}
	if w.AttachmentCount != 1 || w.AttachmentBytes != 4096 {
		t.Errorf("attachment_count/bytes = %d/%d, want 1/4096", w.AttachmentCount, w.AttachmentBytes)
	}
	if w.UnresolvableRefCount != 1 {
		t.Errorf("unresolvable_ref_count = %d, want 1", w.UnresolvableRefCount)
	}

	// The assignment pair must ALSO appear in the `dropped` bucket (DR-8
	// says so explicitly), not only as warning booleans.
	if d, ok := droppedByKey(f.ok(body), "assigned_user"); !ok || d.Kind != "assignment" || d.Reason != "assignee_not_a_member" {
		t.Errorf("assigned_user dropped entry = %+v (present=%v)", d, ok)
	}
	if d, ok := droppedByKey(f.ok(body), "agent_role"); !ok || d.Kind != "assignment" || d.Reason != "agent_role_not_portable" {
		t.Errorf("agent_role dropped entry = %+v (present=%v)", d, ok)
	}
}

// TestCopyPreflight_AssigneeCarriesWhenMemberOfDestination is DR-8's
// affirmative half: the common same-owner case loses nothing.
func TestCopyPreflight_AssigneeCarriesWhenMemberOfDestination(t *testing.T) {
	f := newCopyPreflightFixture(t)
	// The fixture owner is a member of BOTH workspaces.
	if _, err := f.srv.store.UpdateItem(f.source.ID, models.ItemUpdate{AssignedUserID: &f.owner.ID}); err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}

	resp := f.ok(f.baseBody())
	if resp.Warnings.DroppedAssignee {
		t.Error("dropped_assignee should be false when the assignee is a member of the destination")
	}
	if d, ok := droppedByKey(resp, "assigned_user"); ok {
		t.Errorf("assignee should not be in the dropped bucket: %+v", d)
	}
}

// TestCopyPreflight_ChildrenNotOrphanedOnPlainCopy — the weighting is the
// archive_source flag's job, not the client's.
func TestCopyPreflight_ChildrenNotOrphanedOnPlainCopy(t *testing.T) {
	f := newCopyPreflightFixture(t)
	child := mustItem(t, f.srv, f.wsA.ID, f.collA.ID, "Kid")
	if _, err := f.srv.store.SetParentLink(f.wsA.ID, child.ID, f.source.ID, f.owner.ID); err != nil {
		t.Fatalf("SetParentLink: %v", err)
	}

	resp := f.ok(f.baseBody())
	if resp.Warnings.ChildCount != 1 {
		t.Fatalf("child_count = %d, want 1", resp.Warnings.ChildCount)
	}
	if resp.Warnings.ChildrenOrphaned {
		t.Error("children_orphaned must be false on a plain copy — the source keeps its children")
	}
	if resp.ArchiveSource {
		t.Error("archive_source should echo false")
	}
}

// --- error statuses ----------------------------------------------------

// TestCopyPreflight_ErrorStatusesAreDistinguishable covers DR-15's explicit
// requirement that "destination workspace not accessible", "collection not
// found" and "malformed override" are told apart.
func TestCopyPreflight_ErrorStatusesAreDistinguishable(t *testing.T) {
	f := newCopyPreflightFixture(t)

	// A workspace the caller is a stranger to.
	outsider := mustUser(t, f.srv, "outsider@example.com", "outsideruser", "")
	wsC := mustWorkspace(t, f.srv, "Unreachable WS", outsider.ID)

	body := f.baseBody()
	body["target_workspace"] = wsC.Slug
	rr := f.call(f.owner, reqOpts{}, body)
	if rr.Code != http.StatusForbidden {
		t.Errorf("inaccessible destination workspace: got %d, want 403: %s", rr.Code, rr.Body.String())
	}
	if code := errCode(t, rr); code != "forbidden" {
		t.Errorf("inaccessible destination workspace: error code = %q, want forbidden", code)
	}

	// A collection that does not exist in an accessible workspace.
	body = f.baseBody()
	body["target_collection"] = "no-such-collection"
	rr = f.call(f.owner, reqOpts{}, body)
	if rr.Code != http.StatusNotFound {
		t.Errorf("absent destination collection: got %d, want 404: %s", rr.Code, rr.Body.String())
	}
	if code := errCode(t, rr); code != "collection_not_found" {
		t.Errorf("error code = %q, want collection_not_found", code)
	}

	// Missing required request fields.
	rr = f.call(f.owner, reqOpts{}, map[string]any{"target_collection": f.collB.Slug})
	if rr.Code != http.StatusBadRequest || errCode(t, rr) != "missing_field" {
		t.Errorf("absent target_workspace: got %d/%s", rr.Code, rr.Body.String())
	}
	rr = f.call(f.owner, reqOpts{}, map[string]any{"target_workspace": f.wsB.Slug})
	if rr.Code != http.StatusBadRequest || errCode(t, rr) != "missing_field" {
		t.Errorf("absent target_collection: got %d/%s", rr.Code, rr.Body.String())
	}

	// A body that is not decodable at all is its own code.
	rr = f.callRaw(f.owner, []byte("{not json"))
	if rr.Code != http.StatusBadRequest || errCode(t, rr) != "invalid_body" {
		t.Errorf("undecodable body: got %d/%s, want 400 invalid_body", rr.Code, rr.Body.String())
	}
}

// TestCopyPreflight_ArchivedSourceReports409 — an archived source is
// neither "absent" nor copyable. It reports 409 archived, the same status
// the move and update paths report, and only to a caller who can already
// see the archived row.
func TestCopyPreflight_ArchivedSourceReports409(t *testing.T) {
	f := newCopyPreflightFixture(t)
	if err := f.srv.store.DeleteItem(f.source.ID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}

	rr := f.call(f.owner, reqOpts{}, f.baseBody())
	if rr.Code != http.StatusConflict {
		t.Fatalf("archived source: got %d, want 409: %s", rr.Code, rr.Body.String())
	}
	if code := errCode(t, rr); code != "archived" {
		t.Errorf("archived source: error code = %q, want archived", code)
	}

	// A caller who cannot see the archived item must NOT learn it is
	// archived — that would be an existence oracle for a hidden item.
	otherA := mustSchemaCollection(t, f.srv, f.wsA.ID, "Other A", srcSchemaJSON)
	u := f.restrictedEditor("archived-hidden@example.com", "archivedhidden",
		[]string{otherA.ID}, []string{f.collB.ID})
	rr = f.call(u, reqOpts{wsRoleCtx: "editor"}, f.baseBody())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("archived source, hidden from the caller: got %d, want 404: %s",
			rr.Code, rr.Body.String())
	}
}

// --- authorization: the four checks, in order --------------------------

// restrictedEditor returns an editor in both workspaces whose membership in
// each is collection_access="specific", limited to the named collections.
func (f *copyPreflightFixture) restrictedEditor(email, username string, aColls, bColls []string) *models.User {
	f.t.Helper()
	u := mustUser(f.t, f.srv, email, username, "")
	for _, m := range []struct {
		ws     *models.Workspace
		colls  []string
		wsRole string
	}{{f.wsA, aColls, "editor"}, {f.wsB, bColls, "editor"}} {
		if err := f.srv.store.AddWorkspaceMember(m.ws.ID, u.ID, m.wsRole); err != nil {
			f.t.Fatalf("AddWorkspaceMember: %v", err)
		}
		if err := f.srv.store.SetMemberCollectionAccess(m.ws.ID, u.ID, "specific", m.colls); err != nil {
			f.t.Fatalf("SetMemberCollectionAccess: %v", err)
		}
	}
	return u
}

// TestCopyPreflight_SourceItemNotVisible is DR-10b's headline regression:
// a restricted editor who cannot see the SOURCE collection gets a bare 404
// and learns nothing about either side. This is the exfiltration direction
// — the more damaging of the two.
func TestCopyPreflight_SourceItemNotVisible(t *testing.T) {
	f := newCopyPreflightFixture(t)
	// Restricted in A to a collection the source is NOT in; unrestricted
	// enough in B to write there, so only the source check can refuse.
	otherA := mustSchemaCollection(t, f.srv, f.wsA.ID, "Other A", srcSchemaJSON)
	u := f.restrictedEditor("hidden-src@example.com", "hiddensrc",
		[]string{otherA.ID}, []string{f.collB.ID})

	rr := f.call(u, reqOpts{wsRoleCtx: "editor"}, f.baseBody())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a hidden source item, got %d: %s", rr.Code, rr.Body.String())
	}
	// Nothing about the source item OR the destination schema may appear.
	assertDisclosesNothing(t, rr, f)
}

// TestCopyPreflight_SourceEditDenied — visible but read-only in A. The
// copy is a read out of A, but DR-10b requires EDIT on the source, and the
// refusal is the same non-disclosing 404 as invisibility.
func TestCopyPreflight_SourceEditDenied(t *testing.T) {
	f := newCopyPreflightFixture(t)
	viewer := mustUser(t, f.srv, "viewer-a@example.com", "vieweruser", "")
	if err := f.srv.store.AddWorkspaceMember(f.wsA.ID, viewer.ID, "viewer"); err != nil {
		t.Fatalf("AddWorkspaceMember A: %v", err)
	}
	if err := f.srv.store.AddWorkspaceMember(f.wsB.ID, viewer.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember B: %v", err)
	}

	rr := f.call(viewer, reqOpts{wsRoleCtx: "viewer"}, f.baseBody())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when the caller may not edit the source, got %d: %s", rr.Code, rr.Body.String())
	}
	assertDisclosesNothing(t, rr, f)
}

// TestCopyPreflight_DestinationCollectionNotVisible is DR-10a: a restricted
// editor in B may not preflight into a collection hidden from them, and the
// refusal is byte-identical to the one for a collection that does not exist
// — otherwise the endpoint enumerates hidden collections one slug at a time.
func TestCopyPreflight_DestinationCollectionNotVisible(t *testing.T) {
	f := newCopyPreflightFixture(t)
	u := f.restrictedEditor("hidden-dst@example.com", "hiddendst",
		[]string{f.collA.ID}, []string{f.collB.ID})

	body := f.baseBody()
	body["target_collection"] = f.hiddenB.Slug
	hiddenRR := f.call(u, reqOpts{wsRoleCtx: "editor"}, body)
	if hiddenRR.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a hidden destination collection, got %d: %s",
			hiddenRR.Code, hiddenRR.Body.String())
	}

	body = f.baseBody()
	body["target_collection"] = "definitely-not-a-collection"
	absentRR := f.call(u, reqOpts{wsRoleCtx: "editor"}, body)

	if hiddenRR.Code != absentRR.Code || hiddenRR.Body.String() != absentRR.Body.String() {
		t.Fatalf("hidden and absent destination collections are distinguishable:\n hidden: %d %s\n absent: %d %s",
			hiddenRR.Code, hiddenRR.Body.String(), absentRR.Code, absentRR.Body.String())
	}
	// The hidden collection's schema must not appear anywhere.
	if bytes.Contains(hiddenRR.Body.Bytes(), []byte("ticket")) {
		t.Errorf("refusal disclosed the hidden collection's schema: %s", hiddenRR.Body.String())
	}
}

// TestCopyPreflight_DestinationEditDenied — the collection is visible but
// the caller is a viewer in B, so check 4 refuses.
func TestCopyPreflight_DestinationEditDenied(t *testing.T) {
	f := newCopyPreflightFixture(t)
	u := mustUser(t, f.srv, "viewer-b@example.com", "viewerbuser", "")
	if err := f.srv.store.AddWorkspaceMember(f.wsA.ID, u.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember A: %v", err)
	}
	if err := f.srv.store.AddWorkspaceMember(f.wsB.ID, u.ID, "viewer"); err != nil {
		t.Fatalf("AddWorkspaceMember B: %v", err)
	}

	rr := f.call(u, reqOpts{wsRoleCtx: "editor"}, f.baseBody())
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when the caller may not create into the destination, got %d: %s",
			rr.Code, rr.Body.String())
	}
	if bytes.Contains(rr.Body.Bytes(), []byte("ticket")) {
		t.Errorf("refusal disclosed the destination schema: %s", rr.Body.String())
	}
}

// TestCopyPreflight_SourceCheckRunsBeforeDestination pins the ORDERING, not
// just the set. A caller who fails BOTH checks must get the SOURCE's
// verdict — a destination verdict built for someone who could not read the
// source is a disclosure the source check was supposed to prevent.
func TestCopyPreflight_SourceCheckRunsBeforeDestination(t *testing.T) {
	f := newCopyPreflightFixture(t)
	otherA := mustSchemaCollection(t, f.srv, f.wsA.ID, "Other A", srcSchemaJSON)
	// Hidden source AND a destination workspace they are a stranger to.
	u := mustUser(t, f.srv, "both-fail@example.com", "bothfail", "")
	if err := f.srv.store.AddWorkspaceMember(f.wsA.ID, u.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember A: %v", err)
	}
	if err := f.srv.store.SetMemberCollectionAccess(f.wsA.ID, u.ID, "specific", []string{otherA.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}

	rr := f.call(u, reqOpts{wsRoleCtx: "editor"}, f.baseBody())
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected the SOURCE's 404 to win, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestCopyPreflight_TokenAllowListEnforcedOnDestination — the consent
// allow-list is applied automatically only for the workspace in the URL, so
// the destination must be checked by hand. This is the denial a naive
// implementation misses.
func TestCopyPreflight_TokenAllowListEnforcedOnDestination(t *testing.T) {
	f := newCopyPreflightFixture(t)

	rr := f.call(f.owner, reqOpts{
		bearer: true, setAllowed: true, allowed: []string{f.wsA.Slug},
	}, f.baseBody())
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when the destination is outside the token's consent scope, got %d: %s",
			rr.Code, rr.Body.String())
	}
	// The allow-list reports itself — it is the caller's OWN consent scope,
	// not information about the target — and that distinct code is what a
	// client needs in order to tell the user to re-consent rather than to
	// ask for permissions they may already have.
	if code := errCode(t, rr); code != "permission_denied" {
		t.Errorf("token allow-list denial: error code = %q, want permission_denied", code)
	}

	// Sanity: consenting to both workspaces lets the same call through, so
	// the denial above is the allow-list and not a broken fixture.
	rr = f.call(f.owner, reqOpts{
		bearer: true, setAllowed: true, allowed: []string{f.wsA.Slug, f.wsB.Slug},
	}, f.baseBody())
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 with both workspaces consented, got %d: %s", rr.Code, rr.Body.String())
	}
}

// assertDisclosesNothing checks a refusal body for anything that would
// confirm the source item exists or reveal the destination schema.
func assertDisclosesNothing(t *testing.T, rr *httptest.ResponseRecorder, f *copyPreflightFixture) {
	t.Helper()
	body := rr.Body.Bytes()
	for _, secret := range []string{
		f.source.Title, f.source.Slug, f.source.ID, // source identity
		"ticket", "tier", "impact", // destination + source schema keys
		f.collB.Slug, f.wsB.Slug, f.wsB.Name, // destination identity
	} {
		if secret == "" {
			continue
		}
		if bytes.Contains(body, []byte(secret)) {
			t.Errorf("refusal disclosed %q: %s", secret, rr.Body.String())
		}
	}
}

// --- non-mutation (DR-15) ----------------------------------------------

type worldSnapshot struct {
	items       int
	attachments int
	moves       int
	activities  int
	seqA, seqB  int64
}

func (f *copyPreflightFixture) snapshot() worldSnapshot {
	f.t.Helper()
	return worldSnapshot{
		items:       f.countRows(`SELECT COUNT(*) FROM items`),
		attachments: f.countRows(`SELECT COUNT(*) FROM attachments`),
		moves:       f.countRows(`SELECT COUNT(*) FROM item_workspace_moves`),
		activities:  f.countRows(`SELECT COUNT(*) FROM activities`),
		seqA:        f.maxSeq(f.wsA.ID),
		seqB:        f.maxSeq(f.wsB.ID),
	}
}

func (f *copyPreflightFixture) countRows(query string) int {
	f.t.Helper()
	var n int
	if err := f.srv.store.DB().QueryRow(f.srv.store.D().Rebind(query)).Scan(&n); err != nil {
		f.t.Fatalf("count (%s): %v", query, err)
	}
	return n
}

func (f *copyPreflightFixture) maxSeq(workspaceID string) int64 {
	f.t.Helper()
	var seq int64
	if err := f.srv.store.DB().QueryRow(
		f.srv.store.D().Rebind(`SELECT COALESCE(MAX(seq), 0) FROM items WHERE workspace_id = ?`),
		workspaceID).Scan(&seq); err != nil {
		f.t.Fatalf("max seq: %v", err)
	}
	return seq
}

// TestCopyPreflight_WritesNothing is DR-15's non-mutation clause, asserted
// term by term: no item, no attachment row, no provenance row, no activity,
// no SSE event, and NEITHER workspace's seq advances. It must be safe to
// call repeatedly from a live UI as the user changes the destination, so it
// is called several times with several bodies.
func TestCopyPreflight_WritesNothing(t *testing.T) {
	f := newCopyPreflightFixture(t)

	// Give the source an attachment reference and an assignment so the
	// preflight exercises every read path it has.
	att := &models.Attachment{
		WorkspaceID: f.wsA.ID, UploadedBy: f.owner.ID,
		StorageKey: "fs:cafebabe", ContentHash: "cafebabe",
		MimeType: "image/png", SizeBytes: 512, Filename: "a.png",
	}
	if err := f.srv.store.CreateAttachment(att); err != nil {
		t.Fatalf("CreateAttachment: %v", err)
	}
	content := "![](pad-attachment:" + att.ID + ")"
	if _, err := f.srv.store.UpdateItem(f.source.ID, models.ItemUpdate{Content: &content}); err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}

	// Subscribe to BOTH workspaces before the snapshot so any publish
	// lands in a buffer we can inspect afterwards.
	chA := f.bus.Subscribe(f.wsA.ID)
	defer f.bus.Unsubscribe(chA)
	chB := f.bus.Subscribe(f.wsB.ID)
	defer f.bus.Unsubscribe(chB)

	before := f.snapshot()

	bodies := []map[string]any{
		f.baseBody(),
		func() map[string]any {
			b := f.baseBody()
			b["field_overrides"] = map[string]any{"ticket": "T-1", "code": "123"}
			return b
		}(),
		func() map[string]any {
			b := f.baseBody()
			b["archive_source"] = true
			return b
		}(),
		// Including refused calls: a 4xx must not write either.
		func() map[string]any {
			b := f.baseBody()
			b["field_overrides"] = map[string]any{"priority": "urgent"}
			return b
		}(),
	}
	for i, b := range bodies {
		rr := f.call(f.owner, reqOpts{}, b)
		if rr.Code != http.StatusOK && rr.Code != http.StatusBadRequest {
			t.Fatalf("call %d: unexpected status %d: %s", i, rr.Code, rr.Body.String())
		}
	}

	after := f.snapshot()
	if before != after {
		t.Fatalf("the preflight mutated state:\n before: %+v\n after:  %+v", before, after)
	}

	// No SSE. Draining the subscription channels is the direct assertion;
	// EventsSince(0) covers a publish that happened before we subscribed.
	for label, ch := range map[string]chan events.Event{"A": chA, "B": chB} {
		select {
		case ev := <-ch:
			t.Errorf("workspace %s received an SSE event from a dry run: %+v", label, ev)
		default:
		}
	}
	if got := f.bus.EventsSince(f.wsA.ID, 0); len(got) != 0 {
		t.Errorf("workspace A event buffer = %+v, want empty", got)
	}
	if got := f.bus.EventsSince(f.wsB.ID, 0); len(got) != 0 {
		t.Errorf("workspace B event buffer = %+v, want empty", got)
	}
}

// TestCopyPreflight_NoWebhookDispatch — the fanout DR-14 specifies for the
// mutating copy must not fire on the preflight. Asserted with a positive
// control: a REAL item creation in the destination afterwards does deliver,
// so "nothing arrived" is a fact about the preflight and not about a
// dispatcher that was never wired up.
func TestCopyPreflight_NoWebhookDispatch(t *testing.T) {
	f := newCopyPreflightFixture(t)

	received := make(chan string, 8)
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Event string `json:"event"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		received <- payload.Event
		w.WriteHeader(http.StatusOK)
	}))
	defer recv.Close()

	d := webhooks.NewDispatcher(f.srv.store)
	d.SkipSSRF = true // the receiver is a loopback httptest server
	f.srv.SetWebhookDispatcher(d)

	if _, err := f.srv.store.CreateWebhook(f.wsB.ID, models.WebhookCreate{
		URL: recv.URL, Events: `["*"]`,
	}); err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}

	for i := 0; i < 3; i++ {
		if rr := f.call(f.owner, reqOpts{}, f.baseBody()); rr.Code != http.StatusOK {
			t.Fatalf("preflight: %d %s", rr.Code, rr.Body.String())
		}
	}

	// The NEGATIVE assertion comes first, over an explicit window, and no
	// legitimate traffic exists yet — so anything that arrives here came
	// from a preflight. Deliberately not an ordering argument: Dispatch
	// starts each delivery in its own goroutine, so "the control arrived
	// first" would prove nothing about a straggler behind it.
	select {
	case ev := <-received:
		t.Fatalf("a preflight dispatched webhook %q — the dry run must emit nothing", ev)
	case <-time.After(webhookQuietWindow):
	}

	// Positive control. Without it the silence above would also be
	// consistent with a receiver that never works.
	f.srv.webhooks.Dispatch(f.wsB.ID, "item.created", map[string]string{"probe": "1"})
	select {
	case ev := <-received:
		if ev != "item.created" {
			t.Fatalf("positive control delivered %q, want item.created", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("positive control never delivered; the webhook harness is broken, so the negative assertion above proves nothing")
	}
}

// webhookQuietWindow is how long TestCopyPreflight_NoWebhookDispatch waits
// for a delivery that must never come. Deliveries are goroutine-dispatched
// to a loopback httptest server in the same process, so a real one lands in
// microseconds; the margin is for a loaded CI box. It only costs wall-clock
// time in the passing case, so it is set generously rather than tightly.
const webhookQuietWindow = 750 * time.Millisecond

// TestCopyPreflight_RepeatedCallsAreByteIdentical — DR-15 requires the
// preflight to be safe to call repeatedly, which is only true if the answer
// is stable. Go map iteration order makes this a real risk: the bucket
// lists and MigrateFields' Dropped slice all originate from maps.
func TestCopyPreflight_RepeatedCallsAreByteIdentical(t *testing.T) {
	f := newCopyPreflightFixture(t)
	body := f.baseBody()
	body["field_overrides"] = map[string]any{"ticket": "T-1"}

	first := f.call(f.owner, reqOpts{}, body).Body.String()
	for i := 0; i < 20; i++ {
		if got := f.call(f.owner, reqOpts{}, body).Body.String(); got != first {
			t.Fatalf("call %d differed:\n first: %s\n got:   %s", i+2, first, got)
		}
	}
}

// TestCopyPreflight_EmptyBucketsAreArraysNotNull pins the wire shape for
// the TypeScript consumer: `[]`, never `null`, and the same for the two
// link maps.
func TestCopyPreflight_EmptyBucketsAreArraysNotNull(t *testing.T) {
	f := newCopyPreflightFixture(t)
	// A destination whose schema is identical to the source's: nothing
	// drops, nothing is missing.
	twinB := mustSchemaCollection(t, f.srv, f.wsB.ID, "Twin B", srcSchemaJSON)

	body := f.baseBody()
	body["target_collection"] = twinB.Slug
	rr := f.call(f.owner, reqOpts{}, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("preflight: %d %s", rr.Code, rr.Body.String())
	}

	var raw struct {
		Fields struct {
			Carried    json.RawMessage `json:"carried"`
			Dropped    json.RawMessage `json:"dropped"`
			NeedsValue json.RawMessage `json:"needs_value"`
		} `json:"fields"`
		Warnings struct {
			Outgoing json.RawMessage `json:"outgoing_links"`
			Incoming json.RawMessage `json:"incoming_links"`
		} `json:"warnings"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("parse: %v", err)
	}
	for name, blob := range map[string]json.RawMessage{
		"carried": raw.Fields.Carried, "dropped": raw.Fields.Dropped,
		"needs_value": raw.Fields.NeedsValue,
	} {
		if string(blob) == "null" || len(blob) == 0 {
			t.Errorf("fields.%s serialized as %q, want an array", name, string(blob))
		}
	}
	if string(raw.Warnings.Outgoing) != "{}" || string(raw.Warnings.Incoming) != "{}" {
		t.Errorf("link maps = %s / %s, want {} / {}",
			raw.Warnings.Outgoing, raw.Warnings.Incoming)
	}
	if string(raw.Fields.Dropped) != "[]" {
		t.Errorf("dropped = %s, want [] for an identical schema", raw.Fields.Dropped)
	}
}

// TestCopyPreflight_SourceIsItemScopedNotCollectionScoped pins WHICH scope
// the source check uses. An item scope and a collection scope both look
// plausible there and both refuse the restricted-editor cases above, so the
// tests that assert denials cannot tell them apart.
//
// This is the case that can: a GUEST in the source workspace whose sole
// claim is an edit grant on the source ITEM. That is exactly what an item
// grant means, so they may copy it — but a collection-scoped check applies
// full-collection-access semantics and would refuse them. Swapping the
// scope silently narrows a legitimate caller out.
func TestCopyPreflight_SourceIsItemScopedNotCollectionScoped(t *testing.T) {
	f := newCopyPreflightFixture(t)

	guest := mustUser(t, f.srv, "grantguest@example.com", "grantguest", "")
	// No membership in A at all — only the item grant.
	if _, err := f.srv.store.CreateItemGrant(f.wsA.ID, f.source.ID, guest.ID, "edit", f.owner.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}
	// A genuine editor in the destination, so only the source scope is
	// under test.
	if err := f.srv.store.AddWorkspaceMember(f.wsB.ID, guest.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember B: %v", err)
	}

	rr := f.call(guest, reqOpts{wsRoleCtx: "guest"}, f.baseBody())
	if rr.Code != http.StatusOK {
		t.Fatalf("a guest holding an edit grant on the source must be able to preflight it; got %d: %s",
			rr.Code, rr.Body.String())
	}

	// The negative half of the same grant: a SIBLING item in the same
	// collection, which the guest has no grant on, stays unreachable.
	sibling := mustItem(t, f.srv, f.wsA.ID, f.collA.ID, "Ungranted Sibling")
	prev := f.source
	f.source = sibling
	rr = f.call(guest, reqOpts{wsRoleCtx: "guest"}, f.baseBody())
	f.source = prev
	if rr.Code != http.StatusNotFound {
		t.Fatalf("an ungranted sibling item must stay hidden; got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestCopyPreflight_RelationshipCountersAreACLFiltered — the warning
// counters must not become a wider window onto the source workspace than
// /children and /links already are. Store.GetChildItems and
// Store.GetItemLinks apply no ACL of their own, so an unfiltered preflight
// tells a caller holding nothing but an edit grant on the source item the
// exact number and type of relationships attached to it, every one of
// which may live in a collection hidden from them (Codex round 3).
func TestCopyPreflight_RelationshipCountersAreACLFiltered(t *testing.T) {
	f := newCopyPreflightFixture(t)
	s := f.srv.store

	// Two collections in A: one the restricted caller can see, one not.
	openA := mustSchemaCollection(t, f.srv, f.wsA.ID, "Open A", srcSchemaJSON)
	secretA := mustSchemaCollection(t, f.srv, f.wsA.ID, "Secret A", srcSchemaJSON)

	// One visible child + one hidden child.
	for _, coll := range []*models.Collection{openA, secretA} {
		child := mustItem(t, f.srv, f.wsA.ID, coll.ID, "Child in "+coll.Slug)
		if _, err := s.SetParentLink(f.wsA.ID, child.ID, f.source.ID, f.owner.ID); err != nil {
			t.Fatalf("SetParentLink: %v", err)
		}
	}
	// One visible dependency edge + one hidden one.
	for _, coll := range []*models.Collection{openA, secretA} {
		blockee := mustItem(t, f.srv, f.wsA.ID, coll.ID, "Blockee in "+coll.Slug)
		if _, err := s.CreateItemLink(f.wsA.ID,
			models.ItemLinkCreate{TargetID: blockee.ID, LinkType: "blocks", CreatedBy: f.owner.ID},
			f.source.ID); err != nil {
			t.Fatalf("CreateItemLink: %v", err)
		}
	}

	// Control: an unrestricted owner sees both of each.
	if w := f.ok(f.baseBody()).Warnings; w.ChildCount != 2 || w.OutgoingLinks["blocks"] != 2 {
		t.Fatalf("owner: child_count=%d outgoing=%v, want 2 and blocks:2", w.ChildCount, w.OutgoingLinks)
	}

	// The restricted caller can see the source's collection and the open
	// one, but not the secret one.
	u := f.restrictedEditor("rel-restricted@example.com", "relrestricted",
		[]string{f.collA.ID, openA.ID}, []string{f.collB.ID})

	rr := f.call(u, reqOpts{wsRoleCtx: "editor"}, f.baseBody())
	if rr.Code != http.StatusOK {
		t.Fatalf("restricted caller preflight: %d %s", rr.Code, rr.Body.String())
	}
	var resp ItemCopyPreflight
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Warnings.ChildCount != 1 {
		t.Errorf("child_count = %d, want 1 — the child in the hidden collection must not be counted",
			resp.Warnings.ChildCount)
	}
	if resp.Warnings.OutgoingLinks["blocks"] != 1 {
		t.Errorf("outgoing_links = %v, want blocks:1 — the edge into the hidden collection must not be counted",
			resp.Warnings.OutgoingLinks)
	}

	// And the same for the source's own parent, which is the dropped_parent
	// warning rather than a count.
	hiddenParent := mustItem(t, f.srv, f.wsA.ID, secretA.ID, "Hidden Parent")
	if _, err := s.SetParentLink(f.wsA.ID, f.source.ID, hiddenParent.ID, f.owner.ID); err != nil {
		t.Fatalf("SetParentLink(parent): %v", err)
	}
	if !f.ok(f.baseBody()).Warnings.DroppedParent {
		t.Fatal("owner should see dropped_parent")
	}
	rr = f.call(u, reqOpts{wsRoleCtx: "editor"}, f.baseBody())
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Warnings.DroppedParent {
		t.Error("dropped_parent must be false for a caller who cannot see the parent's collection")
	}
}

// TestCopyPreflight_OrphanSourceKeyHasItsOwnReason — a key the item carries
// that its OWN schema no longer declares drops even when the destination
// declares it, because MigrateFields has no source type to convert from.
// That is a different fact from a type mismatch, and labelling it
// "incompatible_type" tells a dialog to explain the wrong thing.
func TestCopyPreflight_OrphanSourceKeyHasItsOwnReason(t *testing.T) {
	f := newCopyPreflightFixture(t)

	// `ticket` is declared by the DESTINATION but not by the source
	// schema; write it onto the item anyway, the way a schema edit leaves
	// an orphan key behind.
	fields := `{"status":"open","priority":"low","code":"123","ticket":"T-7"}`
	if _, err := f.srv.store.UpdateItem(f.source.ID, models.ItemUpdate{Fields: &fields}); err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}

	resp := f.ok(f.baseBody())

	d, ok := droppedByKey(resp, "ticket")
	if !ok {
		t.Fatalf("orphan key `ticket` is not in the dropped bucket: %+v", resp.Fields.Dropped)
	}
	if d.Reason != "undeclared_source_field" {
		t.Errorf("ticket dropped reason = %q, want undeclared_source_field", d.Reason)
	}
	// And it is genuinely dropped, not carried — the orphan value must not
	// silently satisfy the destination's required field.
	n, ok := needsValueByKey(resp, "ticket")
	if !ok || n.Reason != "missing_required" {
		t.Errorf("ticket should still be needs_value/missing_required, got %+v (present=%v)", n, ok)
	}

	// A key declared by BOTH schemas whose value cannot convert keeps the
	// type-mismatch reason, so the two are actually distinguished.
	if d, ok := droppedByKey(f.ok(f.baseBody()), "count"); ok && d.Reason != "incompatible_type" {
		t.Errorf("count dropped reason = %q, want incompatible_type", d.Reason)
	}
}

// TestCopyPreflight_NullOverrideOnDefaultedFieldRedefaults — the other half
// of the null semantics. Deleting the key hands it back to validation,
// which re-applies the destination schema's default, so a nulled field
// that HAS a default is carried rather than reported as missing. Only a
// required field with no default becomes needs_value (asserted separately
// in TestCopyPreflight_NullOverrideUnsetsAndReappears).
func TestCopyPreflight_NullOverrideOnDefaultedFieldRedefaults(t *testing.T) {
	f := newCopyPreflightFixture(t)
	// A destination whose `status` is required AND defaulted.
	defaulted := mustSchemaCollection(t, f.srv, f.wsB.ID, "Defaulted B", `{"fields":[
		{"key":"status","label":"Status","type":"select","options":["open","done"],"required":true,"default":"done"}
	]}`)

	body := f.baseBody()
	body["target_collection"] = defaulted.Slug
	body["field_overrides"] = map[string]any{"status": nil}
	resp := f.ok(body)

	if _, ok := needsValueByKey(resp, "status"); ok {
		t.Error("a nulled REQUIRED field that has a default must not be needs_value — validation re-applies the default")
	}
	if got := carriedByKey(t, resp, "status"); got.Value != "done" || got.From != "default" {
		t.Errorf("status: got %+v, want value=done from=default", got)
	}
	if !resp.Valid {
		t.Error("valid should be true — nothing is left for the caller to resolve")
	}
}

// TestCopyPreflight_OversizedBodyIsRejected — the endpoint is designed to
// be called on every keystroke of a live dialog, so the body must go
// through the project's MaxBytesReader-wrapped decoder rather than
// json.NewDecoder. Without the cap a single request allocates the whole
// blob (Codex round 7).
func TestCopyPreflight_OversizedBodyIsRejected(t *testing.T) {
	f := newCopyPreflightFixture(t)

	// Comfortably past defaultJSONBodyLimit (2 MiB).
	var b bytes.Buffer
	b.WriteString(`{"target_workspace":"` + f.wsB.Slug + `","target_collection":"` + f.collB.Slug + `","field_overrides":{`)
	for i := 0; i < 60000; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"k%d":"%s"`, i, strings.Repeat("x", 64))
	}
	b.WriteString("}}")
	if b.Len() <= defaultJSONBodyLimit {
		t.Fatalf("test body is only %d bytes, under the %d limit", b.Len(), defaultJSONBodyLimit)
	}

	rr := f.callRaw(f.owner, b.Bytes())
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("oversized body: got %d, want 400: %s", rr.Code, rr.Body.String())
	}
	if code := errCode(t, rr); code != "invalid_body" {
		t.Errorf("oversized body: error code = %q, want invalid_body", code)
	}
}

// TestCopyPreflight_MalformedOverrideMessageIsBounded — the 400 must not
// echo an unbounded slice of caller-supplied keys back at them.
func TestCopyPreflight_MalformedOverrideMessageIsBounded(t *testing.T) {
	f := newCopyPreflightFixture(t)

	overrides := map[string]any{}
	for i := 0; i < 200; i++ {
		overrides[fmt.Sprintf("ghost%03d%s", i, strings.Repeat("y", 200))] = "v"
	}
	body := f.baseBody()
	body["field_overrides"] = overrides

	rr := f.call(f.owner, reqOpts{}, body)
	if rr.Code != http.StatusBadRequest || errCode(t, rr) != "malformed_override" {
		t.Fatalf("got %d %s, want 400 malformed_override", rr.Code, rr.Body.String())
	}
	if n := rr.Body.Len(); n > 1024 {
		t.Errorf("malformed_override body is %d bytes — the key list is not bounded: %s", n, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "more") {
		t.Errorf("expected the message to say how many keys were elided: %s", rr.Body.String())
	}
}

// TestCopyPreflight_InvalidOverrideMessageIsBounded — validateFieldType
// quotes the offending value verbatim, so an override carrying a ~2 MiB
// string would be reflected back in full without a cap (Codex round 8).
func TestCopyPreflight_InvalidOverrideMessageIsBounded(t *testing.T) {
	f := newCopyPreflightFixture(t)

	// `code` has pattern ^[0-9]+$, so any long non-numeric string fails
	// and lands in the message.
	huge := strings.Repeat("z", 400000)
	body := f.baseBody()
	body["field_overrides"] = map[string]any{"ticket": "T-1", "code": huge}

	rr := f.call(f.owner, reqOpts{}, body)
	if rr.Code != http.StatusBadRequest || errCode(t, rr) != "invalid_override" {
		t.Fatalf("got %d %s, want 400 invalid_override", rr.Code, rr.Body.String())
	}
	if n := rr.Body.Len(); n > 2048 {
		t.Errorf("invalid_override body is %d bytes — the value is not truncated", n)
	}

	// Truncation must not emit invalid UTF-8 through a multi-byte rune.
	body["field_overrides"] = map[string]any{"ticket": "T-1", "code": strings.Repeat("é", 400)}
	rr = f.call(f.owner, reqOpts{}, body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("got %d %s, want 400", rr.Code, rr.Body.String())
	}
	if !utf8.Valid(rr.Body.Bytes()) {
		t.Error("truncated message is not valid UTF-8")
	}
}

// TestPreflightHierarchyTypesCoverStoreChildren is the drift alarm for the
// one invariant the preflight's link partition rests on: every link type
// store.GetChildItems treats as a child must be classified as hierarchy
// here. Add a third child link type to the store without this and the
// preflight starts reporting parent/child edges as dependency blockers.
func TestPreflightHierarchyTypesCoverStoreChildren(t *testing.T) {
	for _, lt := range store.ChildLinkTypes() {
		if !hierarchyLinkTypes[lt] {
			t.Errorf("store.ChildLinkTypes contains %q but the preflight does not classify it as hierarchy — "+
				"it would be counted as a dependency edge", lt)
		}
	}
	// And the legacy alias is still covered, so a "plan" row alongside a
	// "parent" row cannot double-count.
	for _, lt := range legacyHierarchyLinkTypes {
		if !hierarchyLinkTypes[lt] {
			t.Errorf("legacy hierarchy type %q is not classified as hierarchy", lt)
		}
	}
}

// TestCopyPreflight_RouteIsWiredAndEchoesArchiveSource goes through the
// REAL router rather than calling the handler directly, so the route
// registration and the middleware chain are covered too — every other
// test in this file hand-builds the chi context.
func TestCopyPreflight_RouteIsWiredAndEchoesArchiveSource(t *testing.T) {
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
		"/api/v1/workspaces/"+wsA+"/items/"+item.Slug+"/copy/preflight",
		map[string]interface{}{
			"target_workspace":  wsA,
			"target_collection": collB.Slug,
			"archive_source":    true,
		})
	if rr.Code != http.StatusOK {
		t.Fatalf("routed preflight: got %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var resp ItemCopyPreflight
	parseJSON(t, rr, &resp)
	if !resp.ArchiveSource {
		t.Error("archive_source must echo back as true on the move path")
	}
	if !resp.Valid {
		t.Errorf("expected a same-schema copy to be valid: %+v", resp.Fields.NeedsValue)
	}
	if resp.Destination.CollectionSlug != collB.Slug {
		t.Errorf("destination = %+v", resp.Destination)
	}
}

// TestCopyPreflight_LegacyParentIDColumnIsReported — an item created
// through the API with a raw `parent_id` has NO parent item_links row, so
// the link scan alone misses it. CopyItemAcrossWorkspaces scrubs the
// column anyway, which would drop a parent the preview never announced
// (Codex round 11).
func TestCopyPreflight_LegacyParentIDColumnIsReported(t *testing.T) {
	f := newCopyPreflightFixture(t)

	parent := mustItem(t, f.srv, f.wsA.ID, f.collA.ID, "Column Parent")
	withColumn, err := f.srv.store.CreateItem(f.wsA.ID, f.collA.ID, models.ItemCreate{
		Title: "Has parent_id", Fields: `{"status":"open"}`,
		ParentID: &parent.ID, CreatedBy: f.owner.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	// Sanity: no item_links row exists, so only the column can carry it.
	if links, lErr := f.srv.store.GetItemLinks(withColumn.ID); lErr != nil || len(links) != 0 {
		t.Fatalf("fixture: expected no item_links, got %d (err=%v)", len(links), lErr)
	}

	prev := f.source
	f.source = withColumn
	defer func() { f.source = prev }()

	if !f.ok(f.baseBody()).Warnings.DroppedParent {
		t.Error("dropped_parent must be true for an item whose parent lives in the legacy parent_id column")
	}

	// The mirror-image gap: from the PARENT's side, that same row is a
	// child GetChildItems cannot see, so child_count (and, on the move
	// path, children_orphaned) would miss it.
	f.source = parent
	body := f.baseBody()
	body["archive_source"] = true
	w := f.ok(body).Warnings
	if w.ChildCount != 1 {
		t.Errorf("child_count = %d, want 1 — a child linked only by the legacy parent_id column", w.ChildCount)
	}
	if !w.ChildrenOrphaned {
		t.Error("children_orphaned must be true on the move path for a column-linked child")
	}

	// A child carrying BOTH a link row and the column counts once.
	if _, err := f.srv.store.SetParentLink(f.wsA.ID, withColumn.ID, parent.ID, f.owner.ID); err != nil {
		t.Fatalf("SetParentLink: %v", err)
	}
	if got := f.ok(body).Warnings.ChildCount; got != 1 {
		t.Errorf("child_count = %d, want 1 — a child with both a link and the column must not double-count", got)
	}

	// A column value pointing at another workspace is NOT honoured — the
	// column carries no foreign key, so it is attacker-influenced state.
	foreign := mustItem(t, f.srv, f.wsB.ID, f.collB.ID, "Foreign Parent")
	crossWS, err := f.srv.store.CreateItem(f.wsA.ID, f.collA.ID, models.ItemCreate{
		Title: "Foreign parent_id", Fields: `{"status":"open"}`,
		ParentID: &foreign.ID, CreatedBy: f.owner.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem(cross-ws): %v", err)
	}
	f.source = crossWS
	if f.ok(f.baseBody()).Warnings.DroppedParent {
		t.Error("a parent_id pointing outside the source workspace must not be reported as a dropped parent")
	}
}

// --- TASK-2369: the ACL filter must be visible, and must not leak -------

// mustLegacyPlanLink writes an item_links row carrying the pre-rename
// "plan" link_type directly.
//
// It has to go in by hand: models.NormalizeItemLinkType has no "plan"
// alias, so CreateItemLink rejects it — and that is exactly why the row
// matters. It exists only in workspaces that predate the rename, where no
// current code path can produce it and none can repair it either.
//
// The values are interpolated rather than bound because the two drivers
// disagree about placeholder syntax and Store.q is unexported. Every one
// of them is server-generated, so there is nothing here a caller could
// influence.
func mustLegacyPlanLink(t *testing.T, srv *Server, wsID, childID, parentID string) {
	t.Helper()
	_, err := srv.store.DB().Exec(fmt.Sprintf(
		`INSERT INTO item_links (id, workspace_id, source_id, target_id, link_type, created_by, created_at)
		 VALUES ('legacy-plan-%s-%s', '%s', '%s', '%s', 'plan', 'user', '2024-01-01T00:00:00Z')`,
		childID, parentID, wsID, childID, parentID))
	if err != nil {
		t.Fatalf("insert legacy 'plan' link: %v", err)
	}
}

// TestCopyPreflight_LoneLegacyPlanChildIsCounted is P2 of TASK-2369.
//
// GetChildItems joins on store.ChildLinkTypes, which does not include the
// legacy "plan" alias, so a child reachable ONLY by a lone "plan" edge was
// invisible to child_count. On the move path that rendered as
// `children_orphaned: false` for a child archiving the source does in fact
// strand — the quiet data loss DR-17 forbids.
func TestCopyPreflight_LoneLegacyPlanChildIsCounted(t *testing.T) {
	f := newCopyPreflightFixture(t)

	child := mustItem(t, f.srv, f.wsA.ID, f.collA.ID, "Legacy plan child")
	mustLegacyPlanLink(t, f.srv, f.wsA.ID, child.ID, f.source.ID)

	// Sanity: the store genuinely cannot see it, so the handler is the
	// only thing that can report it. If this ever stops holding, the
	// double-count guard below is what keeps the fix honest.
	kids, err := f.srv.store.GetChildItems(f.source.ID)
	if err != nil {
		t.Fatalf("GetChildItems: %v", err)
	}
	if len(kids) != 0 {
		t.Fatalf("fixture: GetChildItems should not walk 'plan'; got %d", len(kids))
	}

	body := f.baseBody()
	body["archive_source"] = true
	w := f.ok(body).Warnings
	if w.ChildCount != 1 {
		t.Errorf("child_count = %d, want 1 — a child reachable only by a lone legacy 'plan' edge", w.ChildCount)
	}
	if !w.ChildrenOrphaned {
		t.Error("children_orphaned must be true on the move path for a legacy 'plan' child")
	}
	// It is a CHILD, not a dependency: reporting it under a link map would
	// announce a blocker that does not exist.
	if len(w.OutgoingLinks) != 0 || len(w.IncomingLinks) != 0 {
		t.Errorf("legacy 'plan' edge leaked into the dependency maps: out=%v in=%v",
			w.OutgoingLinks, w.IncomingLinks)
	}
	// Nothing is hidden from the owner, so the common case stays unmarked.
	if w.RelationshipsPartial {
		t.Error("relationships_partial must be false for an unrestricted caller with nothing hidden")
	}

	// Deduplication: the same edge ALSO carrying a real `parent` row — the
	// duplication GetChildItemsTx's DISTINCT exists for — counts once.
	if _, err := f.srv.store.SetParentLink(f.wsA.ID, child.ID, f.source.ID, f.owner.ID); err != nil {
		t.Fatalf("SetParentLink: %v", err)
	}
	if got := f.ok(body).Warnings.ChildCount; got != 1 {
		t.Errorf("child_count = %d, want 1 — a 'plan' edge duplicating a 'parent' row must not double-count", got)
	}
}

// TestCopyPreflight_LoneLegacyPlanParentIsReported is the other direction
// of the same edge: OUTGOING, it is the item's own parent, and the copy is
// unparented whichever alias expressed it.
func TestCopyPreflight_LoneLegacyPlanParentIsReported(t *testing.T) {
	f := newCopyPreflightFixture(t)

	parent := mustItem(t, f.srv, f.wsA.ID, f.collA.ID, "Legacy plan parent")
	mustLegacyPlanLink(t, f.srv, f.wsA.ID, f.source.ID, parent.ID)

	w := f.ok(f.baseBody()).Warnings
	if !w.DroppedParent {
		t.Error("dropped_parent must be true for a parent expressed by a lone legacy 'plan' edge")
	}
	if w.ChildCount != 0 {
		t.Errorf("child_count = %d, want 0 — an OUTGOING 'plan' edge is a parent, not a child", w.ChildCount)
	}
}

// TestCopyPreflight_LegacyPlanChildIsStillACLFiltered — the new legacy
// accounting must not become a back door around the visibility filter the
// rest of the block applies. A hidden legacy child stays uncounted; it is
// the marker, not the number, that admits it exists.
func TestCopyPreflight_LegacyPlanChildIsStillACLFiltered(t *testing.T) {
	f := newCopyPreflightFixture(t)
	secretA := mustSchemaCollection(t, f.srv, f.wsA.ID, "Secret A", srcSchemaJSON)
	u := f.restrictedEditor("legacy-acl@example.com", "legacyacl",
		[]string{f.collA.ID}, []string{f.collB.ID})

	child := mustItem(t, f.srv, f.wsA.ID, secretA.ID, "Hidden legacy child")
	mustLegacyPlanLink(t, f.srv, f.wsA.ID, child.ID, f.source.ID)

	if got := f.ok(f.baseBody()).Warnings.ChildCount; got != 1 {
		t.Fatalf("owner: child_count = %d, want 1", got)
	}
	assertCounts(t, f, u, 0)
	assertPartial(t, f, u, true, "a legacy 'plan' child in a collection the caller cannot see")
}

// assertPartial runs the preflight as u and checks relationships_partial.
func assertPartial(t *testing.T, f *copyPreflightFixture, u *models.User, want bool, why string) {
	t.Helper()
	if got := restrictedWarnings(t, f, u).RelationshipsPartial; got != want {
		t.Errorf("relationships_partial = %v, want %v — %s", got, want, why)
	}
}

// assertCounts pins the privacy half of the contract: the numbers stay at
// the VISIBLE total even though the marker admits there is more.
func assertCounts(t *testing.T, f *copyPreflightFixture, u *models.User, wantChildren int) {
	t.Helper()
	if got := restrictedWarnings(t, f, u).ChildCount; got != wantChildren {
		t.Errorf("child_count = %d, want %d — the marker must not start disclosing hidden items",
			got, wantChildren)
	}
}

func restrictedWarnings(t *testing.T, f *copyPreflightFixture, u *models.User) ItemCopyPreflightWarnings {
	t.Helper()
	rr := f.call(u, reqOpts{wsRoleCtx: "editor"}, f.baseBody())
	if rr.Code != http.StatusOK {
		t.Fatalf("preflight: %d %s", rr.Code, rr.Body.String())
	}
	var resp ItemCopyPreflight
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse preflight: %v", err)
	}
	return resp.Warnings
}

// TestCopyPreflight_NoPartialMarkerOnTheCommonCase — the half of P1 that is
// easy to lose. Closing the finding by marking every preview would make the
// product worse: a caveat that is always there is a caveat nobody reads, and
// the one time it means something it would be invisible.
func TestCopyPreflight_NoPartialMarkerOnTheCommonCase(t *testing.T) {
	f := newCopyPreflightFixture(t)
	s := f.srv.store
	openA := mustSchemaCollection(t, f.srv, f.wsA.ID, "Open A", srcSchemaJSON)
	restricted := f.restrictedEditor("clean-case@example.com", "cleancase",
		[]string{f.collA.ID, openA.ID}, []string{f.collB.ID})

	// (a) An unrestricted owner, with no relationships at all.
	if f.ok(f.baseBody()).Warnings.RelationshipsPartial {
		t.Fatal("owner, no relationships: relationships_partial must be false")
	}
	// (b) A restricted caller, with no relationships at all.
	assertPartial(t, f, restricted, false, "a restricted caller with nothing related to hide")

	// (c) A restricted caller who can see every relationship there is —
	//     the case that separates "is the caller restricted" from "was
	//     anything actually withheld". A marker keyed off the former
	//     would fire here.
	child := mustItem(t, f.srv, f.wsA.ID, openA.ID, "Visible child")
	if _, err := s.SetParentLink(f.wsA.ID, child.ID, f.source.ID, f.owner.ID); err != nil {
		t.Fatalf("SetParentLink(child): %v", err)
	}
	parent := mustItem(t, f.srv, f.wsA.ID, openA.ID, "Visible parent")
	if _, err := s.SetParentLink(f.wsA.ID, f.source.ID, parent.ID, f.owner.ID); err != nil {
		t.Fatalf("SetParentLink(parent): %v", err)
	}
	blockee := mustItem(t, f.srv, f.wsA.ID, openA.ID, "Visible blockee")
	if _, err := s.CreateItemLink(f.wsA.ID,
		models.ItemLinkCreate{TargetID: blockee.ID, LinkType: "blocks", CreatedBy: f.owner.ID},
		f.source.ID); err != nil {
		t.Fatalf("CreateItemLink: %v", err)
	}

	w := restrictedWarnings(t, f, restricted)
	if w.RelationshipsPartial {
		t.Error("a restricted caller who can see every relationship must get NO partial marker")
	}
	if w.ChildCount != 1 || !w.DroppedParent || w.OutgoingLinks["blocks"] != 1 {
		t.Errorf("fixture is not exercising real relationships: %+v", w)
	}

	// (d) An unrestricted owner looking at the same graph. Same answer,
	//     for a different reason, and it must stay unmarked too.
	if f.ok(f.baseBody()).Warnings.RelationshipsPartial {
		t.Error("owner with a fully visible relationship graph: relationships_partial must be false")
	}
}

// TestCopyPreflight_PartialMarkerSurfacesHiddenRelationships is P1 of
// TASK-2369. The ACL filtering is correct and stays; what changes is that
// "none" and "none that you can see" stop rendering identically.
//
// Each of the four channels the filter runs through gets its OWN fixture,
// so a marker set by an earlier step can never stand in for one a later
// step failed to set.
func TestCopyPreflight_PartialMarkerSurfacesHiddenRelationships(t *testing.T) {
	// setup returns a fixture whose restricted editor can see only the
	// source's collection, plus the hidden collection to seed into.
	setup := func(t *testing.T, tag string) (*copyPreflightFixture, *models.Collection, *models.User) {
		t.Helper()
		f := newCopyPreflightFixture(t)
		secret := mustSchemaCollection(t, f.srv, f.wsA.ID, "Secret", srcSchemaJSON)
		u := f.restrictedEditor(tag+"@example.com", tag,
			[]string{f.collA.ID}, []string{f.collB.ID})
		return f, secret, u
	}

	t.Run("hidden child", func(t *testing.T) {
		f, secret, u := setup(t, "hidchild")
		kid := mustItem(t, f.srv, f.wsA.ID, secret.ID, "Hidden child")
		if _, err := f.srv.store.SetParentLink(f.wsA.ID, kid.ID, f.source.ID, f.owner.ID); err != nil {
			t.Fatalf("SetParentLink: %v", err)
		}
		assertPartial(t, f, u, true, "a child in a collection the caller cannot see")
		assertCounts(t, f, u, 0)
	})

	t.Run("hidden child via the parent_id column", func(t *testing.T) {
		f, secret, u := setup(t, "hidcolchild")
		if _, err := f.srv.store.CreateItem(f.wsA.ID, secret.ID, models.ItemCreate{
			Title: "Hidden column child", Fields: `{"status":"open"}`,
			ParentID: &f.source.ID, CreatedBy: f.owner.ID,
		}); err != nil {
			t.Fatalf("CreateItem: %v", err)
		}
		assertPartial(t, f, u, true, "a column-linked child in a collection the caller cannot see")
		assertCounts(t, f, u, 0)
	})

	t.Run("hidden dependency edge", func(t *testing.T) {
		f, secret, u := setup(t, "hidlink")
		blockee := mustItem(t, f.srv, f.wsA.ID, secret.ID, "Hidden blockee")
		if _, err := f.srv.store.CreateItemLink(f.wsA.ID,
			models.ItemLinkCreate{TargetID: blockee.ID, LinkType: "blocks", CreatedBy: f.owner.ID},
			f.source.ID); err != nil {
			t.Fatalf("CreateItemLink: %v", err)
		}
		assertPartial(t, f, u, true, "a dependency edge into a collection the caller cannot see")
		if got := restrictedWarnings(t, f, u).OutgoingLinks; len(got) != 0 {
			t.Errorf("outgoing_links = %v, want empty — the hidden edge must stay uncounted", got)
		}
	})

	t.Run("hidden parent link", func(t *testing.T) {
		f, secret, u := setup(t, "hidparent")
		parent := mustItem(t, f.srv, f.wsA.ID, secret.ID, "Hidden parent")
		if _, err := f.srv.store.SetParentLink(f.wsA.ID, f.source.ID, parent.ID, f.owner.ID); err != nil {
			t.Fatalf("SetParentLink: %v", err)
		}
		assertPartial(t, f, u, true, "a parent in a collection the caller cannot see")
		if restrictedWarnings(t, f, u).DroppedParent {
			t.Error("dropped_parent must stay false — the marker replaces the disclosure, it does not add one")
		}
	})

	// The obvious short-circuit — "dropped_parent is already true, skip the
	// column" — leaves a withheld relationship unmarked, because the two
	// mechanisms can name DIFFERENT items. SetParentLink does not touch
	// items.parent_id, so both can be live at once (Codex round 1).
	t.Run("hidden parent_id column behind a visible parent link", func(t *testing.T) {
		f, secret, u := setup(t, "hidcolbehind")
		hiddenParent := mustItem(t, f.srv, f.wsA.ID, secret.ID, "Hidden column parent")
		withColumn, err := f.srv.store.CreateItem(f.wsA.ID, f.collA.ID, models.ItemCreate{
			Title: "Two parents", Fields: `{"status":"open"}`,
			ParentID: &hiddenParent.ID, CreatedBy: f.owner.ID,
		})
		if err != nil {
			t.Fatalf("CreateItem: %v", err)
		}
		visibleParent := mustItem(t, f.srv, f.wsA.ID, f.collA.ID, "Visible link parent")
		if _, err := f.srv.store.SetParentLink(f.wsA.ID, withColumn.ID, visibleParent.ID, f.owner.ID); err != nil {
			t.Fatalf("SetParentLink: %v", err)
		}
		// Fixture guard: the link must not have cleared the column, or the
		// case this test exists for cannot arise.
		reread, err := f.srv.store.GetItem(withColumn.ID)
		if err != nil || reread == nil || reread.ParentID == nil || *reread.ParentID != hiddenParent.ID {
			t.Fatalf("fixture: expected the parent_id column to survive SetParentLink; got %+v (err=%v)", reread, err)
		}
		f.source = withColumn

		w := restrictedWarnings(t, f, u)
		if !w.DroppedParent {
			t.Error("dropped_parent must be true — the caller CAN see the linked parent")
		}
		if !w.RelationshipsPartial {
			t.Error("relationships_partial must be true — a SECOND parent, in the column, is hidden from the caller")
		}
	})

	t.Run("hidden parent via the legacy parent_id column", func(t *testing.T) {
		f, secret, u := setup(t, "hidcolparent")
		parent := mustItem(t, f.srv, f.wsA.ID, secret.ID, "Hidden column parent")
		withColumn, err := f.srv.store.CreateItem(f.wsA.ID, f.collA.ID, models.ItemCreate{
			Title: "Has parent_id", Fields: `{"status":"open"}`,
			ParentID: &parent.ID, CreatedBy: f.owner.ID,
		})
		if err != nil {
			t.Fatalf("CreateItem: %v", err)
		}
		f.source = withColumn
		assertPartial(t, f, u, true, "a legacy parent_id column pointing into a hidden collection")
		if restrictedWarnings(t, f, u).DroppedParent {
			t.Error("dropped_parent must stay false for a column parent the caller cannot see")
		}
	})
}

// TestCopyPreflight_PartialMarkerForAnItemGrantOnlyGuest pins the
// narrowest caller this endpoint accepts: a GUEST with no membership in
// the source workspace at all, whose sole claim is an edit grant on the
// source item.
//
// Two things are being pinned at once.
//
// FIRST, the guest path really is filtered. relVisible composes
// visibleCollectionIDs with isItemVisibleToGuest, and a guest whose
// collection list came back nil would silently be treated as unrestricted
// — the filter AND the marker would both switch off, and every other test
// here uses collection_access="specific" rather than a grant, so none of
// them would notice.
//
// SECOND, and deliberately: this caller CAN learn one bit they cannot
// learn from /children, /links or /items, all of which return empty for
// them either way — namely that at least one relationship exists which
// they may not see. That single bit is the accepted price of TASK-2369.
// It carries no count, no type, no identity and no collection, and the
// alternative is telling someone about to run a MOVE that nothing will be
// stranded when something will. Recorded here so the trade is a decision
// on the record rather than an oversight (Codex round 9).
func TestCopyPreflight_PartialMarkerForAnItemGrantOnlyGuest(t *testing.T) {
	f := newCopyPreflightFixture(t)
	secret := mustSchemaCollection(t, f.srv, f.wsA.ID, "Secret", srcSchemaJSON)

	guest := mustUser(t, f.srv, "grantpartial@example.com", "grantpartial", "")
	if _, err := f.srv.store.CreateItemGrant(f.wsA.ID, f.source.ID, guest.ID, "edit", f.owner.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}
	if err := f.srv.store.AddWorkspaceMember(f.wsB.ID, guest.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember B: %v", err)
	}

	guestWarnings := func() ItemCopyPreflightWarnings {
		t.Helper()
		rr := f.call(guest, reqOpts{wsRoleCtx: "guest"}, f.baseBody())
		if rr.Code != http.StatusOK {
			t.Fatalf("guest preflight: %d %s", rr.Code, rr.Body.String())
		}
		var resp ItemCopyPreflight
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("parse: %v", err)
		}
		return resp.Warnings
	}

	// Nothing related yet: no marker. The guest is the caller most likely
	// to trip a "restricted therefore partial" shortcut.
	if guestWarnings().RelationshipsPartial {
		t.Fatal("an item-grant guest with no relationships must get no marker")
	}

	kid := mustItem(t, f.srv, f.wsA.ID, secret.ID, "Hidden child")
	if _, err := f.srv.store.SetParentLink(f.wsA.ID, kid.ID, f.source.ID, f.owner.ID); err != nil {
		t.Fatalf("SetParentLink: %v", err)
	}

	w := guestWarnings()
	if !w.RelationshipsPartial {
		t.Error("the guest path did not mark a hidden child — is the guest being treated as unrestricted?")
	}
	if w.ChildCount != 0 {
		t.Errorf("child_count = %d, want 0 — a grant on the source is not a grant on its children", w.ChildCount)
	}
	// The owner sees the same child as a real one, so the fixture is not
	// passing because the child failed to exist.
	if got := f.ok(f.baseBody()).Warnings.ChildCount; got != 1 {
		t.Fatalf("owner: child_count = %d, want 1 — fixture is not exercising a real child", got)
	}
}

// TestCopyPreflight_PartialMarkerDoesNotLeakHiddenCount is the negative
// test TASK-2369 asks for by name, and the reason the marker is a bare
// boolean rather than a count.
//
// Two workspaces are built identically except for how much is hidden from
// the caller: one has a single hidden child; the other has three hidden
// children, two hidden dependency edges of two different link types, and a
// hidden parent. If ANY channel of the answer varied with that — a number,
// a map key, an array length, bucket ordering, or the serialized byte
// length — a restricted caller could binary-search the hidden graph, which
// is the leak DR-10a, DR-10b and the moved-to pointer each closed
// separately.
//
// The assertion is byte equality over the WHOLE warnings block rather than
// a field-by-field comparison, precisely so a future field that does vary
// fails here instead of shipping.
func TestCopyPreflight_PartialMarkerDoesNotLeakHiddenCount(t *testing.T) {
	build := func(t *testing.T, tag string, seed func(f *copyPreflightFixture, secret *models.Collection)) []byte {
		t.Helper()
		f := newCopyPreflightFixture(t)
		secret := mustSchemaCollection(t, f.srv, f.wsA.ID, "Secret", srcSchemaJSON)
		u := f.restrictedEditor("leak"+tag+"@example.com", "leak"+tag,
			[]string{f.collA.ID}, []string{f.collB.ID})
		seed(f, secret)

		rr := f.call(u, reqOpts{wsRoleCtx: "editor"}, f.baseBody())
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: preflight %d: %s", tag, rr.Code, rr.Body.String())
		}
		var envelope struct {
			Warnings json.RawMessage `json:"warnings"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("%s: parse: %v", tag, err)
		}
		var w ItemCopyPreflightWarnings
		if err := json.Unmarshal(envelope.Warnings, &w); err != nil {
			t.Fatalf("%s: parse warnings: %v", tag, err)
		}
		// Falsifiability: if the fixture stopped hiding anything, byte
		// equality would pass for the wrong reason.
		if !w.RelationshipsPartial {
			t.Fatalf("%s: fixture is not exercising the marker at all", tag)
		}
		return envelope.Warnings
	}

	one := build(t, "one", func(f *copyPreflightFixture, secret *models.Collection) {
		kid := mustItem(t, f.srv, f.wsA.ID, secret.ID, "Hidden child")
		if _, err := f.srv.store.SetParentLink(f.wsA.ID, kid.ID, f.source.ID, f.owner.ID); err != nil {
			t.Fatalf("SetParentLink: %v", err)
		}
	})

	many := build(t, "many", func(f *copyPreflightFixture, secret *models.Collection) {
		for i := 0; i < 3; i++ {
			kid := mustItem(t, f.srv, f.wsA.ID, secret.ID, fmt.Sprintf("Hidden child %d", i))
			if _, err := f.srv.store.SetParentLink(f.wsA.ID, kid.ID, f.source.ID, f.owner.ID); err != nil {
				t.Fatalf("SetParentLink: %v", err)
			}
		}
		for _, lt := range []string{"blocks", "related"} {
			other := mustItem(t, f.srv, f.wsA.ID, secret.ID, "Hidden "+lt)
			if _, err := f.srv.store.CreateItemLink(f.wsA.ID,
				models.ItemLinkCreate{TargetID: other.ID, LinkType: lt, CreatedBy: f.owner.ID},
				f.source.ID); err != nil {
				t.Fatalf("CreateItemLink(%s): %v", lt, err)
			}
		}
		parent := mustItem(t, f.srv, f.wsA.ID, secret.ID, "Hidden parent")
		if _, err := f.srv.store.SetParentLink(f.wsA.ID, f.source.ID, parent.ID, f.owner.ID); err != nil {
			t.Fatalf("SetParentLink(parent): %v", err)
		}
		kid := mustItem(t, f.srv, f.wsA.ID, secret.ID, "Hidden legacy child")
		mustLegacyPlanLink(t, f.srv, f.wsA.ID, kid.ID, f.source.ID)
	})

	if !bytes.Equal(one, many) {
		t.Fatalf("the warning block varies with the number and type of HIDDEN relationships:\n one:  %s\n many: %s",
			one, many)
	}
	// Called out separately because byte length is the channel a
	// field-by-field comparison would miss.
	if len(one) != len(many) {
		t.Fatalf("warning block byte length leaks the hidden count: %d vs %d", len(one), len(many))
	}
}

// BUG-2674 / Codex round 1 P2. MigrateFields now carries reserved system
// metadata across a collection or workspace change instead of dropping it. The
// preflight builds `carried` by walking the DESTINATION SCHEMA, and these keys
// are declared by no schema anywhere — so without an explicit pass they land in
// neither bucket, and a copy of an item whose only content is notes reports
// "nothing carries over" while in fact retaining them.
//
// The assertion is on the preflight PROMISE matching what the copy DOES, in
// both directions: the key is reported carried, and it is genuinely present on
// the copied item. Asserting only the report would pass if the copy silently
// dropped it; asserting only persistence would pass if the report stayed
// silent — which is the exact defect being closed.
func TestCopyPreflight_ReportsReservedMetadataAsCarried(t *testing.T) {
	f := newCopyPreflightFixture(t)

	notes := `[{"id":"note-1","summary":"must be reported and must survive"}]`
	seeded := `{"status":"open","priority":"low","impact":"large","count":7,"code":"abc",` +
		`"implementation_notes":` + notes + `}`
	updated, err := f.srv.store.UpdateItem(f.source.ID, models.ItemUpdate{Fields: &seeded})
	if err != nil || updated == nil {
		t.Fatalf("seed reserved metadata: %v", err)
	}

	body := f.resolvableBody()
	body["field_overrides"] = map[string]any{"ticket": "T-1", "code": "123"}

	pre := f.ok(body)

	var found *ItemCopyPreflightCarried
	for i := range pre.Fields.Carried {
		if pre.Fields.Carried[i].Key == models.ItemFieldImplementationNotes {
			found = &pre.Fields.Carried[i]
			break
		}
	}
	if found == nil {
		var keys []string
		for _, c := range pre.Fields.Carried {
			keys = append(keys, c.Key)
		}
		t.Fatalf("implementation_notes missing from carried; got %v", keys)
	}
	if found.Type != "system" {
		t.Errorf("carried type = %q, want %q so clients can tell it resolves to no FieldDef", found.Type, "system")
	}
	if found.Label == models.ItemFieldImplementationNotes {
		t.Errorf("carried label is the raw key %q; reserved keys have no author-supplied label and must render a readable one", found.Label)
	}

	// It must not ALSO be reported dropped — the two buckets are exclusive
	// and a key in both is worse than a key in neither.
	for _, d := range pre.Fields.Dropped {
		if d.Key == models.ItemFieldImplementationNotes {
			t.Error("implementation_notes reported both carried and dropped")
		}
	}

	// And the promise has to be true. assertPreflightMatchesCopy walks every
	// `carried` entry and compares it against the COPY's persisted fields, so
	// running it here is what turns the report into a checked claim rather
	// than a label — it fails if the copy dropped what the preflight promised.
	res := assertPreflightMatchesCopy(t, f, "reserved metadata carries", body)

	got := f.persistedFields(res.Item.ID)
	if _, ok := got[models.ItemFieldImplementationNotes]; !ok {
		t.Fatalf("implementation_notes was reported carried but is absent on the copy: %#v", got)
	}
}

// BUG-2674, lead ruling. github_pr is REFERENTIAL system metadata: it names a
// repository belonging to the source workspace's context. It carries on an
// intra-workspace move (context unchanged) and drops on a cross-workspace copy,
// where carrying it would render a live PR link on an item whose project may
// have no relationship to that repo.
//
// The drop must be REPORTED — PLAN-2357 DR-17, "None of this may be silent" —
// and with a reason that explains itself. The generic no_target_field would be
// misleading here: no schema declares this key anywhere, so it is equally true
// of the source and says nothing about why the value is being left behind.
func TestCopyPreflight_ReportsReferentialMetadataAsNotPortable(t *testing.T) {
	f := newCopyPreflightFixture(t)

	seeded := `{"status":"open","priority":"low","impact":"large","count":7,"code":"abc",` +
		`"github_pr":{"number":42,"url":"https://example.invalid/42","state":"OPEN"},` +
		`"implementation_notes":[{"id":"note-1","summary":"travels anywhere"}]}`
	if _, err := f.srv.store.UpdateItem(f.source.ID, models.ItemUpdate{Fields: &seeded}); err != nil {
		t.Fatalf("seed referential metadata: %v", err)
	}

	body := f.resolvableBody()
	body["field_overrides"] = map[string]any{"ticket": "T-1", "code": "123"}
	pre := f.ok(body)

	var dropped *ItemCopyPreflightDropped
	for i := range pre.Fields.Dropped {
		if pre.Fields.Dropped[i].Key == models.ItemFieldGitHubPR {
			dropped = &pre.Fields.Dropped[i]
			break
		}
	}
	if dropped == nil {
		t.Fatalf("github_pr must be reported dropped on a cross-workspace copy; dropped = %+v", pre.Fields.Dropped)
	}
	if dropped.Reason != "referent_not_portable" {
		t.Errorf("reason = %q, want %q — no_target_field is true of the source too and explains nothing",
			dropped.Reason, "referent_not_portable")
	}

	// It must not also be reported carried, and the non-referential sibling
	// must NOT be swept up with it — that pair is the whole distinction.
	for _, c := range pre.Fields.Carried {
		if c.Key == models.ItemFieldGitHubPR {
			t.Error("github_pr reported both dropped and carried")
		}
	}
	var notesCarried bool
	for _, c := range pre.Fields.Carried {
		if c.Key == models.ItemFieldImplementationNotes {
			notesCarried = true
		}
	}
	if !notesCarried {
		t.Error("implementation_notes must still carry across workspaces — it describes the item, not its surroundings")
	}
}
