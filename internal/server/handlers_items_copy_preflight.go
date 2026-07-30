package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/items"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// Cross-workspace copy PREFLIGHT — PLAN-2357 / TASK-2364, implementing
// DR-6, DR-12 and DR-15.
//
// ROUTE (the contract; Phase 3's dialog and the CLI both build against it):
//
//	POST /api/v1/workspaces/{slug}/items/{itemSlug}/copy/preflight
//
// The URL's workspace is the SOURCE. The destination is named in the body.
// The mutating sibling (TASK-2365) is POST …/items/{itemSlug}/copy and
// takes the identical request shape.
//
// REQUEST
//
//	{
//	  "target_workspace":  "pad-web",              // required; slug or UUID
//	  "target_collection": "tasks",                // required; collection slug in the destination
//	  "field_overrides":   {"priority": "high"},   // optional; key MUST be declared by the destination schema
//	  "archive_source":    false                   // optional; the MOVE path (copy + archive source)
//	}
//
// `field_overrides` mirrors the existing move endpoint's field of the same
// name: a flat map of destination-schema field key → value. Keys the
// destination schema does not declare are REJECTED rather than passed
// through — see the malformed_override status below.
//
// A null value means "unset this key", and it is REMOVED from the map
// rather than set to nil. That distinction is normative, not incidental:
// ValidateFields treats a nil value as absent for the required check but
// LEAVES IT IN THE MAP, so assigning nil instead of deleting persists a
// literal `"key": null` into items.fields — a value this preview reports
// as unset. Delete, then validate.
//
// RECONCILED in TASK-2365. Store.migrateCopyFields
// (internal/store/items_cross_workspace_copy.go) used to do
// `migrated.Fields[k] = v` for every override including a nil one, so a
// null override this preflight showed as unset was written as JSON null
// by the copy. It now deletes the key, matching the loop below.
// TestCopyEndpoint_PreflightAndCopyAgreeOnNullOverride runs both paths
// over one input and fails if they drift apart again.
//
// What a nulled key becomes therefore depends on the destination schema,
// not on the null: validation re-applies any DEFAULT the key has, so a
// nulled field with a default comes back in `carried` with from="default"
// (required or not). Only a REQUIRED field with NO default lands in
// `needs_value`; an optional one with no default is simply absent.
//
// RESPONSE — 200, and see the doc comment on each type below for the
// per-field contract. The three bucket names `carried` / `dropped` /
// `needs_value` are the contract (DR-15); renaming one is a breaking
// change. All three arrays and both link maps are ALWAYS present, never
// null.
//
// ERROR STATUSES (DR-15 requires these three to be distinguishable):
//
//	400 invalid_body         — the JSON body did not decode
//	400 missing_field        — target_workspace / target_collection absent
//	400 malformed_override   — an override names a field the destination
//	                           schema does not declare
//	400 invalid_override     — an override's VALUE fails the destination
//	                           schema's type / options / pattern rules
//	                           (DR-12's second half)
//	403 forbidden            — destination workspace not accessible
//	403 permission_denied    — destination workspace outside the bearer
//	                           token's consent allow-list
//	404 not_found            — source item absent or not visible, or the
//	                           caller may not edit it (WriteHidden; the
//	                           source side never distinguishes the two)
//	409 archived             — the source item exists and the caller can
//	                           see it, but it is archived. Shared with the
//	                           move and update paths via
//	                           writeItemResolveError; the copy path has no
//	                           reason to be the one endpoint that reports
//	                           an archived item as simply absent, and a
//	                           client can say something useful about it.
//	404 collection_not_found — destination collection absent, archived,
//	                           foreign, or hidden from the caller
//	403 forbidden            — destination collection visible but the
//	                           caller may not create into it
//	403 actor_required       — there is nobody to attribute the copy to (no
//	                           authenticated user AND a source item with no
//	                           creator). Shared verbatim with the mutating
//	                           copy so the preview cannot promise something
//	                           the copy would refuse.
//
// NON-MUTATION is part of the contract (DR-15), scoped to the COPY's own
// domain: the handler creates no item, no attachment rows and no
// provenance row, advances NEITHER workspace's seq, and emits no
// activity, SSE or webhook. It is safe to call repeatedly from a live UI
// as the user changes the destination, and repeated identical calls
// return identical bytes — which is why every list below is sorted
// deterministically rather than left in Go map-iteration order.
//
// It is NOT a claim that the REQUEST is side-effect-free. Every
// authenticated request in Pad touches the session/activity machinery
// before a handler runs (RequireAuth's asynchronous users.last_active_at
// write, session renewal and IP-rotation audit rows) and mutates process
// state on the way through (rate-limiter buckets, request metrics, the
// compiled-pattern cache). Those are properties of the middleware stack,
// identical for a GET, and DR-15 is not about them. Read the guarantee as
// "a preflight leaves no trace a copy would have left".
//
// RESIDUAL, recorded rather than fixed (Codex round 3). Because DR-11
// requires attachment references to be enumerated from the FINAL
// destination fields, a caller who may edit the source item can put a
// `pad-attachment:<uuid>` literal into a text override and read back
// whether that UUID resolves inside the SOURCE workspace, and its exact
// byte size, via attachment_count / attachment_bytes /
// unresolvable_ref_count. Not fixed here for three reasons: attachment
// ids are unguessable v4 UUIDs, so this enumerates nothing an attacker
// does not already hold; the MUTATING copy has the identical property by
// design (the same override would clone the blob outright), so narrowing
// only the preflight would buy nothing; and enumerating from the
// pre-override fields instead would violate DR-11's stated ordering and
// make the dry-run's byte total disagree with the copy's — which is the
// entire reason the planner is shared between them. If this is judged
// worth closing it belongs at the planner's scope, as a DR-11a
// amendment covering both callers, not as a divergence here.

