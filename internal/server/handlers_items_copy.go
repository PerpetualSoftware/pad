package server

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// Cross-workspace item COPY — PLAN-2357 / TASK-2365, implementing DR-9's
// re-check obligation, DR-13's no-retry rule and DR-14's fanout contract.
//
// ROUTE:
//
//	POST /api/v1/workspaces/{slug}/items/{itemSlug}/copy
//
// The URL's workspace is the SOURCE. The destination is named in the body.
// The dry-run sibling is POST …/items/{itemSlug}/copy/preflight
// (TASK-2364) and takes the IDENTICAL request shape — deliberately, so a
// dialog can send one body to both and a client can preview then commit
// without rebuilding it.
//
// REQUEST — itemCopyPreflightRequest, reused verbatim:
//
//	{
//	  "target_workspace":  "pad-web",              // required; slug or UUID
//	  "target_collection": "tasks",                // required; collection slug in the destination
//	  "field_overrides":   {"priority": "high"},   // optional; key MUST be declared by the destination schema
//	  "archive_source":    false                   // optional; the MOVE path (copy + archive source)
//	}
//
// Override semantics are the preflight's, exactly: an undeclared key is a
// 400, and a null value UNSETS the key (letting the destination schema's
// default re-apply) rather than persisting a literal JSON null. Both of
// those were live disagreements when TASK-2364 shipped; TASK-2365 moved the
// gate and the delete into Store.migrateCopyFields so the preview and the
// copy cannot answer differently. See TestCopyEndpoint_PreflightAndCopyAgree*.
//
// RESPONSE — 201 Created, ItemCopyResult. It carries everything a client
// needs to navigate to the copy (destination workspace slug + ref + slug)
// and, on a move, to navigate AWAY from the source (source.archived, plus
// its ref and workspace slug). `item` is the full destination item, so a
// dialog can render it without a follow-up GET into a workspace it may have
// no route mounted for.
//
// ERROR STATUSES — the preflight's set, plus what only a mutation can hit.
//
// SCOPED TO WHAT handleCopyItem ITSELF EMITS. The route sits inside the full
// middleware stack, so a client must also handle what every other
// /api/v1/workspaces/… route can return before the handler runs: 401
// unauthorized, 403 csrf_error, 403 for an unverified email, 404 for an
// unresolvable workspace slug, 429 rate_limited, and 500 internal_error
// (which this handler can also emit for a failed lookup). Those are not
// restated per-endpoint anywhere in Pad and are not restated here; the list
// below is the business vocabulary a copy client has to understand
// specifically (Codex round 14).
//
//	400 invalid_body            — the JSON body did not decode
//	400 missing_field           — target_workspace / target_collection absent
//	400 malformed_override      — an override names a field the destination
//	                              schema does not declare (store-side gate;
//	                              byte-identical intent to the preflight's)
//	400 validation_error        — the FINAL destination fields (after
//	                              migration, after overrides) fail the
//	                              destination schema. Covers both halves of
//	                              the preflight's split: a needs_value the
//	                              caller never resolved, and an override
//	                              whose VALUE is invalid. The preflight is
//	                              where those are told apart per field; a
//	                              mutation just refuses.
//	403 forbidden               — destination workspace not accessible
//	403 permission_denied       — destination workspace outside the bearer
//	                              token's consent allow-list
//	403 plan_limit_exceeded     — the destination workspace is at its
//	                              items_per_workspace cap (DR-16). Cloud
//	                              mode only; see the EnforceItemLimit note
//	                              below. Carries writePlanLimitError's usual
//	                              payload (plan, limit, current). That is
//	                              MORE than the preflight reveals — the
//	                              preflight documents quota as one of the
//	                              things `valid` deliberately does not
//	                              evaluate — and it is intended: it is
//	                              byte-identical to what handleCreateItem
//	                              already returns to any caller who may
//	                              create in that collection, which is exactly
//	                              the right this endpoint has already
//	                              established by check 4. Withholding it here
//	                              would hide an actionable answer behind a
//	                              distinction a POST to the same collection
//	                              erases anyway (Codex round 8, declined).
//	404 not_found               — source item absent or not visible, or the
//	                              caller may not edit it (WriteHidden; the
//	                              source side never distinguishes the two)
//	409 archived                — source exists, caller can see it, it is
//	                              archived
//	404 collection_not_found    — destination collection absent, archived,
//	                              foreign, or hidden from the caller. Also
//	                              covers a destination collection that was
//	                              still there when the caller was authorized
//	                              and gone by the time the copy locked it.
//	403 forbidden               — destination collection visible but the
//	                              caller may not create into it
//	403 actor_required          — nobody to attribute the copy to (no
//	                              authenticated user AND a source item with
//	                              no creator). Shared verbatim with the
//	                              preflight.
//	409 conflict                — a unique constraint in the destination
//	                              (slug, title, playbook invocation_slug)
//	409 cross_backend_attachments — the copy would have to move attachment
//	                              BYTES between storage backends, which v1
//	                              refuses (store.ErrCopyCrossBackendAttachments).
//	                              NOT REACHABLE TODAY: the handler leaves
//	                              TargetBackend empty, which disables the
//	                              store's cross-backend detection outright (see
//	                              the field's note below), and Pad registers
//	                              exactly one backend. The mapping is here so
//	                              that wiring a second backend is a one-line
//	                              change rather than a new 500; a client need
//	                              not handle it until then (Codex round 14).
//	500 copy_failed             — an AMBIGUOUS failure. See DR-13 below; the
//	                              message tells the user to check the
//	                              destination rather than retry.
//
// DR-13 — NO TRANSPARENT RETRY. There is no idempotency key in v1, so a
// retried copy is a duplicate, and a client that timed out after the commit
// cannot tell. This handler therefore calls the store op EXACTLY ONCE per
// request, whatever comes back: there is no retry loop, no backoff, and no
// "transient error" classification that could grow into one.
// TestCopyEndpoint_DoesNotRetryOnAmbiguousError pins the invocation count at
// one, so adding a retry breaks a test rather than shipping duplicates. The
// 500's message is part of the same contract — it tells the user to CHECK
// THE DESTINATION, because the copy may well have landed.
//
// The clients are clean today: internal/cli/client.go issues one
// httpClient.Do per request, and web/src/lib/api/client.ts retries GET and
// HEAD only, explicitly excluding POST.
//
// ⚠ READ THIS BEFORE PUTTING COPY ON THE MCP SURFACE. PLAN-2357 defers
// `pad_item.action: copy`, and whoever picks it up inherits a live conflict:
// internal/mcp/errors.go annotates EVERY 5xx with "Usually transient — retry
// once", which is the exact advice this endpoint's 500 exists to contradict.
// An agent that follows that hint duplicates the item. Routing copy through
// the MCP dispatcher therefore requires special-casing the `copy_failed` code
// in that classifier first — it is not a wiring-only change (Codex round 8).
//
// DR-9 — THE VERDICT IS RE-CHECKED IN-TRANSACTION. The four-check ladder runs
// at the top of the handler; inside the write transaction, against the
// snapshots re-read under the copy's locks, the copy asserts it is operating
// on the SAME resources that verdict was about
// (CrossWorkspaceCopyRequest.PreCheck / copyResourceInvariantPreCheck).
// AuthorizeCrossWorkspaceEdit says on both its entry points that its verdict
// is not atomic, and the window is real: between the handler's check and the
// lock, the source item can be moved into a collection hidden from the
// caller. See copyResourceInvariantPreCheck for exactly what that does and
// does not guarantee — it is deliberately a pure, I/O-free comparison.
//
// DR-14 — FANOUT IS POST-COMMIT, ASYMMETRIC, AND SEQ-ATTRIBUTED. See
// emitCopyFanout.
//
// COST AND RATE LIMITING. A copy holds BOTH workspaces' advisory locks for
// the length of its transaction, and that length scales with the number of
// attachment references in the item — each cloned row is an insert inside the
// lock. The endpoint inherits only the general authenticated limiter
// (600/min), which PLAN-2357 DR-16 explicitly adjudicates: "Rate limiting is
// a P2 and deliberately deferred… a dedicated lower limit on copy and its
// preflight is worth adding, but it is a hardening follow-on, not a
// correctness gate." Nothing here is unbounded — variants are server-derived,
// exactly two per original and idempotent (thumbnailSpecs), so an attacker
// cannot inflate the per-attachment cost — but the deferral is a decision on
// record rather than an oversight (Codex round 22, declined).
//
// CALLER OBLIGATIONS from CrossWorkspaceCopyResult, both honored here:
// storageInfoCache invalidation for the DESTINATION when attachments were
// copied, and EnforceItemLimit passed s.cloudMode rather than an
// unconditional true (passing true would apply free-tier item caps to
// self-hosted users whose plan row happens to say "free" — a limit that path
// has never had).

