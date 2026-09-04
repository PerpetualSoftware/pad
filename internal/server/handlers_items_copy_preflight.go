package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

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
	//
	// Since BUG-2674 this bucket can also contain RESERVED system metadata
	// (implementation_notes, decision_log, github_pr, convention), appended
	// after the schema-ordered entries. Those keys are declared by no schema
	// anywhere — the copy carries them by identity — so a client must not
	// assume every `carried` entry resolves to a destination FieldDef. They
	// are distinguishable by `type: "system"` and carry a rendered `label`
	// rather than an author-supplied one. Before the carry-through they
	// appeared under `dropped`, which was accurate then; reporting them in
	// neither bucket would have made the preflight claim "nothing carries
	// over" for an item whose notes in fact survive.
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
// The values ItemCopyPreflightDropped.Reason can carry that this package
// originates. The relation reasons come from store.RelationIssueReasons()
// instead — the store owns that vocabulary because it decides those failures.
const (
	dropReasonNoTargetField         = "no_target_field"
	dropReasonIncompatibleType      = "incompatible_type"
	dropReasonUndeclaredSourceField = "undeclared_source_field"
	dropReasonAssigneeNotAMember    = "assignee_not_a_member"
	dropReasonAgentRoleNotPortable  = "agent_role_not_portable"
	dropReasonReferentNotPortable   = string(store.RelationTargetNotPortable)
)

// preflightDropReasons is the COMPLETE set of values this endpoint can put in
// a dropped entry's `reason`.
//
// It exists to be enumerated, not read: the web dialog renders each of these
// as a sentence and falls back to printing the raw enum, so a reason added
// without a case there reaches a user as `undeclared_source_field`. That is
// not hypothetical — `referent_not_portable` shipped in BUG-2674 and rendered
// raw until TASK-2878, which is when a rare reason became a routine one.
// TestCopyPreflightDropReasonsAreRenderedByTheDialog is the gate.
func preflightDropReasons() []string {
	out := []string{
		dropReasonNoTargetField,
		dropReasonIncompatibleType,
		dropReasonUndeclaredSourceField,
		dropReasonAssigneeNotAMember,
		dropReasonAgentRoleNotPortable,
	}
	seen := make(map[string]bool, len(out))
	for _, r := range out {
		seen[r] = true
	}
	for _, r := range store.RelationIssueReasons() {
		if !seen[string(r)] {
			out = append(out, string(r))
			seen[string(r)] = true
		}
	}
	return out
}

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
	//   "referent_not_portable"    — system metadata whose VALUE points at
	//                                something belonging to the SOURCE
	//                                workspace's context, so it describes
	//                                nothing true in the destination
	//                                (BUG-2674). Today that is github_pr:
	//                                the repository is a property of the
	//                                source's project, and carrying it
	//                                would render a live PR link on an
	//                                item whose project may have no
	//                                relationship to that repo. Distinct
	//                                from no_target_field, which would
	//                                otherwise be reported here and is
	//                                simply wrong: no schema declares this
	//                                key ANYWHERE, so "the destination has
	//                                no such field" is true of the source
	//                                too and explains nothing
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

	// RelationshipsPartial says the relationship counters above —
	// ChildCount, ChildrenOrphaned, DroppedParent, OutgoingLinks and
	// IncomingLinks — are a FLOOR rather than a total, because at least
	// one of the source's relationships is attached to an item this
	// caller may not see and was therefore not counted (TASK-2369).
	//
	// ONE EXCEPTION for a client rendering this: ChildrenOrphaned is
	// COMPLETE on a plain copy however much is hidden. A copy archives
	// nothing, so no child — visible or not — can be orphaned by it, and
	// `false` is the whole truth there. Qualify that one only when
	// ArchiveSource is true; the other four are qualified in both modes.
	//
	// The ACL filtering that causes this is correct and stays (see the
	// relationship block below). What is not acceptable is rendering
	// "none" and "unknown, some hidden from you" identically: a caller
	// with edit rights on the source but no visibility of its relatives
	// would otherwise read `children_orphaned: false` and run a MOVE
	// believing nothing is stranded, while hidden children are orphaned
	// in place. DR-17: "none of this may be silent."
	//
	// It is deliberately a BARE BOOLEAN. How many relationships are
	// hidden, of what type, and in which collection are exactly the facts
	// the ACL filter exists to withhold — a marker that varied with the
	// hidden count would reinstate the leak DR-10a, DR-10b and the
	// moved-to pointer each closed separately.
	// TestCopyPreflight_PartialMarkerDoesNotLeakHiddenCount pins that:
	// two workspaces differing only in how much is hidden must produce
	// byte-identical warning blocks.
	//
	// FALSE for an unrestricted caller, and false for a restricted caller
	// with nothing hidden. A marker on the common case is noise everyone
	// learns to ignore, which would make the product worse rather than
	// better.
	//
	// ACCEPTED DISCLOSURE, on the record. A conditional marker is by
	// construction one bit the caller did not previously have: "at least
	// one relationship exists that you may not see." /children, /links and
	// /items all return empty for such a caller either way, so this
	// endpoint is the only place that bit is available. It is the price of
	// the fix and it is worth paying — the alternative is telling someone
	// about to run a MOVE that nothing will be stranded when something
	// will. It carries no count, no type, no identity and no collection,
	// which is what keeps it one bit rather than a window. Pinned by
	// TestCopyPreflight_PartialMarkerForAnItemGrantOnlyGuest.
	RelationshipsPartial bool `json:"relationships_partial"`
}