// itemCopyPreflightRequest is the wire shape. TASK-2365's mutating copy
// takes the same one.
type itemCopyPreflightRequest struct {
	TargetWorkspace  string         `json:"target_workspace"`
	TargetCollection string         `json:"target_collection"`
	FieldOverrides   map[string]any `json:"field_overrides"`
	ArchiveSource    bool           `json:"archive_source"`
}

// ItemCopyPreflight is the 200 response.
type ItemCopyPreflight struct {
	Source      ItemCopyPreflightSource      `json:"source"`
	Destination ItemCopyPreflightDestination `json:"destination"`

	// ArchiveSource echoes the request so a client rendering a cached
	// response cannot mistake a copy preview for a move preview. It also
	// selects the weighting of Warnings.ChildrenOrphaned (DR-4).
	ArchiveSource bool `json:"archive_source"`

	// Valid means EXACTLY ONE THING: the field mapping is complete —
	// NeedsValue is empty, so the caller has nothing left to resolve. It
	// is the boolean a dialog's primary button should gate on.
	//
	// It is NOT a prediction that the copy will succeed, and a client that
	// treats it as one will mishandle the mutating call's error path.
	// Conditions it deliberately does not evaluate, all of which live
	// inside TASK-2363's write transaction because that is the only place
	// they can be decided correctly (PLAN-2357: "a dry-run's schema
	// snapshot is advisory; the in-tx read is authoritative"):
	//
	//   - unique_scope collisions — a carried invocation_slug can collide
	//     with an existing destination item and hit the partial unique
	//     index on insert;
	//   - the destination workspace's items_per_workspace quota (DR-16),
	//     which is enforced in-tx and is advisory anywhere else;
	//   - cross-backend attachment transfer, which the copy resolves with
	//     a target backend the preflight does not supply;
	//   - anything that changes between this call and the copy — the
	//     destination collection being archived or reshaped, the source
	//     being moved or archived, the caller's grants being revoked.
	//
	// It is not a permission statement either; an unauthorized caller
	// never reaches a 200.
	Valid bool `json:"valid"`

	Fields   ItemCopyPreflightFields   `json:"fields"`
	Warnings ItemCopyPreflightWarnings `json:"warnings"`
}

// ItemCopyPreflightSource identifies the item being copied, in displayable
// terms only — the caller has already been authorized to see it.
type ItemCopyPreflightSource struct {
	WorkspaceSlug  string `json:"workspace_slug"`
	CollectionSlug string `json:"collection_slug"`
	Ref            string `json:"ref,omitempty"`
	Slug           string `json:"slug"`
	Title          string `json:"title"`
}

// ItemCopyPreflightDestination identifies where the copy would land.
// Populated only after all four authorization checks have passed, so it
// never discloses a workspace or collection the caller cannot reach.
type ItemCopyPreflightDestination struct {
	WorkspaceSlug  string `json:"workspace_slug"`
	WorkspaceName  string `json:"workspace_name"`
	CollectionSlug string `json:"collection_slug"`
	CollectionName string `json:"collection_name"`
}

// ItemCopyPreflightFields is the bucketing (DR-6/DR-15). The three names
// are the contract. Every slice is non-nil.
type ItemCopyPreflightFields struct {
	// Carried are the fields the copy WOULD have, in destination-schema
	// order.
	Carried []ItemCopyPreflightCarried `json:"carried"`

	// Dropped are values that will not survive the copy: schema fields
	// with no usable counterpart in the destination, plus the assignment
	// pair (DR-8), which is not a schema field but is reported here
	// because DR-8 requires it to appear in this bucket.
	Dropped []ItemCopyPreflightDropped `json:"dropped"`

	// NeedsValue are destination fields the copy cannot satisfy on its
	// own: required-and-empty, or carrying a value the destination schema
	// rejects. Supply an override for each and the entry clears.
	NeedsValue []ItemCopyPreflightNeedsValue `json:"needs_value"`
}

// ItemCopyPreflightCarried is one field that survives to the destination.
type ItemCopyPreflightCarried struct {
	Key   string `json:"key"`
	Label string `json:"label,omitempty"`
	Type  string `json:"type,omitempty"`
	// Value is the FINAL value — after migration, after overrides, after
	// schema defaults.
	Value any `json:"value"`
	// From explains where Value came from:
	//   "migrated" — carried across from the source item
	//   "override" — supplied by the caller's field_overrides
	//   "default"  — filled in from the destination schema's default
	From string `json:"from"`
}