// ItemCopyResult is the 201 response.
//
// Shape note: the three blocks mirror the preflight's Source / Destination /
// ArchiveSource so a dialog that rendered the preview can render the outcome
// with the same accessors.
type ItemCopyResult struct {
	Source      ItemCopyResultSource      `json:"source"`
	Destination ItemCopyResultDestination `json:"destination"`

	// ArchiveSource echoes the request. It is what the request ASKED for;
	// Source.Archived is what actually happened. They agree on every
	// successful response — the field is here so a client holding only this
	// object can tell a copy from a move without inferring it.
	ArchiveSource bool `json:"archive_source"`

	// Item is the destination item, exactly as the copy committed it: its
	// Seq is workspace B's committed seq for this create, and its Ref /
	// Slug / CollectionSlug are the destination's.
	//
	// NOT enriched with relations (children, links, parent). A fresh copy
	// has none by construction — DR-17 scrubs the parent and copies no
	// links — so enrichment would be a round-trip that can only ever
	// return empty.
	Item *models.Item `json:"item"`

	Warnings ItemCopyResultWarnings `json:"warnings"`
}

// ItemCopyResultSource identifies what was copied, and whether it is still
// there. On a move a client uses this to navigate away from a source that
// no longer exists as a live item.
type ItemCopyResultSource struct {
	WorkspaceSlug  string `json:"workspace_slug"`
	CollectionSlug string `json:"collection_slug"`
	Ref            string `json:"ref,omitempty"`
	Slug           string `json:"slug"`
	Title          string `json:"title"`

	// Archived is true when this copy archived the source (the move path).
	// A plain copy leaves the source completely untouched.
	Archived bool `json:"archived"`

	// Seq is workspace A's committed seq for the ARCHIVE, and is present
	// only on a move — a plain copy does not write in A at all and must not
	// advance its cursor (DR-14). A client holding an A-side delta cursor
	// can use it to reconcile without a poll.
	Seq int64 `json:"seq,omitempty"`
}

