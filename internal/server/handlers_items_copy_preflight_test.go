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
	f.srv.dispatchWebhook(f.wsB.ID, "item.created", map[string]string{"probe": "1"})
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