// ItemCopyPreflightDropped is one value that will not be copied.
type ItemCopyPreflightDropped struct {
	Key   string `json:"key"`
	Label string `json:"label,omitempty"`
	// Kind is "field" for a collection-schema field, or "assignment" for
	// the assignee / agent-role pair (DR-8), which live on the item row
	// rather than in its fields.
	Kind string `json:"kind"`
	// Reason is one of:
	//   "no_target_field"          — destination schema has no such key
	//   "incompatible_type"        — the key exists in BOTH schemas but
	//                                the value cannot be converted
	//                                (includes a select value absent from
	//                                the destination's options)
	//   "undeclared_source_field"  — the item carries a key its OWN
	//                                collection schema does not declare
	//                                (an orphan left by a schema edit).
	//                                MigrateFields has no source type to
	//                                convert from, so it drops the value
	//                                even when the destination declares
	//                                the key — which is a different fact
	//                                from a type mismatch and reads very
	//                                differently in a dialog
	//   "assignee_not_a_member"    — assignee is not a member of the
	//                                destination workspace (DR-8)
	//   "agent_role_not_portable"  — role slugs are workspace-local and
	//                                never carry (DR-8)
	Reason string `json:"reason"`
}

// ItemCopyPreflightNeedsValue is one destination field the caller must
// resolve with an override before the copy can proceed.
type ItemCopyPreflightNeedsValue struct {
	Key      string   `json:"key"`
	Label    string   `json:"label,omitempty"`
	Type     string   `json:"type,omitempty"`
	Options  []string `json:"options,omitempty"`
	Required bool     `json:"required"`
	// Reason is "missing_required" (no value and no default) or
	// "invalid_value" (a value carried across that the destination schema
	// rejects — only reachable for non-override values; an INVALID
	// OVERRIDE is a 400, not a bucket entry).
	Reason string `json:"reason"`
	// Message is the validator's explanation, for display.
	Message string `json:"message,omitempty"`
}

// ItemCopyPreflightWarnings is DR-15's full warning set. Nothing here
// blocks the copy; all of it is information the user is entitled to
// before agreeing (DR-17: "none of this may be silent").
type ItemCopyPreflightWarnings struct {
	// ChildCount is how many live child items the source has (DR-4).
	// They are never copied.
	ChildCount int `json:"child_count"`

	// ChildrenOrphaned is ChildCount > 0 AND archive_source — the move
	// path, where archiving the source leaves the children parentless in
	// the source workspace. This is DR-4's "weighted more heavily on the
	// move path" made explicit rather than left to the client.
	ChildrenOrphaned bool `json:"children_orphaned"`

	// DroppedParent is true when the source has a parent item; the copy
	// is unparented (DR-17). The parent is deliberately NOT named — it is
	// an item in the source workspace whose own visibility this endpoint
	// has not checked.
	//
	// Counts children reached by EITHER mechanism (see DroppedParent),
	// deduplicated. A child that carries both a link row and the column
	// counts once.
	//
	// TWO parent mechanisms are consulted, because the copy scrubs both:
	// the `parent` item_links edge every Pad surface resolves and displays
	// (readParentLinkTarget, enrichItemForResponse, GetChildItems), AND
	// the items.parent_id COLUMN, which models.ItemCreate accepts on the
	// create route and CreateItem inserts verbatim. Either one alone would
	// under-report — an item created through the API with a raw parent_id
	// has no item_links row at all, and CopyItemAcrossWorkspaces sets
	// ParentID: nil regardless.
	DroppedParent bool `json:"dropped_parent"`

	// OutgoingLinks / IncomingLinks count the source's dependency edges
	// by link_type — the "blocked task that silently becomes actionable"
	// case DR-17 calls out. None of them carry.
	//
	// Hierarchy link types ("parent", "implements" and the legacy "plan"
	// alias) are EXCLUDED from both maps: they are reported by ChildCount
	// and DroppedParent, and counting them twice would let a client that
	// sums the maps overstate the loss. Always non-nil; empty means no
	// dependency edges.
	OutgoingLinks map[string]int `json:"outgoing_links"`
	IncomingLinks map[string]int `json:"incoming_links"`

	// DroppedAssignee / DroppedAgentRole mirror the corresponding
	// `dropped` bucket entries (DR-8). An assignee who IS a member of the
	// destination workspace carries, so DroppedAssignee is false in the
	// common same-owner case.
	DroppedAssignee  bool `json:"dropped_assignee"`
	DroppedAgentRole bool `json:"dropped_agent_role"`

	// AttachmentCount / AttachmentBytes are what the copy would add to
	// the destination workspace's storage. REPORTED, NOT ENFORCED
	// (DR-16). Bytes are the sum of size_bytes over every row that would
	// be created, including thumbnail variants, matching what the
	// destination's storage page will show afterwards.
	AttachmentCount int   `json:"attachment_count"`
	AttachmentBytes int64 `json:"attachment_bytes"`

	// UnresolvableRefCount is how many pad-attachment: references in the
	// copied payload resolve to nothing under the source workspace's
	// scope (DR-11a). They are not cloned and do not block the copy; the
	// copy renders exactly as broken as the source does.
	UnresolvableRefCount int `json:"unresolvable_ref_count"`
}