// ItemCopyResultDestination is where the copy landed. Ref + WorkspaceSlug
// are the navigation pair.
type ItemCopyResultDestination struct {
	WorkspaceSlug  string `json:"workspace_slug"`
	WorkspaceName  string `json:"workspace_name"`
	CollectionSlug string `json:"collection_slug"`
	CollectionName string `json:"collection_name"`
	Ref            string `json:"ref,omitempty"`
	Slug           string `json:"slug"`

	// Seq is workspace B's committed seq for the create. Its own
	// workspace's seq, never A's (DR-14).
	Seq int64 `json:"seq,omitempty"`
}

// ItemCopyResultWarnings is the after-the-fact counterpart to the
// preflight's warning block: what the copy actually dropped and actually
// cloned, as opposed to what a preview predicted.
//
// It is deliberately NARROWER than ItemCopyPreflightWarnings. The
// relationship counters (child_count, dropped_parent, the link maps) are
// preview-only: they describe the SOURCE's relatives, which the copy did not
// touch and which the caller already saw before agreeing. Recomputing them
// post-commit would mean a second ACL-filtered pass over workspace A to
// restate a number the preflight already gave.
type ItemCopyResultWarnings struct {
	// DroppedFields are the destination-schema keys migration could not
	// carry. Always non-nil.
	DroppedFields []string `json:"dropped_fields"`

	// DroppedAssignee / DroppedAgentRole record the DR-8 scrubs.
	DroppedAssignee  bool `json:"dropped_assignee"`
	DroppedAgentRole bool `json:"dropped_agent_role"`

	// AttachmentCount / AttachmentBytes are what actually landed in the
	// destination workspace's storage.
	AttachmentCount int   `json:"attachment_count"`
	AttachmentBytes int64 `json:"attachment_bytes"`

	// UnresolvableRefCount is how many pad-attachment references resolved
	// to nothing under the source workspace's scope (DR-11a). Not cloned,
	// never fatal; the copy renders exactly as broken as the source did.
	UnresolvableRefCount int `json:"unresolvable_ref_count"`
}

// copyPreCheckDenial is an authorization refusal from the IN-TRANSACTION
// re-check. It records WHICH side refused so the handler can write the same
// response that side would have written at the top of the request — the
// disclosure posture must not change just because the denial arrived later.
type copyPreCheckDenial struct {
	// side is "source" or "destination".
	side   string
	access CrossWorkspaceAccess
}

func (e *copyPreCheckDenial) Error() string {
	return fmt.Sprintf("cross-workspace copy pre-check denied on the %s side: %s", e.side, e.access.Reason)
}