// legacyHierarchyLinkTypes are hierarchy link types store.ChildLinkTypes
// does NOT walk. Today that is just "plan", the pre-rename alias that can
// still sit alongside a "parent" row on the same edge (see
// GetChildItemsTx's DISTINCT, which exists because of exactly that
// duplication).
//
// A LONE "plan" edge — one with no accompanying "parent" row — is
// invisible to GetChildItems, whose join is restricted to
// store.ChildLinkTypes. An earlier revision of this file left it counted
// as neither a child nor a dependency, on the reasoning that
// under-reporting a legacy duplicate is the safe direction.
//
// That reasoning does not survive the MOVE path (TASK-2369). Archiving
// the source makes it unavailable to a child on the far end of a lone
// "plan" edge just as surely as it does to one on a "parent" edge, and
// reporting `child_count: 0` / `children_orphaned: false` tells the user
// nothing is stranded when something is. So BOTH directions are now
// accounted for:
//
//   - OUTGOING (source_id == the item): the item's own parent edge.
//     Already reported as DroppedParent by the hierarchy branch below,
//     which keys off hierarchyLinkTypes and so has always included
//     "plan".
//   - INCOMING (target_id == the item): a CHILD. Folded into
//     countedChildren by the hierarchy branch below, deduplicated
//     against the two mechanisms GetChildItems and the parent_id column
//     already cover.
//
// It is still not reported as a DEPENDENCY, which would announce a
// blocker that does not exist.
//
// Not reachable through any current write path — models.ItemLinkType has
// no "plan" alias, so CreateItemLink rejects it — but it is reachable in
// rows written before the rename, which is exactly the population a
// legacy alias exists for.
var legacyHierarchyLinkTypes = []string{"plan"}

// storeChildLinkTypes is the store's own child-link set as a lookup —
// the complement of legacyHierarchyLinkTypes within hierarchyLinkTypes.
// A hierarchy link type NOT in here is one GetChildItems cannot see, and
// so is one this handler has to count for itself.
var storeChildLinkTypes = buildStoreChildLinkTypes()