// legacyHierarchyLinkTypes are hierarchy link types store.ChildLinkTypes
// does NOT walk. Today that is just "plan", the pre-rename alias that can
// still sit alongside a "parent" row on the same edge (see
// GetChildItemsTx's DISTINCT, which exists because of exactly that
// duplication).
//
// A LONE "plan" edge — one with no accompanying "parent" row — is
// therefore counted as neither a child nor a dependency. That is
// deliberate: GetChildItems does not consider it a child, so calling it
// one here would make child_count disagree with every other surface, and
// calling it a dependency would report a blocker that does not exist.
// Under-reporting a legacy duplicate is the safe direction, and the
// condition is not reachable through any current write path.
var legacyHierarchyLinkTypes = []string{"plan"}

// hierarchyLinkTypes are the link types that express parent/child rather
// than dependency, and so are reported by child_count / dropped_parent
// instead of by the link maps.
//
// DERIVED from store.ChildLinkTypes rather than restated, so a new child
// link type added in the store cannot silently start being counted here
// as a dependency edge — which would misreport a parent/child
// relationship as a blocker. TestPreflightHierarchyTypesCoverStoreChildren
// pins the containment.
var hierarchyLinkTypes = buildHierarchyLinkTypes()

func buildHierarchyLinkTypes() map[string]bool {
	out := map[string]bool{}
	for _, t := range store.ChildLinkTypes() {
		out[t] = true
	}
	for _, t := range legacyHierarchyLinkTypes {
		out[t] = true
	}
	return out
}