// handleCopyItem copies one item into another workspace, optionally
// archiving the source. See the file header for the full contract.
func (s *Server) handleCopyItem(w http.ResponseWriter, r *http.Request) {
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
		// ResolveItem filters soft-deleted rows, so this covers "absent" AND
		// "archived"; writeItemResolveError separates them exactly as the
		// preflight, move and update paths do.
		s.writeItemResolveError(w, r, sourceWorkspaceID, itemSlug)
		return
	}

	// ---- Authorization, DR-10a/DR-10b, four checks in order --------------
	//
	// Identical to the preflight's, statement for statement, including the
	// early return between the source and destination halves: a destination
	// verdict built for a caller who could not read the source is itself a
	// disclosure. Diverging here would let a caller preview a copy they
	// cannot perform, or worse, perform one they cannot preview.
	src := s.AuthorizeCrossWorkspaceEdit(r, sourceWorkspaceID, CrossWorkspaceItemScope(item))
	if !src.Allowed {
		src.WriteHidden(w, "Item")
		return
	}

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

	dstWS := s.AuthorizeCrossWorkspaceEdit(r, input.TargetWorkspace, CrossWorkspaceWorkspaceOnlyScope())
	if !dstWS.Allowed {
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

	dst := s.AuthorizeCrossWorkspaceEdit(r, input.TargetWorkspace, CrossWorkspaceCollectionScope(targetColl.ID))
	if !dst.Allowed {
		dst.WriteCollectionNotFound(w)
		return
	}

	// Refused HERE rather than left to the store, which answers a bare
	// "actor is required" that would be mapped to the ambiguous 500 and send
	// the user hunting for an item nothing ever tried to create. The
	// preflight applies the identical guard, so the preview cannot promise a
	// copy this would refuse (Codex round 4).
	actorID := copyActorID(r, item)
	if actorID == "" {
		writeCopyActorRequired(w)
		return
	}

	actor, actorSource := actorFromRequest(r)

	// ---- The copy. ONE call, no retry (DR-13). ---------------------------
	res, err := s.copyItemAcrossWorkspaces(store.CrossWorkspaceCopyRequest{
		SourceItemID:       item.ID,
		TargetWorkspaceID:  dst.WorkspaceID(),
		TargetCollectionID: targetColl.ID,
		FieldOverrides:     input.FieldOverrides,
		Actor:              actorID,
		CreatedBy:          actor,
		Source:             actorSource,
		ArchiveSource:      input.ArchiveSource,

		// TargetBackend is deliberately LEFT EMPTY, which disables
		// cross-backend detection. Pad registers exactly one attachment
		// backend today (attachments.FSPrefix, wired in cmd/pad), and — the
		// binding reason — the preflight leaves it empty too. Setting it on
		// only one of the two would let the copy refuse a request its own
		// preview accepted, which is the DR-6 disagreement this pair of
		// endpoints exists to prevent. When a second backend lands, both
		// call sites get it together.
		TargetBackend: "",

		// s.cloudMode, NEVER an unconditional true: EnforceItemLimit mirrors
		// enforcePlanLimit's `if !s.cloudMode { return true }` gate, and
		// forcing it on would apply free-tier item caps to self-hosted users
		// whose plan row says "free".
		EnforceItemLimit: s.cloudMode,

		// DR-9's in-tx re-check, against the under-lock snapshots.
		PreCheck: copyResourceInvariantPreCheck(copyAuthorizedResources{
			sourceItemID:       item.ID,
			sourceWorkspaceID:  sourceWorkspaceID,
			sourceCollectionID: item.CollectionID,
			targetCollectionID: targetColl.ID,
			targetWorkspaceID:  dst.WorkspaceID(),
			source:             src,
			destination:        dst,
		}),
	})
	if err != nil {
		s.writeCopyError(w, err)
		return
	}

	// ---- Post-commit, in this order. -------------------------------------
	//
	// The copy is committed and IRREVERSIBLE from here. Nothing below may
	// turn into a non-2xx response: a client that sees a failure after a
	// successful copy is exactly the ambiguity DR-13 forbids resolving by
	// retrying, and there is nothing here worth telling the user the copy
	// failed over.

	// EVERY collection fact below comes from res, not from the pre-transaction
	// reads above. The store re-reads both collection rows under a FOR UPDATE
	// pin and hands them back for exactly this reason: between the
	// authorization lookup and the lock, the source item can be moved into a
	// different collection and either collection can be renamed. Reporting the
	// pre-transaction slug would attribute the source's archive event to a
	// collection the copy did not touch and hand the client a stale slug
	// (Codex round 1 P2).
	s.afterCopyCommit(r, res)

	resp := ItemCopyResult{
		Source: ItemCopyResultSource{
			// The CANONICAL slug, never the URL parameter — /workspaces/{slug}
			// also accepts a UUID, and a client building a "go back to the
			// source" link out of that gets one that does not resolve (Codex
			// round 14).
			WorkspaceSlug:  src.WorkspaceSlug(),
			CollectionSlug: res.SourceCollection.Slug,
			Ref:            res.Source.Ref,
			Slug:           res.Source.Slug,
			Title:          res.Source.Title,
			Archived:       res.SourceSeq != nil,
		},
		Destination: ItemCopyResultDestination{
			WorkspaceSlug:  dst.WorkspaceSlug(),
			WorkspaceName:  dst.Workspace.Name,
			CollectionSlug: res.TargetCollection.Slug,
			CollectionName: res.TargetCollection.Name,
			Ref:            res.Item.Ref,
			Slug:           res.Item.Slug,
			Seq:            res.Item.Seq,
		},
		ArchiveSource: input.ArchiveSource,
		Item:          res.Item,
		Warnings: ItemCopyResultWarnings{
			DroppedFields:        nonNilStrings(res.DroppedFields),
			DroppedAssignee:      res.DroppedAssignee,
			DroppedAgentRole:     res.DroppedAgentRole,
			AttachmentCount:      res.AttachmentsCopied,
			AttachmentBytes:      res.BytesCopied,
			UnresolvableRefCount: len(res.UnresolvableRefs),
		},
	}
	if res.SourceSeq != nil {
		resp.Source.Seq = *res.SourceSeq
	}

	writeJSON(w, http.StatusCreated, resp)
}