func buildStoreChildLinkTypes() map[string]bool {
	out := map[string]bool{}
	for _, t := range store.ChildLinkTypes() {
		out[t] = true
	}
	return out
}

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
	// Resolution plus the four-step authorization ladder, DR-10a/DR-10b —
	// ONE implementation, shared with handleCopyItem, ordering included.
	// See resolveAuthorizedCopy for why each step sits where it does; the
	// dry run and the mutation must agree about the ORDER of refusals, not
	// merely the set, and the only way to guarantee that is to have one
	// copy of the sequence.
	ac, ok := s.resolveAuthorizedCopy(w, r)
	if !ok {
		return
	}
	sourceWorkspaceID := ac.sourceWorkspaceID
	item := ac.item
	input := ac.input
	src := ac.source
	dst := ac.destination
	targetColl := ac.targetCollection
	actorID := ac.actorID

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
	if bad := items.UndeclaredOverrideKeys(input.FieldOverrides, items.SchemaForMigratedFields(targetSchema).Fields); len(bad) > 0 {
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
	// `terminal_options` or `unique_scope`.
	//
	// The RELATION half of that is closed as of TASK-2878, and not where
	// this comment predicted. A same-named `relation` field used to carry a
	// SOURCE-workspace item id into the destination and be reported as a
	// clean carry, when across workspaces it is a dangling reference. The fix
	// did NOT go into MigrateFields: that function is in `internal/items`,
	// which is DB-free by construction, and deciding whether a string names a
	// live item in a particular collection is a database question. It went
	// into `store.MigrateRelationReferents`, called below by this endpoint and
	// by `migrateCopyFields` — one function, so the preview and the copy still
	// cannot disagree, which is what the original objection was actually
	// about. `computed`, `terminal_options` and `unique_scope` remain
	// unmodelled here.
	//
	// MigrateFields computes result.Errors before any override exists, so
	// those errors are stale the instant an override merges in. They are
	// deliberately never read here; the authoritative answer comes from
	// items.ValidateFieldsDetailed run over the MERGED map, which also
	// applies destination defaults and type/option/pattern checks.
	// Scope is COMPUTED, not assumed cross-workspace: this endpoint accepts a
	// target_workspace equal to the source, and hardcoding CrossWorkspace
	// would drop a github_pr from a duplicate whose repo context never
	// changed (BUG-2674). Same computation in the mutating copy, or the two
	// disagree — the divergence DR-6 exists to prevent.
	migrated := items.MigrateFields(currentFields, sourceSchema.Fields, targetSchema.Fields,
		items.ScopeFor(item.WorkspaceID, dst.WorkspaceID()))

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
	// Coerce strings to their declared types before validating (BUG-2850).
	// MUST match the store-side copy (items_cross_workspace_copy.go): the
	// preflight exists to PREDICT what the copy does, so a coercion on one
	// side only would make it report a field as failing that the copy accepts.
	final = items.CoerceFields(final, items.SchemaForMigratedFields(targetSchema))
	// Relation referents (TASK-2878), through the SAME store function the
	// mutating copy calls — which is the whole reason that function is in
	// `store` rather than beside either caller. This endpoint and
	// `migrateCopyFields` sit in different PACKAGES, and the note above the
	// CoerceFields call says in as many words that this is how the two drift
	// unnoticed; a preview that says "carried" while the copy drops is one
	// request answered two ways, the exact DR-6 divergence this pair exists to
	// prevent.
	//
	// POOL executor (`s.store.MigrateRelationReferents`, not the ...Q form):
	// the preflight is a read-only dry run holding no transaction. The copy
	// passes its tx for the reason recorded at that call site.
	//
	// Scope computed the same way MigrateFields was given it, a few lines
	// above — not re-derived, so the two cannot disagree about whether this
	// request crosses a boundary.
	relMode := store.RelationCarryWithinWorkspace
	if items.ScopeFor(item.WorkspaceID, dst.WorkspaceID()) == items.CrossWorkspace {
		relMode = store.RelationCarryCrossWorkspace
	}
	// Visibility on the supplied half, against the DESTINATION and the
	// caller's role THERE — dst.Role, never workspaceRole(r), which is the
	// source's. See refuseInvisibleRelationOverrides.
	if invisible, err := s.refuseInvisibleRelationOverrides(
		r, dst.WorkspaceID(), dst.Role, items.SchemaForMigratedFields(targetSchema),
		input.FieldOverrides); err != nil {
		writeInternalError(w, err)
		return
	} else if refuseRelationIssues(w, invisible) {
		return
	}
	relRefusals, relDropped, relErr := s.store.MigrateRelationReferents(
		dst.WorkspaceID(), items.SchemaForMigratedFields(targetSchema), final,
		input.FieldOverrides, currentFields, relMode)
	if relErr != nil {
		writeInternalError(w, fmt.Errorf("copy preflight: resolve relation referents: %w", relErr))
		return
	}
	// An override the caller typed that names nothing is REFUSED here, not
	// bucketed into needs_value — DR-12's disposition for an override with an
	// invalid value, which is the branch immediately below. Same 400
	// validation_error and the same sentence the copy returns, because both
	// render through store.RelationIssuesMessage.
	if refuseRelationIssues(w, relRefusals) {
		return
	}
	// Carried values that could not survive join `migrated.Dropped` rather
	// than a bucket of their own, so `StillDropped` filters them against the
	// final map exactly as it filters a type-mismatch drop — including the
	// case where the destination schema's own default re-populates the key,
	// which makes it a carry again and must not also be reported dropped.
	// `origin` loses its entry for the same reason: if a default does
	// re-populate the key, its origin is the destination's default, not the
	// source value that was just discarded.
	relationDropReason := make(map[string]string, len(relDropped))
	for _, ri := range relDropped {
		migrated.Dropped = append(migrated.Dropped, ri.Key)
		relationDropReason[ri.Key] = string(ri.Reason)
		delete(origin, ri.Key)
	}
	issues := items.ValidateFieldsDetailed(final, items.SchemaForMigratedFields(targetSchema))

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
	//
	// The STRIPPED schema, so a grandfathered reserved declaration is not
	// walked here and then appended again by the reserved pass below —
	// emitting the same key twice in one `carried` array (Codex round 4). The
	// existing preflight/copy parity helper collapses carried entries into a
	// map, so it could not see the duplicate: a check that de-duplicates
	// before comparing cannot detect duplication.
	for _, def := range items.SchemaForMigratedFields(targetSchema).Fields {
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

	// Reserved system metadata carries too (BUG-2674), and it is invisible to
	// the loop above because that walks the DESTINATION SCHEMA and these keys
	// are declared by no schema anywhere. Reporting them here keeps the
	// preflight honest in both directions: before the carry-through they at
	// least showed up under `dropped`, so omitting them now would turn an
	// accurate "these will be lost" into a silent "nothing carries over" while
	// the copy in fact retains them — a preflight that under-reports what it
	// will do is the same defect class as the move that reported nothing.
	//
	// Appended after the schema walk, in the enumeration's stable order, so
	// the destination-schema section of the response is untouched.
	for _, key := range models.ReservedItemFieldKeys() {
		val, present := final[key]
		if !present {
			continue
		}
		resp.Fields.Carried = append(resp.Fields.Carried, ItemCopyPreflightCarried{
			Key:   key,
			Label: reservedFieldLabel(key),
			Type:  "system",
			Value: val,
			From:  "migrated",
		})
	}

	// dropped — schema fields first, in SOURCE-schema order (MigrateFields
	// ranges a map, so its Dropped slice arrives in nondeterministic
	// order and must be re-sorted or repeated calls would differ).
	// Filtered against the FINAL map first (Codex round 2 P2-2): MigrateFields
	// computes Dropped before overrides merge and before defaults are
	// injected, so a key it lists may have been supplied since. Reporting a
	// field as dropped in the same response that reports it CARRIED — which is
	// what happened, the two buckets disagreeing about one key — is the
	// preflight lying to the dialog it exists to populate.
	for _, key := range sortedDroppedKeys(items.StillDropped(migrated.Dropped, final), sourceSchema.Fields) {
		reason := dropReasonNoTargetField
		label := key
		// A relation value whose referent did not survive (TASK-2878). The
		// reason is the resolver's own — `referent_not_portable` for a carried
		// value on a cross-workspace copy, or the specific lookup failure
		// within a workspace — because the generic no_target_field is simply
		// false here: the destination DOES declare the key, and reporting a
		// missing field would send the reader to fix a schema that is fine.
		if relReason, isRelation := relationDropReason[key]; isRelation {
			if def, exists := targetDefs[key]; exists && def.Label != "" {
				label = def.Label
			}
			resp.Fields.Dropped = append(resp.Fields.Dropped, ItemCopyPreflightDropped{
				Key: key, Label: label, Kind: "field", Reason: relReason,
			})
			continue
		}
		// A reserved key in Dropped can only have got there one way: it is
		// referential and this is a cross-workspace copy. The generic
		// no_target_field would be actively misleading — no schema declares
		// these keys anywhere, so it is equally true of the source and
		// explains nothing about why the value is being left behind.
		if models.IsReservedItemField(key) {
			resp.Fields.Dropped = append(resp.Fields.Dropped, ItemCopyPreflightDropped{
				Key: key, Label: reservedFieldLabel(key), Kind: "field", Reason: dropReasonReferentNotPortable,
			})
			continue
		}
		srcDef, declaredBySource := fieldDefByKey(sourceSchema.Fields, key)
		if def, exists := targetDefs[key]; exists {
			// The key exists downstream, so migration rejected the VALUE.
			// Which of the two ways matters to whoever reads this: a
			// genuine type mismatch, or a key the SOURCE schema never
			// declared — MigrateFields then has no source type to convert
			// from and drops the value unconditionally, even when the two
			// keys would otherwise have been compatible.
			reason = dropReasonIncompatibleType
			if !declaredBySource {
				reason = dropReasonUndeclaredSourceField
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
				Reason: dropReasonAssigneeNotAMember,
			})
		}
	}
	if item.AgentRoleID != nil && *item.AgentRoleID != "" {
		resp.Warnings.DroppedAgentRole = true
		resp.Fields.Dropped = append(resp.Fields.Dropped, ItemCopyPreflightDropped{
			Key: "agent_role", Label: "Agent role", Kind: "assignment",
			Reason: dropReasonAgentRoleNotPortable,
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
	//
	// What is NOT acceptable is leaving that trade invisible. Every point
	// below that drops a relationship for visibility reasons also sets
	// Warnings.RelationshipsPartial, so "none" and "none that you can see"
	// stop rendering identically (TASK-2369). The marker is a bare bool by
	// design — see its doc comment on ItemCopyPreflightWarnings.
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
			if countedChildren[set[i].ID] {
				continue
			}
			if !relVisible(&set[i]) {
				resp.Warnings.RelationshipsPartial = true
				continue
			}
			countedChildren[set[i].ID] = true
		}
	}

	links, err := s.store.GetItemLinks(item.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	for _, link := range links {
		// The far end of the edge — the item whose visibility decides
		// whether this relationship may be counted, and, for a lone
		// legacy child edge, the child itself.
		otherID := link.TargetID
		if otherID == item.ID {
			otherID = link.SourceID
		}
		// A lone legacy hierarchy edge pointing AT this item is a child
		// GetChildItems' join cannot see, so this handler has to resolve
		// and count it itself (see legacyHierarchyLinkTypes).
		//
		// COST: this is the one case that makes an UNRESTRICTED caller pay
		// for a per-link GetItem, which restricted callers already paid
		// (Codex round 4). Accepted rather than batched, because the
		// predicate is exactly "there is a legacy 'plan' row here": a
		// workspace with none — every workspace created since the rename,
		// since NormalizeItemLinkType has no 'plan' alias and no write path
		// can produce one — issues zero extra queries, and one that does
		// pays a bounded number of primary-key lookups in exchange for a
		// data-loss warning it was previously not given at all. Batching
		// would mean a new store method for a population that cannot grow.
		legacyChildEdge := hierarchyLinkTypes[link.LinkType] &&
			!storeChildLinkTypes[link.LinkType] &&
			link.TargetID == item.ID &&
			otherID != item.ID

		var other *models.Item
		if relVisibleIDs != nil || legacyChildEdge {
			other, err = s.store.GetItem(otherID)
			if err != nil {
				// DELIBERATE divergence from handleGetItemLinks, which
				// swallows this error and omits the link. Omitting a row
				// from a LIST is a partial answer; omitting it from a
				// COUNT is a wrong one, and the user is being asked to
				// agree to a loss on the strength of that number. Fail
				// loudly rather than under-report.
				writeInternalError(w, err)
				return
			}
		}
		if relVisibleIDs != nil {
			if other == nil {
				// The counterpart row is gone — GetItem excludes
				// soft-deleted items, and the row can also have vanished
				// between the two reads. Omit it unmarked, exactly as
				// /links does: nobody is losing a relationship that no
				// longer exists, so this is not a visibility gap.
				continue
			}
			if !relVisible(other) {
				resp.Warnings.RelationshipsPartial = true
				continue
			}
		}
		if hierarchyLinkTypes[link.LinkType] {
			if link.SourceID == item.ID {
				// The source's own parent edge — the copy is unparented.
				resp.Warnings.DroppedParent = true
			}
			// `other != nil` covers liveness (GetItem excludes
			// soft-deleted rows, so a dead counterpart is not a child).
			// The workspace guard is the same one the parent_id column
			// branch applies: item_links carries no workspace predicate
			// on the far end.
			if legacyChildEdge && other != nil && other.WorkspaceID == sourceWorkspaceID {
				countedChildren[other.ID] = true
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

	// Counted after the link scan, not before it: the scan is what folds
	// in lone legacy hierarchy edges (TASK-2369), and reading the totals
	// early would report those children as zero.
	resp.Warnings.ChildCount = len(countedChildren)
	resp.Warnings.ChildrenOrphaned = len(countedChildren) > 0 && input.ArchiveSource

	// The legacy items.parent_id column, which the item_links scan above
	// cannot see. models.ItemCreate accepts `parent_id` on the create
	// route and CreateItem inserts it verbatim, so this is API-reachable
	// state, and CopyItemAcrossWorkspaces scrubs it along with the link
	// (Codex round 11). Same visibility filter as every other
	// relationship, plus an explicit workspace guard: the column's foreign
	// key constrains it to items(id) but says nothing about WHICH
	// workspace, and GetItem is id-global with no workspace predicate, so
	// a value pointing outside the source must not be honoured.
	//
	// Evaluated even when DroppedParent is ALREADY true. The obvious
	// short-circuit ("we have already decided to warn, so skip the second
	// mechanism") is wrong once the marker exists: the two mechanisms can
	// name DIFFERENT items — SetParentLink does not touch items.parent_id,
	// so an item created with a hidden parent_id and later given a visible
	// `parent` link carries both — and skipping the column then leaves a
	// withheld relationship unmarked while dropped_parent reports the
	// visible one (Codex round 1).
	if item.ParentID != nil && *item.ParentID != "" {
		legacyParent, pErr := s.store.GetItem(*item.ParentID)
		if pErr != nil {
			writeInternalError(w, pErr)
			return
		}
		if legacyParent != nil && legacyParent.WorkspaceID == sourceWorkspaceID {
			if relVisible(legacyParent) {
				resp.Warnings.DroppedParent = true
			} else {
				// A real parent the copy will scrub, withheld because the
				// caller cannot see it. Not naming it is right; leaving
				// dropped_parent reading a flat "no" is not.
				resp.Warnings.RelationshipsPartial = true
			}
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

		// The caller's own visibility, applied to every reference the
		// planner resolves (TASK-2408). A reference the caller cannot see
		// lands in UnresolvableRefs, so it is reported here exactly as a
		// dangling or foreign id is — no count distinguishes them — and
		// the copy will not clone it, because it is the same authorizer.
		Authorize: ac.attachmentAuth,
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

// reservedFieldLabel renders a human label for a reserved metadata key in the
// copy preflight. These keys have no FieldDef and therefore no author-supplied
// Label, so the alternative is showing the raw snake_case key in a dialog whose
// every other row is titled prose.
//
// Kept adjacent to reservedFieldRemedy below: both are hand-maintained per-key
// tables over models.ReservedItemFieldKeys(), and putting them in one place is
// what stops a new key getting a label and no remedy. Both are covered by
// TestReservedFieldTablesAreExhaustive.
func reservedFieldLabel(key string) string {
	switch key {
	case models.ItemFieldImplementationNotes:
		return "Implementation notes"
	case models.ItemFieldDecisionLog:
		return "Decision log"
	case models.ItemFieldGitHubPR:
		return "Linked pull request"
	case models.ItemFieldConvention:
		return "Convention metadata"
	}
	return key
}

// reservedFieldRemedy names the write path that legitimately maintains a
// reserved metadata key, for the refusal message the field-patch gate emits
// (BUG-2627 part 2).
//
// It is per-key rather than one sentence because the remedies genuinely differ,
// and a message naming `pad item note` for a github_pr rejection would send the
// caller somewhere that cannot help them — the failure PATTE-135 exists to
// prevent. The empty string means "no user-facing write path", which the caller
// renders as a plain refusal rather than inventing a command; an unhelpful-but-
// true message beats a confident wrong one.
//
// Every remedy here was run against `--help` when it was written. Two keys
// deliberately have none, for opposite reasons:
//
//   - `convention` — its metadata is stamped at activation / create time
//     (models.BuildConventionItemFields), and a convention item's user-facing
//     trigger / scope / priority are ORDINARY schema fields, so `pad item
//     update --field trigger=always` is unaffected by this gate.
//   - `github_pr` — it never reaches this message at all. The patch door does
//     not refuse it (items.PatchRefusedFieldKeysIn exempts it, because on
//     remote MCP that door is its only writer), so naming `pad github link`
//     here would be prescribing a command for a refusal that cannot happen —
//     and prescribing it to the one audience that cannot run it.
func reservedFieldRemedy(key string) string {
	switch key {
	case models.ItemFieldImplementationNotes:
		return "`pad item note <ref> \"<summary>\"` (MCP: pad_item action=note)"
	case models.ItemFieldDecisionLog:
		return "`pad item decide <ref> \"<decision>\"` (MCP: pad_item action=decide)"
	}
	return ""
}

// appendBackedReservedKey reports whether a reserved key's remedy is an APPEND
// helper — the ones carrying BUG-2627 part 3's refuse-rather-than-destroy
// guard, and therefore the only ones whose remedy can itself refuse.
func appendBackedReservedKey(key string) bool {
	return key == models.ItemFieldImplementationNotes || key == models.ItemFieldDecisionLog
}

// reservedFieldPatchMessage renders the refusal the field-patch gate returns
// (BUG-2627 part 2). keys arrive sorted from items.ReservedFieldKeysIn, so the
// message is stable for a given input — a caller diffing two responses, or a
// test asserting on one, should not see the order move.
//
// currentFields is the item's stored blob, and it is here for one reason: a
// remedy has to work in the state the caller is actually in (PATTE-135). If the
// stored value for an append-backed key is ALREADY undecodable, `pad item note`
// refuses too — so naming it without qualification would send the caller in a
// circle, which is the failure that convention exists to prevent (Codex round
// 1). That case gets told the truth instead: nothing user-facing repairs it.
//
// The message carries the WHY as well as the what. "Not allowed" alone would
// read as arbitrary policy; the reason this door is closed is that the write it
// permits is silently destructive downstream, and a caller who knows that stops
// looking for a way around the gate. The destructive-downstream clause is
// stated ONLY for the append-backed keys: github_pr and convention are
// overwritten by a raw write, not made unreadable-then-unappendable, and
// claiming otherwise would be a confident wrong explanation.
func reservedFieldPatchMessage(keys []string, currentFields string) string {
	var b strings.Builder
	if len(keys) == 1 {
		fmt.Fprintf(&b, "%q is system metadata and cannot be set through a field update.", keys[0])
	} else {
		quoted := make([]string, 0, len(keys))
		for _, k := range keys {
			quoted = append(quoted, fmt.Sprintf("%q", k))
		}
		fmt.Fprintf(&b, "%s are system metadata and cannot be set through a field update.", strings.Join(quoted, ", "))
	}

	var anyAppendBacked, anyUnreadable bool
	for _, k := range keys {
		remedy := reservedFieldRemedy(k)
		if remedy == "" {
			continue
		}
		if appendBackedReservedKey(k) {
			anyAppendBacked = true
			if !models.StructuredFieldIsAppendable(currentFields, k) {
				anyUnreadable = true
				fmt.Fprintf(&b, " %s cannot be repaired from here: its stored value on this item"+
					" is already unreadable, so %s refuses as well (that refusal is what stops the"+
					" existing entries being overwritten).", k, remedy)
				continue
			}
		}
		fmt.Fprintf(&b, " Maintain %s with %s.", k, remedy)
	}

	if anyAppendBacked {
		b.WriteString(" A raw field write bypasses the writer that maintains it — and from the CLI or MCP" +
			" it also stores a value Pad cannot read back, because a `--field` value is typed by schema" +
			" lookup and these keys are in no schema: the entries go invisible on every surface, and the" +
			" append path then refuses on this item until the stored value is repaired (BUG-2627).")
	} else {
		b.WriteString(" A raw field write would overwrite what Pad's own writer maintains there," +
			" bypassing the checks that writer applies (BUG-2627).")
	}
	if anyUnreadable {
		b.WriteString(" Inspect the stored value with `pad item show <ref> --format json`;" +
			" repairing it takes a full `fields` write, which no CLI flag exposes today.")
	}
	return b.String()
}