// handleCopyItemPreflight answers "what would this cross-workspace copy
// do?" without doing any of it. See the file header for the full contract.
func (s *Server) handleCopyItemPreflight(w http.ResponseWriter, r *http.Request) {
	sourceWorkspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	itemSlug := chi.URLParam(r, "itemSlug")
	item, err := s.store.ResolveItem(sourceWorkspaceID, itemSlug)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if item == nil {
		// ResolveItem filters soft-deleted rows, so this covers "absent"
		// AND "archived". writeItemResolveError separates them the same
		// way the move and update paths do — 409 archived when the caller
		// can independently SEE the archived row, 404 otherwise — so the
		// distinction is never made for someone who could not see it.
		s.writeItemResolveError(w, r, sourceWorkspaceID, itemSlug)
		return
	}

	// ---- Authorization, DR-10a/DR-10b, four checks in order -----------
	//
	// Checks 1 and 2 (source item visibility, then source edit) compose
	// out of one AuthorizeCrossWorkspaceEdit call with an item scope; 3
	// and 4 (destination collection visibility, then destination edit)
	// out of one with a collection scope. The early return between them
	// is part of the ordering, not style: a destination verdict built for
	// a caller who could not read the source is itself a disclosure.
	//
	// The helper is used for the SOURCE too, even though it is the
	// request's own workspace, because it derives the role from
	// membership rather than from workspaceRole(r) — the same answer the
	// front door gives, computed without the context value the
	// destination half must not touch. One code path, one ordering.
	src := s.AuthorizeCrossWorkspaceEdit(r, sourceWorkspaceID, CrossWorkspaceItemScope(item))
	if !src.Allowed {
		// WriteHidden: absence and forbidden-ness are one 404. A preflight
		// that confirmed a hidden item exists would be the leak DR-10b is
		// about.
		src.WriteHidden(w, "Item")
		return
	}

	// decodeJSON, not json.NewDecoder: it wraps the body in a
	// MaxBytesReader so field_overrides cannot be used to make the server
	// allocate a multi-GB map on an endpoint that is designed to be called
	// repeatedly from a live UI (Codex round 7).
	var input itemCopyPreflightRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "Invalid JSON body")
		return
	}
	if input.TargetWorkspace == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "target_workspace is required")
		return
	}
	if input.TargetCollection == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "target_collection is required")
		return
	}

	// Check 3, part one: workspace-level access to the destination. This
	// is the narrow legitimate use of the workspace-only scope — an early
	// reject BEFORE the collection is known, because resolving a
	// collection slug requires the destination workspace's ID and doing
	// that lookup first would answer a question about a workspace the
	// caller may have no business addressing.
	dstWS := s.AuthorizeCrossWorkspaceEdit(r, input.TargetWorkspace, CrossWorkspaceWorkspaceOnlyScope())
	if !dstWS.Allowed {
		// WriteDenied, not WriteHidden: the caller named this workspace
		// themselves, so acknowledging the refusal tells them nothing they
		// did not already assert. It still refuses to separate "absent"
		// from "forbidden".
		dstWS.WriteDenied(w)
		return
	}

	targetColl, err := s.store.GetCollectionBySlug(dstWS.WorkspaceID(), input.TargetCollection)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if targetColl == nil {
		writeError(w, http.StatusNotFound,
			crossWorkspaceCollectionNotFoundCode, crossWorkspaceCollectionNotFoundMessage)
		return
	}

	// Checks 3 (collection visibility) and 4 (collection edit). The
	// collection-scoped denial is written by WriteCollectionNotFound,
	// which collapses "hidden" onto the same 404 the nil branch above
	// emits — otherwise a restricted member of the destination could
	// enumerate the collections they were excluded from.
	dst := s.AuthorizeCrossWorkspaceEdit(r, input.TargetWorkspace, CrossWorkspaceCollectionScope(targetColl.ID))
	if !dst.Allowed {
		dst.WriteCollectionNotFound(w)
		return
	}

	// Who the copy would be attributed to, resolved through the SAME function
	// the mutating copy uses and refused here on the same terms. Nothing is
	// written on this path, so a placeholder would work — and that is exactly
	// the trap: an earlier version supplied a `"preflight"` literal as a last
	// resort, so this endpoint happily previewed a copy the mutation would
	// refuse outright for want of an actor (Codex round 4).
	//
	// Placed HERE, immediately after the fourth authorization check and
	// BEFORE any field or override handling, because the mutating copy checks
	// it in the same position. Leaving it down by the attachment planner made
	// the two disagree about a request that fails BOTH ways — no actor and a
	// malformed override — with the preview reporting the override and the
	// copy the actor (Codex round 5). Agreement is about the ORDER of
	// refusals, not just the set.
	actorID := copyActorID(r, item)
	if actorID == "" {
		writeCopyActorRequired(w)
		return
	}

	// ---- Everything below this line is authorized and read-only -------
	//
	// SNAPSHOT CAVEAT. What follows is a sequence of independent, unlocked
	// reads, so a concurrent write can leave the buckets and the warnings
	// describing slightly different moments — and the authorization
	// verdict above is explicitly not atomic either (see
	// AuthorizeCrossWorkspaceEdit). That is by design and is why the
	// mutating copy re-reads both sides and re-applies every check inside
	// its transaction (DR-9): a dry-run's snapshot is advisory, the in-tx
	// read is authoritative. Taking locks here would put a read-only
	// preview that a live UI calls on every keystroke in contention with
	// real writes, to buy a consistency the copy cannot rely on anyway.

	sourceColl, err := s.store.GetCollection(item.CollectionID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if sourceColl == nil {
		writeInternalError(w, fmt.Errorf("copy preflight: source collection %s missing", item.CollectionID))
		return
	}

	var sourceSchema, targetSchema models.CollectionSchema
	if err := json.Unmarshal([]byte(sourceColl.Schema), &sourceSchema); err != nil {
		writeInternalError(w, fmt.Errorf("copy preflight: parse source schema: %w", err))
		return
	}
	if err := json.Unmarshal([]byte(targetColl.Schema), &targetSchema); err != nil {
		writeInternalError(w, fmt.Errorf("copy preflight: parse target schema: %w", err))
		return
	}

	targetDefs := make(map[string]models.FieldDef, len(targetSchema.Fields))
	for _, f := range targetSchema.Fields {
		targetDefs[f.Key] = f
	}

	// An override naming a field the destination does not declare is a
	// client bug, and silently ignoring it would let a dialog show a
	// value that never lands. Rejected before anything else is computed
	// so the 400 cannot depend on the source item's contents.
	//
	// RECONCILED in TASK-2365. Store.migrateCopyFields used to merge every
	// override unconditionally, and ValidateFields ignores keys the schema
	// does not declare, so the copy PERSISTED an undeclared override as an
	// orphan key on the new item while this preflight refused the same
	// request. The gate now exists on both sides
	// (store.UndeclaredOverrideError, surfaced by the copy endpoint as the
	// same 400 malformed_override this emits);
	// TestCopyEndpoint_PreflightAndCopyAgreeOnUndeclaredOverride pins it.
	// items.UndeclaredOverrideKeys, the SAME function the store's copy path
	// calls. It moved into internal/items in TASK-2365 because two
	// implementations of "which keys are undeclared" is precisely the DR-6
	// divergence this pair of endpoints exists to prevent (Codex round 17).
	if bad := items.UndeclaredOverrideKeys(input.FieldOverrides, targetSchema.Fields); len(bad) > 0 {
		writeError(w, http.StatusBadRequest, "malformed_override",
			"Destination collection has no field(s): "+summarizeKeys(bad))
		return
	}

	// A fields blob that will not decode into an object is treated as
	// empty, matching handleMoveItem and the bulk move path. Diverging —
	// 500ing on corrupt data the move path happily migrates — would make
	// the preview refuse a copy the copy itself would accept, which is the
	// DR-6 disagreement this endpoint exists to prevent. It also covers the
	// benign empty-string case on old rows.
	var currentFields map[string]any
	if err := json.Unmarshal([]byte(item.Fields), &currentFields); err != nil {
		currentFields = map[string]any{}
	}

	// ---- DR-12: migrate → apply overrides → THEN validate -------------
	//
	// LIMITATION, inherited from MigrateFields and shared with the
	// existing intra-workspace move path (Codex round 5). Migration
	// matches on key and type only — plus the option list for a select.
	// It does not consider a relation field's `collection`, `computed`,
	// `terminal_options` or `unique_scope`. So a same-named `relation`
	// field carries a SOURCE-workspace item id into the destination and
	// is reported as a clean carry, when across workspaces it is a
	// dangling reference. Reporting that here would require the preflight
	// to model semantics MigrateFields does not, and the copy would then
	// disagree with its own preview — the divergence DR-6 exists to
	// prevent. It belongs in MigrateFields, for both callers at once.
	//
	// MigrateFields computes result.Errors before any override exists, so
	// those errors are stale the instant an override merges in. They are
	// deliberately never read here; the authoritative answer comes from
	// items.ValidateFieldsDetailed run over the MERGED map, which also
	// applies destination defaults and type/option/pattern checks.
	migrated := items.MigrateFields(currentFields, sourceSchema.Fields, targetSchema.Fields)

	final := make(map[string]any, len(migrated.Fields)+len(input.FieldOverrides))
	origin := make(map[string]string, len(final))
	for k, v := range migrated.Fields {
		final[k] = v
		// MigrateFields' output is two things at once: values carried
		// across from the source item, and destination-schema defaults it
		// filled in for keys the source had nothing for. Presence in the
		// SOURCE's field map is what separates them.
		if _, fromSource := currentFields[k]; fromSource {
			origin[k] = "migrated"
		} else {
			origin[k] = "default"
		}
	}
	overridden := make(map[string]bool, len(input.FieldOverrides))
	for k, v := range input.FieldOverrides {
		overridden[k] = true
		if v == nil {
			// An explicit null means "leave this unset" — drop it so the
			// validator sees a genuinely absent key (and re-reports it as
			// needs_value if the destination requires it).
			delete(final, k)
			delete(origin, k)
			continue
		}
		final[k] = v
		origin[k] = "override"
	}
	// ValidateFieldsDetailed injects any remaining schema defaults into
	// `final` in place, so a key that appears only afterwards has no
	// origin entry and is reported as "default".
	issues := items.ValidateFieldsDetailed(final, targetSchema)

	// DR-12's other half: an override whose VALUE is invalid is rejected,
	// not bucketed. It is the caller's own input and there is nothing for
	// them to resolve in a needs_value row that they did not just type.
	var badOverrides []string
	issueByKey := make(map[string]items.FieldIssue, len(issues))
	for _, iss := range issues {
		issueByKey[iss.Key] = iss
		if iss.Kind == items.IssueInvalid && overridden[iss.Key] {
			badOverrides = append(badOverrides, iss.Message)
		}
	}
	if len(badOverrides) > 0 {
		sort.Strings(badOverrides)
		// Bounded for the same reason the malformed_override message is:
		// validateFieldType quotes the offending VALUE verbatim, so a
		// single ~2 MiB override string would otherwise be reflected back
		// in full. The cap keeps the 400 useful without making the
		// endpoint an amplifier.
		writeError(w, http.StatusBadRequest, "invalid_override",
			"Invalid override value(s): "+summarizeMessages(badOverrides))
		return
	}

	resp := ItemCopyPreflight{
		Source: ItemCopyPreflightSource{
			// The CANONICAL slug from the resolved workspace, never the URL
			// parameter: /workspaces/{slug} also accepts a UUID, and echoing
			// that back in a field a client uses to build a web route hands
			// it a link that does not resolve (Codex round 14). Same reason
			// the destination reports dst.WorkspaceSlug().
			WorkspaceSlug:  src.WorkspaceSlug(),
			CollectionSlug: sourceColl.Slug,
			Ref:            item.Ref,
			Slug:           item.Slug,
			Title:          item.Title,
		},
		Destination: ItemCopyPreflightDestination{
			WorkspaceSlug:  dst.WorkspaceSlug(),
			WorkspaceName:  dst.Workspace.Name,
			CollectionSlug: targetColl.Slug,
			CollectionName: targetColl.Name,
		},
		ArchiveSource: input.ArchiveSource,
		Fields: ItemCopyPreflightFields{
			Carried:    []ItemCopyPreflightCarried{},
			Dropped:    []ItemCopyPreflightDropped{},
			NeedsValue: []ItemCopyPreflightNeedsValue{},
		},
		Warnings: ItemCopyPreflightWarnings{
			OutgoingLinks: map[string]int{},
			IncomingLinks: map[string]int{},
		},
	}

	// carried / needs_value, walked in destination-schema order so the
	// response is stable across identical calls.
	for _, def := range targetSchema.Fields {
		if iss, bad := issueByKey[def.Key]; bad {
			reason := "invalid_value"
			if iss.Kind == items.IssueRequired {
				reason = "missing_required"
			}
			resp.Fields.NeedsValue = append(resp.Fields.NeedsValue, ItemCopyPreflightNeedsValue{
				Key:      def.Key,
				Label:    def.Label,
				Type:     def.Type,
				Options:  def.Options,
				Required: def.Required,
				Reason:   reason,
				Message:  iss.Message,
			})
			continue
		}
		val, present := final[def.Key]
		if !present {
			continue
		}
		from := origin[def.Key]
		if from == "" {
			from = "default"
		}
		resp.Fields.Carried = append(resp.Fields.Carried, ItemCopyPreflightCarried{
			Key:   def.Key,
			Label: def.Label,
			Type:  def.Type,
			Value: val,
			From:  from,
		})
	}

	// dropped — schema fields first, in SOURCE-schema order (MigrateFields
	// ranges a map, so its Dropped slice arrives in nondeterministic
	// order and must be re-sorted or repeated calls would differ).
	for _, key := range sortedDroppedKeys(migrated.Dropped, sourceSchema.Fields) {
		reason := "no_target_field"
		label := key
		srcDef, declaredBySource := fieldDefByKey(sourceSchema.Fields, key)
		if def, exists := targetDefs[key]; exists {
			// The key exists downstream, so migration rejected the VALUE.
			// Which of the two ways matters to whoever reads this: a
			// genuine type mismatch, or a key the SOURCE schema never
			// declared — MigrateFields then has no source type to convert
			// from and drops the value unconditionally, even when the two
			// keys would otherwise have been compatible.
			reason = "incompatible_type"
			if !declaredBySource {
				reason = "undeclared_source_field"
			}
			if def.Label != "" {
				label = def.Label
			}
		} else if declaredBySource && srcDef.Label != "" {
			label = srcDef.Label
		}
		resp.Fields.Dropped = append(resp.Fields.Dropped, ItemCopyPreflightDropped{
			Key: key, Label: label, Kind: "field", Reason: reason,
		})
	}

	// dropped — the assignment pair (DR-8). Not schema fields, but DR-8
	// requires them in this bucket, and they carry a parallel boolean in
	// warnings so a client can render either surface.
	if item.AssignedUserID != nil && *item.AssignedUserID != "" {
		member, mErr := s.store.GetWorkspaceMember(dst.WorkspaceID(), *item.AssignedUserID)
		if mErr != nil {
			writeInternalError(w, mErr)
			return
		}
		if member == nil {
			resp.Warnings.DroppedAssignee = true
			resp.Fields.Dropped = append(resp.Fields.Dropped, ItemCopyPreflightDropped{
				Key: "assigned_user", Label: "Assignee", Kind: "assignment",
				Reason: "assignee_not_a_member",
			})
		}
	}
	if item.AgentRoleID != nil && *item.AgentRoleID != "" {
		resp.Warnings.DroppedAgentRole = true
		resp.Fields.Dropped = append(resp.Fields.Dropped, ItemCopyPreflightDropped{
			Key: "agent_role", Label: "Agent role", Kind: "assignment",
			Reason: "agent_role_not_portable",
		})
	}

	// ---- Relationship warnings (DR-4 / DR-17) -------------------------
	//
	// The counters are CALLER-RELATIVE, deliberately. Store.GetChildItems
	// and Store.GetItemLinks apply no ACL of their own; the /children and
	// /links endpoints filter their results by collection visibility and
	// item grants afterwards, and this must do the same or it becomes a
	// wider window onto the source workspace than the endpoints that
	// already answer these questions. A caller holding nothing but an edit
	// grant on the source item would otherwise learn the exact number and
	// type of children and dependency edges attached to it, every one of
	// which may live in a collection hidden from them (Codex round 3).
	//
	// The trade is that a hidden child still gets left behind without being
	// counted. That is the correct side to err on: the alternative reports
	// a loss the caller was never entitled to know about, and the same
	// asymmetry already exists everywhere else these relationships surface.
	relVisibleIDs, err := s.visibleCollectionIDs(r, sourceWorkspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	relFullCollIDs, relGrantedItemIDs, err := s.guestResourceFilter(r, sourceWorkspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	// relVisibleIDs == nil means "unrestricted caller"; both helper sets are
	// then irrelevant and every relationship is legitimately visible.
	relVisible := func(other *models.Item) bool {
		if relVisibleIDs == nil {
			return true
		}
		if other == nil {
			return false
		}
		if !isCollectionVisible(other.CollectionID, relVisibleIDs) {
			return false
		}
		return s.isItemVisibleToGuest(r, sourceWorkspaceID, other, relFullCollIDs, relGrantedItemIDs)
	}

	// Children come from BOTH mechanisms, for the same reason
	// DroppedParent consults both: GetChildItems joins item_links and
	// never looks at items.parent_id, while an item created through the
	// API with a raw parent_id has no link row at all. Missing those
	// would leave children_orphaned false on a move that strands them
	// (Codex round 12). Deduplicated by id — a child can legitimately
	// carry both.
	children, err := s.store.GetChildItems(item.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	// ListItems is workspace-scoped and filters soft-deleted rows, so the
	// column-side query needs no extra guard of its own.
	columnChildren, err := s.store.ListItems(sourceWorkspaceID, models.ItemListParams{
		ParentID:  item.ID,
		NoContent: true,
	})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	countedChildren := make(map[string]bool, len(children)+len(columnChildren))
	for _, set := range [][]models.Item{children, columnChildren} {
		for i := range set {
			if countedChildren[set[i].ID] || !relVisible(&set[i]) {
				continue
			}
			countedChildren[set[i].ID] = true
		}
	}
	resp.Warnings.ChildCount = len(countedChildren)
	resp.Warnings.ChildrenOrphaned = len(countedChildren) > 0 && input.ArchiveSource

	links, err := s.store.GetItemLinks(item.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	for _, link := range links {
		if relVisibleIDs != nil {
			// Judge the OTHER end of the edge, the same way
			// handleGetItemLinks does.
			otherID := link.TargetID
			if otherID == item.ID {
				otherID = link.SourceID
			}
			other, oErr := s.store.GetItem(otherID)
			if oErr != nil {
				// DELIBERATE divergence from handleGetItemLinks, which
				// swallows this error and omits the link. Omitting a row
				// from a LIST is a partial answer; omitting it from a
				// COUNT is a wrong one, and the user is being asked to
				// agree to a loss on the strength of that number. Fail
				// loudly rather than under-report.
				writeInternalError(w, oErr)
				return
			}
			// A nil `other` (the row vanished between the two reads) is
			// skipped by relVisible, matching /links.
			if !relVisible(other) {
				continue
			}
		}
		if hierarchyLinkTypes[link.LinkType] {
			if link.SourceID == item.ID {
				// The source's own parent edge — the copy is unparented.
				resp.Warnings.DroppedParent = true
			}
			continue
		}
		if link.SourceID == item.ID {
			resp.Warnings.OutgoingLinks[link.LinkType]++
		}
		if link.TargetID == item.ID {
			resp.Warnings.IncomingLinks[link.LinkType]++
		}
	}

	// The legacy items.parent_id column, which the item_links scan above
	// cannot see. models.ItemCreate accepts `parent_id` on the create
	// route and CreateItem inserts it verbatim, so this is API-reachable
	// state, and CopyItemAcrossWorkspaces scrubs it along with the link
	// (Codex round 11). Same visibility filter as every other
	// relationship, plus an explicit workspace guard: the column's foreign
	// key constrains it to items(id) but says nothing about WHICH
	// workspace, and GetItem is id-global with no workspace predicate, so
	// a value pointing outside the source must not be honoured.
	if !resp.Warnings.DroppedParent && item.ParentID != nil && *item.ParentID != "" {
		legacyParent, pErr := s.store.GetItem(*item.ParentID)
		if pErr != nil {
			writeInternalError(w, pErr)
			return
		}
		if legacyParent != nil && legacyParent.WorkspaceID == sourceWorkspaceID && relVisible(legacyParent) {
			resp.Warnings.DroppedParent = true
		}
	}

	// ---- Attachment warnings (DR-11 / DR-11a / DR-16) -----------------
	//
	// The planner is shared with the mutating copy precisely so these
	// numbers ARE the numbers rather than a second implementation that
	// drifts. DryRun is what makes a plan legal without a destination
	// item id, and nothing here inserts the rows it returns.
	//
	// DR-11's ordering: refs are enumerated from the source CONTENT plus
	// the FINAL destination fields — never the source's raw fields, which
	// would count blobs referenced only by keys migration dropped and
	// overstate AttachmentBytes.
	plan, err := s.store.PlanAttachmentCopy(store.AttachmentCopyRequest{
		SourceWorkspaceID: item.WorkspaceID,
		TargetWorkspaceID: dst.WorkspaceID(),
		DryRun:            true,
		UploadedBy:        actorID,
		Content:           item.Content,
		Fields:            final,
	})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	resp.Warnings.AttachmentCount = len(plan.Rows)
	resp.Warnings.AttachmentBytes = plan.TotalBytes
	resp.Warnings.UnresolvableRefCount = len(plan.UnresolvableRefs)

	resp.Valid = len(resp.Fields.NeedsValue) == 0

	writeJSON(w, http.StatusOK, resp)
}

// sortedDroppedKeys orders MigrateFields' Dropped slice deterministically:
// source-schema order first (that is the order the user sees the fields in
// on the source item), then any orphan keys the source schema no longer
// declares, alphabetically. Duplicates are collapsed.
func sortedDroppedKeys(dropped []string, sourceFields []models.FieldDef) []string {
	rank := make(map[string]int, len(sourceFields))
	for i, f := range sourceFields {
		rank[f.Key] = i
	}
	seen := make(map[string]bool, len(dropped))
	out := make([]string, 0, len(dropped))
	for _, k := range dropped {
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, iOK := rank[out[i]]
		rj, jOK := rank[out[j]]
		switch {
		case iOK && jOK:
			return ri < rj
		case iOK != jOK:
			return iOK
		default:
			return out[i] < out[j]
		}
	})
	return out
}

// summarizeKeys / summarizeMessages render at most a handful of
// caller-supplied strings, each truncated, for an error message. The
// request body is capped at 2 MiB, which still leaves room for thousands
// of long keys or one enormous value — and validateFieldType quotes the
// offending value verbatim. Echoing either in full would turn a 400 into
// an amplifier.
func summarizeKeys(keys []string) string { return summarize(keys, 5, 64, ", ") }

func summarizeMessages(msgs []string) string { return summarize(msgs, 5, 200, "; ") }

func summarize(vals []string, maxVals, maxLen int, sep string) string {
	shown := vals
	if len(shown) > maxVals {
		shown = shown[:maxVals]
	}
	parts := make([]string, 0, len(shown))
	for _, v := range shown {
		// Truncate on a RUNE boundary: these are user-supplied strings and
		// a byte slice through a multi-byte character would emit invalid
		// UTF-8 into the JSON body.
		if len(v) > maxLen {
			runes := []rune(v)
			if len(runes) > maxLen {
				runes = runes[:maxLen]
			}
			v = string(runes) + "…"
		}
		parts = append(parts, v)
	}
	out := strings.Join(parts, sep)
	if len(vals) > len(shown) {
		out += fmt.Sprintf(" (and %d more)", len(vals)-len(shown))
	}
	return out
}

func fieldDefByKey(defs []models.FieldDef, key string) (models.FieldDef, bool) {
	for _, d := range defs {
		if d.Key == key {
			return d, true
		}
	}
	return models.FieldDef{}, false
}