// copyItemAcrossWorkspaces calls the store op exactly once.
//
// The indirection exists ONLY so a test can count invocations and prove
// DR-13's no-retry guarantee — see copyItemFn. Production always takes the
// nil branch. It is deliberately a plain call with no error classification:
// the moment a "should we retry this?" branch appears here, the guarantee is
// gone and no test outside this package would notice.
func (s *Server) copyItemAcrossWorkspaces(req store.CrossWorkspaceCopyRequest) (*store.CrossWorkspaceCopyResult, error) {
	if s.copyItemFn != nil {
		return s.copyItemFn(req)
	}
	return s.store.CopyItemAcrossWorkspaces(req)
}

// copyResourceInvariantPreCheck builds the DR-9 in-transaction re-check.
//
// WHAT IT CHECKS. Identity, and nothing else: that the resources the store
// re-read UNDER ITS LOCKS are the same resources the handler's four-check
// ladder was run against. The source item is still the one that was
// authorized, still in that workspace and — critically — still in the
// COLLECTION it was authorized in; the destination collection is still the
// one that was authorized, still in the destination workspace.
//
// WHAT IT CLOSES. The window TASK-2358 documents on both
// AuthorizeCrossWorkspaceEdit entry points ("NOT ATOMIC … a MUTATING caller
// must re-read both sides and re-apply the check"). Concretely: between the
// handler's verdict and the copy's locks, the source item can be MOVED into a
// different collection. Without this the copy reads that item under the lock
// and copies it out on the strength of a verdict about a collection it is no
// longer in — an item the caller may have no right to see at all, landing in
// a workspace they control. That is the exfiltration DR-10b is about, and it
// is the one failure mode in this family that is both genuinely reachable and
// genuinely damaging.
//
// It refuses on ANY collection change, not only a move into a hidden one.
// Deliberately conservative, and it pays a second dividend: the migration
// would otherwise run against a source schema the preflight never showed the
// user, which is a DR-6 disagreement of its own.
//
// WHAT IT DOES NOT CHECK, and why — an honest boundary is worth more than a
// broad-sounding one. It does NOT re-read membership or grants. An earlier
// version re-ran the whole ladder here, and that was wrong on two counts
// (Codex round 6):
//
//   - It OVERCLAIMED. The authorize helpers read through s.store, not this
//     transaction, and Postgres READ COMMITTED takes a fresh snapshot per
//     statement — so the "re-check" judged locked resources against
//     authorization state read at several unsynchronised moments. And
//     membership can be revoked and committed after those reads and before
//     this transaction commits regardless, because membership writers do not
//     participate in the copy's advisory locks. Narrowing that window from
//     "since the handler ran" to "since the lock was taken" is arbitrary; it
//     is not a boundary.
//   - It was a LIVENESS HAZARD. Those reads need a pool connection while this
//     transaction already holds one. SQLite's pool is capped at 16
//     (sqliteMaxOpenConns): sixteen concurrent copies means one transaction
//     holding the BEGIN IMMEDIATE write lock and fifteen parked inside
//     db.Begin(), so the active transaction's re-check finds no free
//     connection and waits — for transactions that cannot proceed until it
//     commits. An application-level deadlock that unwinds only when a waiter
//     hits the 30-second busy timeout, on the security-critical path of the
//     one operation that holds TWO workspaces' locks. The pure comparison
//     below performs NO I/O at all and cannot do that.
//
// THE CONSEQUENCE, so this is not read as an oversight: a membership, grant
// or token-consent revocation that commits while a copy is in flight does NOT
// stop that copy. Codex round 22 raised this as an attack; it is declined on
// three grounds. Re-reading here would only NARROW the window from "since the
// handler ran" to "since the lock was taken", never close it — closing it
// needs every membership writer to take the copy's workspace advisory locks,
// a workspace-wide change this task has no mandate for. No other write path
// in Pad closes it either: handleCreateItem, handleUpdateItem,
// handleDeleteItem and handleMoveItem all authorize before their store call
// and none re-checks inside, so a copy that tried would be inconsistent with
// the product rather than safer than it. And the attempt costs a real
// liveness hazard, described below. The honest statement is that
// authorization is evaluated once, at the start of a request measured in
// milliseconds.
//
// The comparisons cannot fail spuriously: every value is an immutable
// identifier the handler read moments earlier.
func copyResourceInvariantPreCheck(authorized copyAuthorizedResources) func(*sql.Tx, *models.Item, *models.Collection) error {
	return func(_ *sql.Tx, source *models.Item, targetColl *models.Collection) error {
		if source == nil || source.ID != authorized.sourceItemID ||
			source.WorkspaceID != authorized.sourceWorkspaceID ||
			source.CollectionID != authorized.sourceCollectionID {
			// Returned bare: CopyItemAcrossWorkspaces wraps it in
			// store.CopyPreCheckError so the rollback is classified as a
			// caller-facing rejection, and writeCopyError recovers this type
			// through that wrapper with errors.As.
			return &copyPreCheckDenial{side: "source", access: authorized.source}
		}
		if targetColl == nil || targetColl.ID != authorized.targetCollectionID ||
			targetColl.WorkspaceID != authorized.targetWorkspaceID {
			return &copyPreCheckDenial{side: "destination", access: authorized.destination}
		}
		return nil
	}
}

// copyAuthorizedResources records exactly what the handler's four-check
// ladder was run against, so the in-transaction re-check can assert the copy
// is operating on the same things.
//
// The two CrossWorkspaceAccess values are the ALLOWED verdicts, carried only
// so a refusal can be written through the same disclosure-preserving writer
// the handler itself would have used. Their Allowed flag is never inspected;
// by construction it is true.
type copyAuthorizedResources struct {
	sourceItemID       string
	sourceWorkspaceID  string
	sourceCollectionID string
	targetCollectionID string
	targetWorkspaceID  string

	source      CrossWorkspaceAccess
	destination CrossWorkspaceAccess
}

// afterCopyCommit performs every post-commit side effect the copy owes, under
// ONE panic boundary.
//
// The boundary is the point. At the moment this runs the transaction has
// committed and the copy is irreversible, but the 201 has not been written
// yet — so a panic escaping into chi's recoverer would turn a SUCCEEDED copy
// into a 500. That is precisely the ambiguous outcome DR-13 forbids a client
// from resolving by retrying, and the retry would duplicate the item. None of
// the work below is worth telling the user their copy failed over: a missed
// event is reconciled by the next delta poll, and a stale storage figure
// expires in thirty seconds.
//
// The cache invalidation is INSIDE the boundary rather than beside it (Codex
// round 3): storageInfoCache is a mutex-guarded map, so a panic there is not
// something to reason about case by case — it is simply another way to lose a
// committed copy's response, and it belongs under the same guard as the
// fanout.
//
// The recover handler itself reads only pre-captured strings. Dereferencing
// res inside a deferred recover would let a malformed result panic AGAIN
// while recovering, which is unrecoverable and takes the process with it.
func (s *Server) afterCopyCommit(r *http.Request, res *store.CrossWorkspaceCopyResult) {
	// Captured eagerly so the deferred handler can never dereference res.
	var sourceWorkspaceID, targetWorkspaceID, targetItemID string
	if res != nil {
		sourceWorkspaceID = res.SourceWorkspaceID
		if res.Item != nil {
			targetWorkspaceID = res.Item.WorkspaceID
			targetItemID = res.Item.ID
		}
	}
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("cross-workspace copy post-commit work panicked; the copy IS committed and the response stands",
				"source_workspace_id", sourceWorkspaceID,
				"target_workspace_id", targetWorkspaceID,
				"target_item_id", targetItemID,
				"panic", rec,
				"stack", string(debug.Stack()))
		}
	}()

	// CALLER OBLIGATION from CrossWorkspaceCopyResult. Workspace B's storage
	// usage is memoized for 30 seconds and the store has no handle on the
	// cache, so without this the destination's storage page reports stale
	// usage for the rest of the window — right after the user watched the
	// bytes land. Only the DESTINATION: the copy adds rows in B and changes
	// nothing in A, even on a move (soft-delete never removes bytes).
	if res.AttachmentsCopied > 0 {
		s.storageInfoCache.invalidate(targetWorkspaceID)
	}

	s.emitCopyFanout(r, res)
}

// emitCopyFanout is DR-14's emission contract, in full. It is the whole of
// what a copy publishes, and the asymmetry is the specification:
//
//   - ALWAYS, in the DESTINATION: activity `created`, SSE ItemCreated,
//     webhook `item.created`.
//   - ONLY on archive_source, in the SOURCE: activity `archived`, SSE
//     ItemArchived, webhook `item.deleted`.
//   - A PLAIN COPY EMITS NOTHING AT ALL IN THE SOURCE. Not a "copied"
//     activity row, not an ItemUpdated. The source row is untouched by a
//     plain copy — its seq does not move — so any event there would tell A's
//     watchers something changed when nothing did, and would desynchronise
//     a delta cursor that is legitimately still valid.
//   - Every event carries ITS OWN workspace's committed seq. B's create
//     event carries res.Item.Seq; A's archive event carries *res.SourceSeq,
//     the value the archive assigned under A's seq lock. Crossing them would
//     hand each workspace's clients a cursor from the other's sequence
//     space, which is not merely wrong but silently wrong: the numbers are
//     plausible.
//
// POST-COMMIT, and only post-commit. Emitting inside the transaction leaks
// an event for a copy that then rolls back — the existing move path already
// treats activity logging as best-effort post-commit for the same reason.
// The corollary is that a rolled-back copy emits nothing anywhere, which
// falls out of this being called only on the success path.
//
// THE ONE GAP, stated because it is inherent rather than overlooked (Codex
// round 20): if tx.Commit() itself errors, the store returns no result, so
// nothing here runs — and on Postgres a commit whose acknowledgement is lost
// may nonetheless have committed. That is DR-13's ambiguity in its purest
// form: a copy that landed with no events. It cannot be closed from here —
// database/sql reports commit failure as an ordinary error with no
// "maybe-committed" signal, and the seq needed to emit anything is only
// readable inside the transaction that failed. It is bounded rather than
// silent: delta sync is a cursor walk over items.seq, not an event log, so
// the destination's next /items-changes poll surfaces the item regardless,
// and the endpoint's 500 tells the user to go and look.
//
// SYNCHRONOUS, deliberately. logActivity writes a row, the SSE publish can
// reach Redis, and dispatchWebhook lists and marshals before it spawns its
// delivery goroutines — so this runs before the 201 and can add latency to
// it. That is exactly what handleCreateItem, handleDeleteItem and
// handleMoveItem already do (Codex round 3), and diverging would buy nothing:
// moving it to a goroutine would make the emission ordering — which DR-14
// specifies and these tests assert — unobservable, and would decouple the
// "copy succeeded" response from the events that describe it for no
// correctness gain. If fanout latency ever becomes a problem it is a
// workspace-wide change to the three primitives, not a special case here.
//
// COLLECTION SLUGS COME FROM res, the under-lock snapshots — never from the
// handler's pre-transaction reads. An event carrying the collection the item
// used to be in is a quiet lie to every SSE consumer that routes on it.
//
// It is called only from afterCopyCommit, which owns the panic containment
// for this and for the cache invalidation alike.
func (s *Server) emitCopyFanout(r *http.Request, res *store.CrossWorkspaceCopyResult) {
	actor, actorSource := actorFromRequest(r)
	actorName := actorNameFromRequest(r)

	// --- Destination: always all three. ---
	targetWorkspaceID := res.Item.WorkspaceID
	s.logActivity(targetWorkspaceID, res.Item.ID, "created", r)
	s.publishItemEventWithName(events.ItemCreated, targetWorkspaceID, res.Item.ID, res.Item.Title,
		res.TargetCollection.Slug, actor, actorName, actorSource, res.Item.Seq)
	s.dispatchWebhook(targetWorkspaceID, "item.created", res.Item)

	// --- Source: only on a move, and SourceSeq is the discriminator. It is
	// non-nil if and only if the archive ran, so this cannot emit an archive
	// event for a copy that did not archive, even if a future caller passes
	// ArchiveSource inconsistently. ---
	if res.SourceSeq == nil {
		return
	}
	s.logActivity(res.SourceWorkspaceID, res.Source.ID, "archived", r)
	s.publishItemEventWithName(events.ItemArchived, res.SourceWorkspaceID, res.Source.ID, res.Source.Title,
		res.SourceCollection.Slug, actor, actorName, actorSource, *res.SourceSeq)
	s.dispatchWebhook(res.SourceWorkspaceID, "item.deleted", res.Source)
}

// writeCopyError maps the store's typed errors onto the statuses the
// contract promises. Everything the store classifies as a caller-facing
// rejection has an entry; the default is the DR-13 ambiguity message.
func (s *Server) writeCopyError(w http.ResponseWriter, err error) {
	// The in-tx authorization re-check, first: it must produce the SAME
	// response the same denial would have produced at the top of the
	// request. A caller whose grant was revoked mid-copy learns exactly what
	// they would have learned a millisecond earlier, and no more.
	var denial *copyPreCheckDenial
	if errors.As(err, &denial) {
		if denial.side == "source" {
			denial.access.WriteHidden(w, "Item")
		} else {
			denial.access.WriteCollectionNotFound(w)
		}
		return
	}

	var limit *store.ItemLimitError
	if errors.As(err, &limit) {
		writePlanLimitError(w, limit.Result)
		return
	}

	var undeclared *store.UndeclaredOverrideError
	if errors.As(err, &undeclared) {
		// Same code the preflight emits, and bounded the same way: the keys
		// are caller-supplied and validateFieldType-style verbatim echoing
		// turns a 400 into an amplifier.
		writeError(w, http.StatusBadRequest, "malformed_override",
			"Destination collection has no field(s): "+summarizeKeys(undeclared.Keys))
		return
	}

	var validation *store.FieldValidationError
	if errors.As(err, &validation) {
		writeError(w, http.StatusBadRequest, "validation_error",
			summarizeMessages([]string{validation.Err.Error()}))
		return
	}

	if errors.Is(err, store.ErrCopyCrossBackendAttachments) {
		writeError(w, http.StatusConflict, "cross_backend_attachments",
			"This item's attachments are stored in a different storage backend than the destination workspace. Cross-backend copy is not supported.")
		return
	}

	// A collection that vanished between the authorization lookup and the
	// copy's lock. Both are PRE-WRITE rejections — nothing was inserted and
	// nothing can have committed — so neither may fall through to the
	// ambiguous 500, which would send the user hunting for an item that
	// provably does not exist (Codex round 1 P2). Each side keeps its own
	// disclosure posture: the source collapses to the bare "Item not found"
	// every other source-side refusal gives, and the destination reuses the
	// one collection_not_found answer shared with "absent" and "hidden".
	if errors.Is(err, store.ErrCopySourceCollectionMissing) {
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return
	}
	if errors.Is(err, store.ErrCopyTargetCollectionMissing) {
		writeError(w, http.StatusNotFound,
			crossWorkspaceCollectionNotFoundCode, crossWorkspaceCollectionNotFoundMessage)
		return
	}

	if errors.Is(err, sql.ErrNoRows) {
		// The source vanished or was archived between the authorization
		// check and the copy's lock. Same 404 the source side gives for
		// everything else — the source never distinguishes absence from
		// forbidden-ness.
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return
	}

	if store.IsUniqueViolation(err) {
		// The destination's slug-per-workspace constraint or the playbook
		// invocation_slug partial unique index. Kept generic for the same
		// reason createItemChecked keeps it generic — it covers both, and
		// naming which one would report on rows the caller may not see.
		//
		// store.IsUniqueViolation rather than a local string match, so this
		// and CreateItem's 409 cannot drift apart (Codex round 8). The
		// heuristic is a text match and CAN in principle misfire on an
		// unrelated error whose message happens to contain the phrase — which
		// is harmless here, and worth saying why: EVERY error out of
		// CopyItemAcrossWorkspaces means the transaction did not commit (a
		// failed Commit rolls back, and nothing after a successful Commit can
		// fail). So a misclassified error is 409 instead of 500 on a request
		// that provably wrote nothing either way. The one genuinely ambiguous
		// case — a commit whose outcome the client never learns because the
		// connection died — does not carry this text.
		writeError(w, http.StatusConflict, "conflict",
			"The copy conflicts with an existing record in the destination workspace (duplicate slug, title, or invocation slug)")
		return
	}

	// DR-13's ambiguous case. The copy MAY have committed — a failure can
	// land on the commit itself — so the message says "check", never "retry".
	// Deliberately not writeInternalError: its generic text would invite the
	// exact retry that duplicates the item.
	slog.Error("cross-workspace item copy failed", "error", err)
	writeError(w, http.StatusInternalServerError, "copy_failed",
		"The copy did not complete. It may or may not have landed — check the destination workspace before trying again.")
}

// copyActorID resolves the identity a copy is attributed to: every cloned
// attachment's uploaded_by and the provenance row's created_by. Returns ""
// when there is nobody to attribute it to.
//
// SHARED WITH THE PREFLIGHT, and that is the point. The store REQUIRES a
// non-empty actor, and on a fresh install (no users yet, everything open
// until `pad auth setup`) there is no current user at all — so the fallback
// is the source item's creator, which CreateItem defaults to "user" on every
// write path. The preflight originally had its own version of this with an
// extra `"preflight"` literal at the end, which was harmless there (it hands
// the value to a planner that writes nothing) but made the preview accept a
// request the copy would refuse — a THIRD divergence of exactly the kind
// DR-6 exists to prevent, found in Codex round 4. One function, one answer;
// both endpoints refuse identically via writeCopyActorRequired.
func copyActorID(r *http.Request, item *models.Item) string {
	if id := currentUserID(r); id != "" {
		return id
	}
	return item.CreatedBy
}

// writeCopyActorRequired is the shared refusal for an unattributable copy.
//
// Both endpoints emit it, byte for byte, so the preview and the mutation
// agree. It is deliberately NOT routed through the copy's ambiguous-outcome
// 500: this is decided before anything is attempted, so telling the user to
// go check the destination would be false.
func writeCopyActorRequired(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, "actor_required",
		"A copy must be attributed to a user. Sign in, or run `pad auth setup` to create the first account.")
}

// nonNilStrings guarantees `[]` rather than `null` on the wire, matching the
// preflight's promise for every list it returns.
func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
