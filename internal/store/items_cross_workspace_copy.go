package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/items"
	"github.com/PerpetualSoftware/pad/internal/kernelevents"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// Cross-workspace item copy — PLAN-2357 DR-9 / DR-9a / DR-11 / DR-12 / DR-14 /
// DR-16 / DR-17.
//
// CopyItemAcrossWorkspaces is the one store operation that lands an item from
// workspace A into workspace B. It cannot be assembled from existing
// primitives: CreateItem, CreateAttachment and DeleteItem each open and commit
// their own transaction, so composing them would leave a window in which the
// destination item exists without its attachments, or the source is archived
// with no destination to point at. Everything below runs in ONE transaction.
//
// AUTHORIZATION IS NOT HERE. This is a store primitive; it enforces data
// invariants (scope, quota, seq, provenance) and nothing else. The four-step
// visibility/edit ladder of DR-10a / DR-10b — source item visible, source edit,
// destination collection visible, destination collection edit — belongs to the
// HTTP layer (TASK-2358 / TASK-2365) and MUST run before this is called. A
// caller that skips it has built an exfiltration path. The one concession is
// CrossWorkspaceCopyRequest.PreCheck: a hook that lets the HTTP layer re-check
// its verdict against the UNDER-LOCK snapshots (DR-9), still without this
// package knowing what a permission is. Read that field's doc before assuming
// what it guarantees — the production hook compares resource IDENTITY and does
// not re-evaluate grants.
//
// FIELD SEMANTICS ARE SHARED WITH THE PREFLIGHT (DR-6). migrateCopyFields and
// handleCopyItemPreflight must agree on what a copy would persist, key for key
// — the preview lying about the copy is the failure the shared-endpoint design
// exists to prevent. TASK-2364 shipped with two known disagreements in the
// override merge (null overrides, undeclared overrides); TASK-2365 closed both
// on the preflight's side. See migrateCopyFields.
//
// FANOUT IS NOT HERE EITHER (DR-14). No activity row, no SSE publish, no
// webhook. Emitting inside the transaction would leak an event for a copy that
// then rolls back. The caller emits post-commit from CrossWorkspaceCopyResult.
//
// NARROWED BY TASK-2658, deliberately and without reversing DR-14's reasoning.
// The transactional event OUTBOX is written here — by createItemTx for the
// destination item, and by archiveItemForCopyTx for a move's source archive.
// That is not fanout: it is the durable record that the mutation happened, and
// it is exempt from DR-14's rationale rather than an exception to it. DR-14
// refuses in-transaction emission because a rollback would leak an event; an
// outbox row written on the SAME transaction rolls back WITH the copy, so the
// leak DR-14 names cannot occur. The three things DR-14 actually lists —
// activity row, SSE publish, webhook — still happen post-commit at the caller,
// unchanged.

// ErrCopyCrossBackendAttachments is returned when the copy would have to move
// attachment BYTES between storage backends.
//
// v1 refuses rather than transferring. Two reasons, both structural:
//
//   - The store has no handle on AttachmentStore. Blob backends are wired at
//     the server layer and store-level code deliberately never touches the
//     filesystem or object storage (see CreateAttachment's doc). Threading a
//     backend registry into the store to serve one branch of one operation
//     would invert that boundary for every other caller too.
//   - Even with a handle, the Get/Put would run while this transaction holds
//     BOTH workspaces' advisory locks. Every writer in both workspaces would
//     block behind an unbounded network round-trip per attachment, and a
//     partially-transferred set has no rollback (Put is not transactional).
//     Doing it outside the transaction reintroduces exactly the atomicity hole
//     DR-9 exists to close.
//
// Same-instance copies — the only shape that exists today, where source and
// destination resolve through the same backend — are unaffected: storage is
// content-addressed, so the clone is a row copy and NeedsByteTransfer is
// false for every row. Callers signal cross-backend detection by setting
// CrossWorkspaceCopyRequest.TargetBackend; leaving it empty disables the check
// entirely, which is correct for a single-backend deployment.
var ErrCopyCrossBackendAttachments = errors.New("copy item across workspaces: attachment bytes live in a different storage backend; cross-backend copy is not supported")

// ErrCopySourceCollectionMissing / ErrCopyTargetCollectionMissing are returned
// when a collection that existed when the caller was authorized is gone by the
// time this transaction re-reads it under the locks — soft-deleted, or (for the
// destination) never in the workspace the caller named.
//
// SENTINELS RATHER THAN fmt.Errorf, because the distinction is caller-facing.
// Both are pre-write rejections: nothing has been inserted and nothing can
// have committed. Reporting them as an anonymous failure would make the HTTP
// layer answer with its AMBIGUOUS-outcome 500 ("the copy may or may not have
// landed — check the destination"), which is both wrong and actively unhelpful
// — it sends the user hunting for an item that provably does not exist, over a
// condition that has a precise answer. The window is the same one
// AuthorizeCrossWorkspaceEdit warns about on both entry points; it is narrow,
// not impossible.
//
// They are deliberately NOT collapsed into one: the two sides get different
// responses under the disclosure posture — the source is a bare "Item not
// found", the destination the shared collection_not_found — and a single
// sentinel would force the handler to guess.
var (
	ErrCopySourceCollectionMissing = errors.New("copy item across workspaces: source collection not found")
	ErrCopyTargetCollectionMissing = errors.New("copy item across workspaces: target collection not found")
)

// ItemLimitError reports a DR-16 item-count quota rejection. It carries the
// LimitResult so the HTTP layer can render the same plan-limit payload
// writePlanLimitError produces for handleCreateItem.
type ItemLimitError struct {
	Result *LimitResult
}

func (e *ItemLimitError) Error() string {
	return fmt.Sprintf("copy item across workspaces: destination workspace is at its item limit (%d of %d)",
		e.Result.Current, e.Result.Limit)
}

// FieldValidationError reports a DR-12 validation failure — the destination
// fields, AFTER migration and AFTER overrides, do not satisfy the destination
// collection's schema. Distinguished from a generic error so the caller can
// answer with 400 rather than 500.
type FieldValidationError struct {
	Err error
}

func (e *FieldValidationError) Error() string {
	return fmt.Sprintf("copy item across workspaces: %v", e.Err)
}

func (e *FieldValidationError) Unwrap() error { return e.Err }

// UndeclaredOverrideError reports field overrides naming keys the DESTINATION
// collection's schema does not declare (TASK-2365, reconciling the second of
// TASK-2364's two KNOWN DIVERGENCES).
//
// Before this existed, migrateCopyFields merged every override
// unconditionally and ValidateFields ignores keys the schema does not
// declare — so the copy PERSISTED an undeclared override as an orphan key on
// the new item while the preflight refused the identical request with a 400
// `malformed_override`. That is exactly the DR-6 disagreement the shared
// preflight exists to prevent: the preview said "no", the copy said "yes, and
// here is a field your schema has never heard of".
//
// The preflight is the stricter and safer side, so the gate moved here rather
// than the rejection being removed there. Silently dropping the keys instead
// would be worse than either: a client that asked for a value would get an
// item without it and no way to tell.
//
// Keys is sorted, so the message is stable for the same input.
type UndeclaredOverrideError struct {
	Keys []string
}

func (e *UndeclaredOverrideError) Error() string {
	return fmt.Sprintf("copy item across workspaces: destination collection has no field(s): %s",
		strings.Join(e.Keys, ", "))
}

// CopyPreCheckError wraps a refusal from CrossWorkspaceCopyRequest.PreCheck.
//
// It exists purely so the rollback is classified as a caller-facing rejection
// rather than an incident: an authorization re-check that fires is a 403 or a
// 404 the HTTP layer renders, not something an operator should be paged about.
// The wrapped error is the caller's own, and Unwrap lets them recover it with
// errors.As to choose the status.
type CopyPreCheckError struct {
	Err error
}

func (e *CopyPreCheckError) Error() string {
	return fmt.Sprintf("copy item across workspaces: pre-check refused: %v", e.Err)
}

func (e *CopyPreCheckError) Unwrap() error { return e.Err }

// CrossWorkspaceCopyRequest is the complete input to CopyItemAcrossWorkspaces.
type CrossWorkspaceCopyRequest struct {
	// SourceItemID is the item in workspace A. The source workspace is
	// DERIVED from it rather than supplied — an item's workspace is not the
	// caller's to assert.
	SourceItemID string

	// TargetWorkspaceID and TargetCollectionID name the destination. The
	// collection is re-read in-tx under `workspace_id = TargetWorkspaceID AND
	// deleted_at IS NULL`, so a collection from another workspace is a
	// not-found, not a cross-workspace write.
	TargetWorkspaceID  string
	TargetCollectionID string

	// FieldOverrides are merged over the migrated fields and then validated
	// (DR-12 — MigrateFields computes its Errors before any override exists,
	// so those errors are stale the moment an override lands).
	FieldOverrides map[string]any

	// Actor is the user performing the copy. It becomes every cloned
	// attachment's uploaded_by (DR-11: never the source uploader, who may not
	// be a member of B at all) and the provenance row's created_by.
	Actor string

	// CreatedBy and Source are items.created_by / items.source for the new
	// row, matching CreateItem's vocabulary ("user"/"agent", "web"/"cli"/…).
	// Both default the same way CreateItem defaults them.
	CreatedBy string
	Source    string

	// ArchiveSource turns the copy into a move (DR-1): the source is
	// soft-deleted in the same transaction, workspace A's seq advances, and
	// the provenance row records that seq. A plain copy leaves A completely
	// untouched — no write, no seq bump, nothing for A's watchers to see.
	ArchiveSource bool

	// TargetBackend is the storage-backend prefix workspace B writes through
	// ("fs", "s3", …). Empty disables cross-backend detection — correct for a
	// single-backend deployment. See ErrCopyCrossBackendAttachments.
	TargetBackend string

	// AttachmentAuthorizer is the caller's per-attachment visibility check,
	// handed straight to the planner (TASK-2408). It is the SAME value the
	// preflight passes — both come out of resolveAuthorizedCopy — so the two
	// endpoints agree about which references are unresolvable for the same
	// reason they agree about everything else: one path, not two.
	//
	// See store.AttachmentAuthorizer for what nil means and why this is a
	// callback rather than a pre-filtered id set.
	AttachmentAuthorizer AttachmentAuthorizer

	// PreCheck, when non-nil, runs INSIDE the transaction once every input has
	// been re-read under the locks and before anything is written. Returning
	// an error rolls the whole copy back.
	//
	// It exists for one caller and one reason: TASK-2358's authorization
	// verdict is explicitly NOT ATOMIC, and PLAN-2357 DR-9 requires the
	// mutating copy to re-read both sides and re-check inside its
	// transaction. The four-check ladder itself stays at the HTTP layer —
	// this store op still enforces data invariants only (see the file header)
	// — but the SNAPSHOTS it judges have to be the locked ones, because the
	// item can be moved into a different (possibly hidden) collection between
	// the handler's check and the lock.
	//
	// WHAT THE ONE PRODUCTION HOOK ACTUALLY DOES, stated here because a future
	// caller must not assume more: it compares IDENTITY ONLY — that the locked
	// source item and destination collection are the same ones the handler's
	// ladder was run against. It does NOT re-read membership or grants, and
	// nothing in this transaction makes authorization state atomic with the
	// copy. See server.copyResourceInvariantPreCheck for why that is the
	// honest boundary (an I/O-performing hook here also risks pool starvation
	// while this transaction holds both workspaces' locks).
	//
	// `source` is the under-lock re-read of the source item; `targetColl` is
	// the under-lock, workspace-scoped destination collection. Both are
	// DETACHED DEEP COPIES of what the rest of the pipeline consumes (see
	// detachedSnapshot) — read them; retaining or mutating them, including
	// through their pointer and slice fields, changes nothing.
	//
	// Return any error to refuse. It is wrapped in CopyPreCheckError for you,
	// so a refusal is classified as a caller-facing rejection rather than an
	// operator-visible incident (see isExpectedCopyRejection); recover your
	// own error type with errors.As. The established shape is
	// MoveItemWithPreCheck / UpdateItemWithPreCheck.
	PreCheck func(tx *sql.Tx, source *models.Item, targetColl *models.Collection) error

	// EnforceItemLimit turns on the DR-16 items_per_workspace check against
	// the DESTINATION workspace, inside the transaction.
	//
	// It is a caller flag rather than an unconditional check for parity with
	// enforcePlanLimit, which self-hosted mode short-circuits before touching
	// the store (`if !s.cloudMode { return true }`). Enforcing unconditionally
	// here would apply free-tier caps to any self-hosted user whose plan row
	// says "free" — a limit that path has never had. Cloud callers set it;
	// self-hosted callers do not.
	EnforceItemLimit bool

	// failAfterStage is a TEST-ONLY seam. It is unexported, so nothing outside
	// internal/store can set it, and it is the only way to PROVE the rollback
	// obligation the acceptance criteria state — "a failure at each stage
	// leaves nothing in either workspace". Three of the four stages have no
	// reachable natural failure once the lock protocol holds: the archive's
	// row count and the provenance insert can only fail if something upstream
	// is already broken. Without a seam those branches would be asserted by
	// inspection rather than by a test that fails when the rollback breaks.
	//
	// See the copyStage* constants for the recognised values.
	failAfterStage string
}

// Stage names for CrossWorkspaceCopyRequest.failAfterStage.
const (
	copyStageCreateItem  = "create_item"
	copyStageAttachments = "attachments"
	copyStageArchive     = "archive"
	copyStageProvenance  = "provenance"
)

// injectedStageFailure returns a synthetic error when the request asked to
// fail after the named stage. Always nil in production — failAfterStage is
// unexported and no production caller can set it.
func (req CrossWorkspaceCopyRequest) injectedStageFailure(stage string) error {
	if req.failAfterStage == "" || req.failAfterStage != stage {
		return nil
	}
	return fmt.Errorf("copy item across workspaces: injected failure after stage %q", stage)
}

// CrossWorkspaceCopyResult is what a committed copy produced. Everything the
// post-commit fanout (TASK-2365) and the CLI/HTTP response need.
type CrossWorkspaceCopyResult struct {
	// Item is the destination item, read back inside the transaction, so its
	// Seq is exactly the one this copy assigned in workspace B.
	Item *models.Item

	// Source is the source item as re-read UNDER LOCK — the snapshot that was
	// actually copied, not the caller's pre-transaction read.
	Source *models.Item

	// SourceCollection / TargetCollection are the two collection rows as read
	// under the FOR UPDATE pin — the schemas the migration actually consumed.
	//
	// Returned rather than left to the caller to re-read, for the same reason
	// Source is: the caller's pre-transaction copies are advisory and both can
	// be stale by the time this commits. The item can have been MOVED into
	// another collection in A, and either collection can have been renamed.
	// A caller that builds its response or its fanout from the pre-transaction
	// rows attributes the events to a collection the copy did not use, and
	// hands the client a slug that no longer resolves (Codex round 1 P2).
	SourceCollection *models.Collection
	TargetCollection *models.Collection

	// SourceWorkspaceID is workspace A, derived from the source item.
	SourceWorkspaceID string

	// Move is the provenance row written in the same transaction.
	Move *models.ItemWorkspaceMove

	// SourceSeq is the seq the archive assigned in workspace A, nil on a plain
	// copy (which does not write in A at all).
	SourceSeq *int64

	// AttachmentsCopied / BytesCopied describe the cloned attachment rows.
	//
	// CALLER OBLIGATION: when AttachmentsCopied > 0 the destination
	// workspace's storage usage changed, and internal/server memoizes that for
	// 30 seconds. The caller MUST invalidate the destination's
	// storageInfoCache entry after a successful copy — the store has no handle
	// on it — or the storage page reports stale usage for the rest of the
	// window, right after the user watched the bytes land.
	AttachmentsCopied int
	BytesCopied       int64

	// UnresolvableRefs are pad-attachment references in the copied body that
	// resolved to nothing under the DR-11a scope. Never fatal; the literal
	// text survives so the copy renders exactly as broken as the source did.
	UnresolvableRefs []string

	// DroppedFields are field keys MigrateFields could not carry into the
	// destination schema. DroppedAssignee / DroppedAgentRole record the DR-8
	// scrubs.
	DroppedFields    []string
	DroppedAssignee  bool
	DroppedAgentRole bool
}

// CopyItemAcrossWorkspaces copies one item from its workspace into another,
// atomically.
//
// THE LOCK ORDER. Changed only deliberately, and stated here because it is the
// only place it is true:
//
//  1. Workspace A and B advisory locks, sorted and deduplicated BY THE
//     hashtext LOCK KEY.
//  2. Source and destination collection rows, sorted by collection ID, locked
//     FOR UPDATE.
//  3. Source item re-read under those locks.
//
// Three things that look like nits and are not:
//
//   - Sorting the workspace ID STRINGS does not order their hashes. Postgres
//     locks hashtext(workspace_id), so two opposing movers sorting by ID could
//     still take the two locks in opposite order and deadlock. Sort by the
//     computed key. And deduplicate: two distinct workspaces can collide onto
//     one key, in which case there is one lock to take.
//   - BOTH collection rows, not just the destination. MigrateFields consumes
//     both schemas, so pinning only the destination leaves half the input
//     racy and lets a dry-run and the commit disagree about what carries.
//     Sorted by collection ID for the same deadlock reason.
//   - FOR UPDATE is not optional. A schema-only collection update does not
//     necessarily take the workspace advisory lock, so merely READING the
//     collections after the advisory locks leaves a window to reshape a schema
//     before this transaction commits.
//
// pg_advisory_xact_lock and FOR UPDATE are dialect-gated. SQLite's DSN makes
// every db.Begin() a BEGIN IMMEDIATE, so all writers already serialize and the
// ordering concern is moot there — and FOR UPDATE is a syntax ERROR on SQLite,
// so emitting it unconditionally would fail on exactly one backend.
//
// THE PIPELINE, in the order DR-11 requires and no other:
//
//	fresh source under lock -> migrate fields -> apply overrides -> validate
//	  -> quota -> PlanAttachmentCopy(source content + FINAL destination fields)
//	  -> rewrite content + fields via the plan's IDMap
//	  -> createItemTxWithID in B (version row + wiki-link index therefore see
//	     the POST-rewrite content, per DR-9a)
//	  -> insert attachment rows, originals before variants, item_id set from
//	     the outset
//	  -> archive in A if requested, advancing A's seq
//	  -> provenance row carrying that seq
//
// Enumerating attachment refs from the FINAL fields rather than the source's
// raw fields is load-bearing: raw enumeration clones blobs referenced only by
// fields MigrateFields DROPS, and those land in B invisible and beyond the
// reach of the orphan sweep (which only considers item_id IS NULL rows).
//
// SEQ (DR-14). B always advances, via createItemTxWithID. A advances ONLY on
// ArchiveSource — and a plain copy must not advance it at all, so A's cursor
// stays put and A's watchers see nothing. The archive reproduces DeleteItem's
// acquireWorkspaceSeqLock + nextWorkspaceSeqSubquery deliberately rather than
// calling it, because DeleteItem opens its own transaction.
//
// WHAT DOES NOT CARRY (DR-17). The copy is unparented — ParentID is scrubbed —
// and it has no item_links, no children, no comments, no versions beyond its
// own initial one, no stars and no grants. Tags DO carry: items.tags is a
// plain JSON array on the row with no workspace-scoped entity behind it.
// AgentRoleID always clears (role slugs are workspace-local); AssignedUserID
// carries only when the assignee is a member of the destination (DR-8).
func (s *Store) CopyItemAcrossWorkspaces(req CrossWorkspaceCopyRequest) (*CrossWorkspaceCopyResult, error) {
	if req.SourceItemID == "" {
		return nil, fmt.Errorf("copy item across workspaces: source_item_id is required")
	}
	if req.TargetWorkspaceID == "" {
		return nil, fmt.Errorf("copy item across workspaces: target_workspace_id is required")
	}
	if req.TargetCollectionID == "" {
		return nil, fmt.Errorf("copy item across workspaces: target_collection_id is required")
	}
	if req.Actor == "" {
		return nil, fmt.Errorf("copy item across workspaces: actor is required")
	}

	// Derive workspace A before opening the transaction. This read is used for
	// NOTHING but the lock keys — every value the copy actually consumes is
	// re-read under the locks below. An item cannot change workspace (no code
	// path writes items.workspace_id), so a stale answer here is impossible in
	// a way the in-tx re-read would not catch anyway.
	sourceWorkspaceID, err := s.itemWorkspaceID(req.SourceItemID)
	if err != nil {
		return nil, err
	}

	result, err := s.copyItemAcrossWorkspacesTx(req, sourceWorkspaceID)
	if err != nil {
		// DR-9's observability obligation: the lock ordering is the one failure
		// mode nothing else surfaces, so an UNEXPECTED rollback — a deadlock
		// especially — is logged with both workspaces and the item.
		//
		// Expected, caller-facing REJECTIONS are excluded, and deliberately so.
		// A validation failure, a quota rejection, a missing source and a
		// cross-backend refusal are all 4xx answers the caller renders; they are
		// not incidents, and logging them here would (a) make the signal this
		// log exists for unfindable under routine bad requests, and (b) copy
		// user-controlled field values into the operator log, since
		// ValidateFields quotes the offending value verbatim. The quota
		// rejection gets its own line, with bounded fields and no user content,
		// at the point it is decided.
		if isExpectedCopyRejection(err) {
			return nil, err
		}
		// A genuine deadlock and SQLite writer contention are reported
		// SEPARATELY and at different severities, because conflating them
		// destroys the only signal this log exists to carry.
		//
		// SQLite is single-writer with a 30-second busy timeout (see store.go),
		// so "database is locked" is an expected saturation mode under burst
		// load — it says the box is busy, not that the lock ordering is wrong.
		// Postgres' 40P01 says the opposite: DR-9's ordering is meant to make
		// it impossible, so if it appears in production the ordering is subtly
		// wrong and nothing else will surface it. Logging both as
		// deadlock=true at ERROR would leave an operator at 3am unable to tell
		// a lock-ordering bug from ordinary load.
		deadlock := isDeadlockError(err)
		lockTimeout := !deadlock && isLockTimeoutError(err)
		attrs := []any{
			"source_workspace_id", sourceWorkspaceID,
			"target_workspace_id", req.TargetWorkspaceID,
			"source_item_id", req.SourceItemID,
			"archive_source", req.ArchiveSource,
			"deadlock", deadlock,
			"lock_timeout", lockTimeout,
			"error", err,
		}
		if deadlock {
			slog.Error("cross-workspace item copy rolled back", attrs...)
		} else {
			slog.Warn("cross-workspace item copy rolled back", attrs...)
		}
		return nil, err
	}
	return result, nil
}

// itemWorkspaceID reads an item's workspace, including soft-deleted rows so a
// copy of an already-archived source fails with a clear "not found" from the
// in-tx re-read rather than a confusing nil here.
func (s *Store) itemWorkspaceID(itemID string) (string, error) {
	var workspaceID string
	err := s.db.QueryRow(s.q(`SELECT workspace_id FROM items WHERE id = ?`), itemID).Scan(&workspaceID)
	if err == sql.ErrNoRows {
		return "", sql.ErrNoRows
	}
	if err != nil {
		return "", fmt.Errorf("copy item across workspaces: resolve source workspace: %w", err)
	}
	return workspaceID, nil
}

func (s *Store) copyItemAcrossWorkspacesTx(req CrossWorkspaceCopyRequest, sourceWorkspaceID string) (*CrossWorkspaceCopyResult, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("copy item across workspaces: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	// --- Step 1: both workspace advisory locks, ordered by lock KEY. ---
	if _, err := s.acquireWorkspaceLocksOrdered(tx, sourceWorkspaceID, req.TargetWorkspaceID); err != nil {
		return nil, err
	}

	// --- Step 2: both collection rows, FOR UPDATE, ordered by collection ID.
	// Read the source item's collection first — we need its ID to lock it, and
	// the workspace lock is already held so nothing can move the item now.
	var sourceCollectionID string
	if err := tx.QueryRow(s.q(`SELECT collection_id FROM items WHERE id = ?`), req.SourceItemID).Scan(&sourceCollectionID); err != nil {
		if err == sql.ErrNoRows {
			return nil, sql.ErrNoRows
		}
		return nil, fmt.Errorf("copy item across workspaces: read source collection id: %w", err)
	}
	if err := s.lockCollectionRows(tx, sourceCollectionID, req.TargetCollectionID); err != nil {
		return nil, err
	}

	sourceColl, err := s.getCollectionInWorkspaceTx(tx, sourceCollectionID, sourceWorkspaceID)
	if err != nil {
		return nil, err
	}
	if sourceColl == nil {
		return nil, ErrCopySourceCollectionMissing
	}
	targetColl, err := s.getCollectionInWorkspaceTx(tx, req.TargetCollectionID, req.TargetWorkspaceID)
	if err != nil {
		return nil, err
	}
	if targetColl == nil {
		return nil, ErrCopyTargetCollectionMissing
	}

	// --- Step 3: re-read the source under lock. Copy THIS snapshot. ---
	// Never the pre-transaction read: a concurrent edit or archive would
	// otherwise race the copy and workspace B would get a torn — or
	// already-archived — version. MoveItemWithPreCheck establishes the shape.
	source, err := s.getItemTx(tx, req.SourceItemID)
	if err != nil {
		return nil, err
	}
	if source == nil {
		// Deleted (or archived) between the pre-transaction read and the lock.
		return nil, sql.ErrNoRows
	}

	// --- Caller's in-tx re-check (DR-9), against the LOCKED snapshots and
	// before anything is written or any quota is consumed. ---
	//
	// DETACHED SNAPSHOTS, not the canonical pointers. The very objects handed
	// over here go on to drive migration, attachment planning and the insert,
	// and are returned in the result for the post-commit fanout — so a hook
	// that mutated one, or retained it and mutated it later, would silently
	// rewrite what this transaction copies or what its events say (Codex
	// round 10). A hook has no business needing more than a read, and
	// detachedSnapshot makes that structural rather than a convention —
	// including for every pointer- and slice-backed field, which a plain
	// struct copy would have left aliased (Codex rounds 11 and 12).
	//
	// The refusal is WRAPPED here rather than left to the hook. The doc on
	// PreCheck used to ask callers to wrap it themselves, which meant a
	// caller who forgot turned an intended 403 into an operator-visible
	// rollback incident. Guaranteeing it in one place removes the trap;
	// errors.As still recovers the caller's own error type through Unwrap.
	if req.PreCheck != nil {
		sourceSnapshot, err := detachedSnapshot(source)
		if err != nil {
			return nil, err
		}
		targetCollSnapshot, err := detachedSnapshot(targetColl)
		if err != nil {
			return nil, err
		}
		if err := req.PreCheck(tx, sourceSnapshot, targetCollSnapshot); err != nil {
			return nil, &CopyPreCheckError{Err: err}
		}
	}

	// --- Fields: migrate -> override -> validate (DR-12). ---
	//
	// BEFORE THE QUOTA, and the order is a contract rather than a preference
	// (Codex round 18). This is pure computation over rows already read, so
	// running it first costs nothing and changes no invariant — the quota
	// still runs inside the transaction and still runs before any insert,
	// which is all DR-16 requires. What it buys is agreement with the
	// preflight: the preflight has no quota check at all, so with the checks
	// the other way round a malformed request into a quota-full cloud
	// workspace got 403 plan_limit_exceeded from the copy and 400
	// malformed_override from its own preview. A bad request is a bad request
	// whether or not the destination happens to be full, and a client told
	// "you are out of room" cannot fix an override it was never told about.
	// Scope is COMPUTED from the two workspace ids rather than assumed
	// cross-workspace: this path also serves a copy whose target IS the source
	// workspace, and hardcoding CrossWorkspace would drop a github_pr from a
	// duplicate whose repo context never changed (BUG-2674). The preflight
	// computes it the same way — a divergence here would have the preview
	// promising a carry the copy drops, which DR-6 exists to prevent.
	scope := items.ScopeFor(sourceWorkspaceID, req.TargetWorkspaceID)
	// The TRANSACTION is the executor, not the pool. This function now reads
	// (relation referents, TASK-2878), and `copyItemAcrossWorkspacesTx` has
	// held a transaction since its second statement — a pool read from inside
	// it can wait for a free connection while every pooled connection is
	// itself blocked on this transaction's locks, which is the starvation
	// shape BUG-2409 fixed for the attachment planner and this repo keeps a
	// deterministic test for.
	//
	// The DESTINATION workspace id, not the source's: a supplied override is
	// a write into workspace B and has to name something that exists THERE.
	finalFields, dropped, err := s.migrateCopyFields(tx, req.TargetWorkspaceID, source.Fields, sourceColl.Schema, targetColl.Schema, req.FieldOverrides, scope)
	if err != nil {
		return nil, err
	}

	// --- Quota (DR-16), inside the transaction, before any insert. ---
	if req.EnforceItemLimit {
		limit, err := s.CheckLimitTx(tx, req.TargetWorkspaceID, "items_per_workspace")
		if err != nil {
			return nil, fmt.Errorf("copy item across workspaces: check item limit: %w", err)
		}
		if !limit.Allowed {
			slog.Warn("cross-workspace item copy rejected by item quota",
				"target_workspace_id", req.TargetWorkspaceID,
				"source_item_id", req.SourceItemID,
				"current", limit.Current,
				"limit", limit.Limit,
				"plan", limit.Plan)
			return nil, &ItemLimitError{Result: limit}
		}
	}

	// --- DR-8 / DR-17 scrubs against the DESTINATION workspace. ---
	assignedUserID, droppedAssignee, err := s.carryAssigneeTx(tx, req.TargetWorkspaceID, source.AssignedUserID)
	if err != nil {
		return nil, err
	}
	droppedAgentRole := source.AgentRoleID != nil && *source.AgentRoleID != ""

	// --- Attachments: plan INSIDE the transaction (staleness contract). ---
	// The plan is a snapshot valid only in the critical section that produced
	// it; caching one across the lock would let a soft-delete, an orphan-GC
	// reclaim or a revoked membership slip between planning and inserting.
	//
	// The planner reads ON THIS TRANSACTION, not through the pool (BUG-2409).
	// This transaction holds both workspace advisory locks, and a pool read
	// here can wait for a free connection while every pooled connection is
	// itself occupied by another copy waiting on these locks — starvation
	// presenting as a hang. Running the reads (the planner's passes AND the
	// Authorize callback's, which receives the same executor) on the
	// transaction's own connection removes the pool from the critical
	// section entirely. The dry-run still plans through the pool via
	// PlanAttachmentCopy — one implementation, two executors, so the shared
	// no-drift shape TASK-2354 established survives.
	//
	// What tx-routing deliberately does NOT change is the attachment-row
	// staleness window, which remains bounded and harmless (TASK-2354):
	//
	//   - On SQLite there is no window at all. BEGIN IMMEDIATE means this
	//     transaction holds the database's write lock, so a concurrent
	//     SoftDeleteAttachment / HardDeleteAttachment simply blocks until the
	//     copy commits.
	//   - On Postgres a concurrent soft-delete CAN land between planning and
	//     inserting, tx or no tx: READ COMMITTED takes a fresh snapshot per
	//     statement, so the same committed delete is just as visible either
	//     way. Only making every attachment writer take the workspace
	//     advisory lock would close it, which means putting a lock on the
	//     upload hot path for this.
	//   - And the outcome of losing that race is benign. Soft-delete never
	//     removes bytes, and the clone this transaction commits carries the
	//     same content_hash — so it is itself a protecting row for
	//     CountProtectingAttachmentsForHash, which is workspace-agnostic. The
	//     orphan GC therefore cannot reclaim the blob out from under the copy.
	//     What workspace B gets is a copy of something the user deleted in A a
	//     moment after asking for the copy, which is defensible on its own.
	//
	// The destination item id is minted HERE, before the item row exists,
	// because every clone must carry item_id from the outset — a
	// NULL-item_id row that the copied body then references is a permanent,
	// un-reclaimable orphan (see AttachmentCopyRequest.DryRun).
	targetItemID := newID()
	plan, err := s.planAttachmentCopyQ(tx, AttachmentCopyRequest{
		SourceWorkspaceID: sourceWorkspaceID,
		TargetWorkspaceID: req.TargetWorkspaceID,
		TargetItemID:      targetItemID,
		UploadedBy:        req.Actor,
		Content:           source.Content,
		Fields:            finalFields,
		TargetBackend:     req.TargetBackend,
		Authorize:         req.AttachmentAuthorizer,
	})
	if err != nil {
		return nil, err
	}
	if plan.CrossBackend {
		return nil, ErrCopyCrossBackendAttachments
	}
	if len(plan.UnresolvableRefs) > 0 {
		// DR-11a observability: a spike means either a data-integrity problem
		// or someone probing the confused-deputy path. Never fatal.
		slog.Info("cross-workspace item copy has unresolvable attachment refs",
			"source_workspace_id", sourceWorkspaceID,
			"target_workspace_id", req.TargetWorkspaceID,
			"source_item_id", req.SourceItemID,
			"unresolvable_refs", len(plan.UnresolvableRefs))
	}

	// --- Rewrite content AND fields with the plan's IDMap. ---
	// Both go through remapAttachmentRefs over the exact representations the
	// planner enumerated from (raw content; the fields' JSON encoding), so the
	// rewrite covers precisely the reference set the plan cloned. Any other
	// route risks covering a different set.
	newContent := remapAttachmentRefs(source.Content, plan.IDMap)
	finalFieldsJSON, err := json.Marshal(finalFields)
	if err != nil {
		return nil, fmt.Errorf("copy item across workspaces: encode destination fields: %w", err)
	}
	newFieldsJSON := remapAttachmentRefs(string(finalFieldsJSON), plan.IDMap)

	// --- Create in B. Advances B's seq; writes the initial version row and
	// the wiki-link index against the POST-rewrite content (DR-9a). ---
	// TITLE VALIDATION IS DELIBERATELY NOT APPLIED HERE (BUG-2833 / BUG-2831,
	// raised again by codex R2 as a finding — recorded so the next reader does
	// not have to re-derive it).
	//
	// This path goes through createItemTxWithID, below store.CreateItem's
	// write-time guard, so a source row whose title predates the bound is
	// carried across intact. That is the intended behaviour, on the same
	// reasoning as the grandfathering clause in UpdateItem and the lead's
	// coerce-not-refuse ruling for ImportWorkspace: a copy carries a value that
	// already exists in the database, and refusing it would break a working
	// operation for data this product itself accepted.
	//
	// What it cannot do is mint an invalid title FROM CALLER INPUT — which is
	// the guarantee this unit actually makes, and is narrower than "cannot mint
	// an invalid title", since a legacy-invalid source row does put that title
	// on a NEW destination row (codex round 6). The title below is
	// `source.Title`, read from the source row; there is no caller-supplied
	// title on this path at all (`--field` sets destination FIELDS, never the
	// title), so no input a user controls reaches it. The copy stays repairable
	// by rename in either workspace, where the bound does apply.
	item, err := s.createItemTxWithID(tx, targetItemID, req.TargetWorkspaceID, req.TargetCollectionID, models.ItemCreate{
		Title:   source.Title,
		Content: newContent,
		Fields:  newFieldsJSON,
		Tags:    source.Tags,
		// ParentID stays nil (DR-17): the source's parent lives in A, and
		// DR-4 rules out dragging relatives along.
		ParentID:       nil,
		AssignedUserID: assignedUserID,
		AgentRoleID:    nil,
		CreatedBy:      req.CreatedBy,
		Source:         req.Source,
	})
	if err != nil {
		return nil, err
	}
	if err := req.injectedStageFailure(copyStageCreateItem); err != nil {
		return nil, err
	}

	// --- Attachment rows, in the planner's order (originals before their
	// variants). attachments has no parent_id foreign key, so this ordering is
	// a caller contract the database will not enforce. ---
	for i := range plan.Rows {
		row := plan.Rows[i].Attachment
		if err := s.CreateAttachmentTx(tx, &row); err != nil {
			return nil, fmt.Errorf("copy item across workspaces: clone attachment %s: %w", plan.Rows[i].SourceID, err)
		}
	}
	if err := req.injectedStageFailure(copyStageAttachments); err != nil {
		return nil, err
	}

	// --- Archive the source (move only), advancing A's seq (DR-14). ---
	var sourceSeq *int64
	archiveTS := now()
	if req.ArchiveSource {
		seq, err := s.archiveItemForCopyTx(tx, sourceWorkspaceID, req.SourceItemID, archiveTS)
		if err != nil {
			return nil, err
		}
		sourceSeq = &seq
	}
	if err := req.injectedStageFailure(copyStageArchive); err != nil {
		return nil, err
	}

	// --- Provenance. Written last so it can carry the archive's seq, and in
	// the same transaction so a rollback can never leave a pointer at an item
	// that does not exist. ---
	move, err := s.RecordItemWorkspaceMoveTx(tx, models.ItemWorkspaceMove{
		SourceWorkspaceID: sourceWorkspaceID,
		SourceItemID:      req.SourceItemID,
		TargetWorkspaceID: req.TargetWorkspaceID,
		TargetItemID:      item.ID,
		ArchivedSource:    req.ArchiveSource,
		SourceSeq:         sourceSeq,
		CreatedBy:         req.Actor,
		CreatedAt:         archiveTS,
	})
	if err != nil {
		return nil, err
	}
	if err := req.injectedStageFailure(copyStageProvenance); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("copy item across workspaces: commit: %w", err)
	}

	return &CrossWorkspaceCopyResult{
		Item:              item,
		Source:            source,
		SourceCollection:  sourceColl,
		TargetCollection:  targetColl,
		SourceWorkspaceID: sourceWorkspaceID,
		Move:              move,
		SourceSeq:         sourceSeq,
		AttachmentsCopied: len(plan.Rows),
		BytesCopied:       plan.TotalBytes,
		UnresolvableRefs:  plan.UnresolvableRefs,
		DroppedFields:     dropped,
		DroppedAssignee:   droppedAssignee,
		DroppedAgentRole:  droppedAgentRole,
	}, nil
}

// detachedSnapshot returns a deep copy of v that a PreCheck hook can neither
// mutate nor retain into anything the copy depends on.
//
// WHY NOT `out := *src`. Struct assignment copies the POINTER and SLICE
// fields, so a hook doing `*source.AssignedUserID = "someone-else"` reaches
// straight through into carryAssigneeTx and puts a different user on the
// copied item (Codex round 11).
//
// WHY NOT FIELD-BY-FIELD CLONING. That was the first fix, and it was wrong in
// the way hand-maintained lists are always wrong: models.Item carries a dozen
// reference-backed fields — ParentID, AssignedUserID, AgentRoleID, ItemNumber,
// DeletedAt, IsUnparented, DerivedClosure, CodeContext, Convention, MovedTo,
// ImplementationNotes, DecisionLog — several of which getItemTx hydrates, and
// the enumeration missed four of them (Codex round 12). Worse, it would go
// stale silently the next time a field is added to the model, in a way no test
// would notice.
//
// A JSON round-trip is total by construction and stays correct as the models
// grow. It is legitimate here specifically because models.Item and
// models.Collection are API DTOs: every field carries a JSON tag and none is
// `json:"-"`, so nothing is lost. The cost — one marshal plus one unmarshal —
// is paid once per copy, only when a hook is installed, against a transaction
// that already holds two workspaces' advisory locks.
func detachedSnapshot[T any](src *T) (*T, error) {
	raw, err := json.Marshal(src)
	if err != nil {
		return nil, fmt.Errorf("copy item across workspaces: snapshot for pre-check: %w", err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("copy item across workspaces: snapshot for pre-check: %w", err)
	}
	return &out, nil
}

// acquireWorkspaceLocksOrdered takes the workspace advisory locks for every
// supplied workspace, in ascending lock-KEY order, with duplicates collapsed.
// Returns the ordered key set actually locked (nil on SQLite).
//
// Sorting by lock key rather than by workspace ID is the entire point. Postgres
// locks hashtext(workspace_id) — the same key acquireWorkspaceSeqLock uses, so
// these acquisitions are re-entrant with the one createItemTxWithID takes later
// — and hashtext does not preserve string order. An A->B copy and a B->A copy
// sorting by ID would therefore be free to grab the two locks in opposite
// order and deadlock, which is the one failure DR-9 exists to prevent.
//
// Deduplicating is not cosmetic either: hashtext is a 32-bit hash, so two
// distinct workspaces CAN collide onto one key. When they do there is one lock
// to take, not two. (Taking it twice would in fact be harmless — advisory xact
// locks are re-entrant — but the returned key set is what tests assert on, and
// "how many distinct locks does this transaction hold" should be answerable.)
//
// SQLite is a no-op: BEGIN IMMEDIATE already serializes every writer, so there
// is no interleaving for an ordering to protect.
func (s *Store) acquireWorkspaceLocksOrdered(tx *sql.Tx, workspaceIDs ...string) ([]int64, error) {
	if s.dialect.Driver() != DriverPostgres {
		return nil, nil
	}

	keys := make([]int64, 0, len(workspaceIDs))
	for _, id := range workspaceIDs {
		if id == "" {
			continue
		}
		var key int64
		// hashtext returns int4; the ::bigint cast makes the value we sort and
		// lock on identical to the one pg_advisory_xact_lock(bigint) resolves
		// to when acquireWorkspaceSeqLock passes hashtext($1) directly.
		if err := tx.QueryRow("SELECT hashtext($1)::bigint", id).Scan(&key); err != nil {
			return nil, fmt.Errorf("compute workspace lock key for %q: %w", id, err)
		}
		keys = append(keys, key)
	}

	ordered := sortedDedupedLockKeys(keys)
	for _, key := range ordered {
		if _, err := tx.Exec("SELECT pg_advisory_xact_lock($1)", key); err != nil {
			return nil, fmt.Errorf("acquire workspace lock %d: %w", key, err)
		}
	}
	return ordered, nil
}

// sortedDedupedLockKeys returns keys in ascending order with duplicates
// collapsed. Split out from the acquisition so the ordering contract is
// testable without a database.
func sortedDedupedLockKeys(keys []int64) []int64 {
	if len(keys) == 0 {
		return nil
	}
	sorted := append([]int64(nil), keys...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	out := sorted[:1]
	for _, k := range sorted[1:] {
		if k != out[len(out)-1] {
			out = append(out, k)
		}
	}
	return out
}

// lockCollectionRows pins the source and destination collection rows FOR
// UPDATE, in ascending collection-ID order with duplicates collapsed.
//
// BOTH rows, because MigrateFields consumes both schemas: pinning only the
// destination leaves half the migration input free to change under the
// transaction, so a dry-run and the commit that follows it can disagree about
// which fields carry.
//
// FOR UPDATE rather than a plain read, because a schema-only collection update
// does not necessarily take the workspace advisory lock — reading after the
// advisory locks would still leave a window to reshape the schema before this
// transaction commits.
//
// Sorted for the same reason the workspace locks are: two copies whose
// collection sets overlap must take the overlap in one order. SQLite is a
// no-op, and FOR UPDATE is a SYNTAX ERROR there — the dialect gate is not an
// optimization.
func (s *Store) lockCollectionRows(tx *sql.Tx, collectionIDs ...string) error {
	if s.dialect.Driver() != DriverPostgres {
		return nil
	}
	seen := make(map[string]struct{}, len(collectionIDs))
	ids := make([]string, 0, len(collectionIDs))
	for _, id := range collectionIDs {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		var got string
		err := tx.QueryRow(s.q(`SELECT id FROM collections WHERE id = ? FOR UPDATE`), id).Scan(&got)
		if err == sql.ErrNoRows {
			// Not an error here — the scoped re-read below turns a missing or
			// soft-deleted collection into the caller-facing "not found".
			continue
		}
		if err != nil {
			return fmt.Errorf("lock collection %q: %w", id, err)
		}
	}
	return nil
}

// getCollectionInWorkspaceTx reads a live collection inside the transaction,
// scoped to a workspace. The scope is the security boundary, not a hint: it is
// what makes "target collection in another workspace" a not-found instead of a
// cross-workspace write. Returns (nil, nil) when there is no such row.
//
// The IN-TX read is authoritative. A dry-run's schema snapshot is advisory —
// it was taken without the FOR UPDATE pin above, so it can be stale by the
// time the copy commits.
func (s *Store) getCollectionInWorkspaceTx(tx *sql.Tx, collectionID, workspaceID string) (*models.Collection, error) {
	c, err := s.scanCollectionRow(tx, getCollectionInWorkspaceQuery, collectionID, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("copy item across workspaces: read collection: %w", err)
	}
	return c, nil
}

// migrateCopyFields runs the DR-12 field pipeline: migrate the source fields
// into the destination schema, merge the caller's overrides, then validate.
//
// The ORDER is the decision. MigrateFields computes result.Errors before any
// override exists, so testing those errors after merging overrides in — which
// is what the existing single-workspace move path does — reports required
// fields an override has already satisfied, and never type-checks the override
// itself. ValidateFields is re-run over the merged map instead: it enforces
// required presence, applies schema defaults, and validates types and options.
//
// THE OVERRIDE MERGE IS THE PREFLIGHT'S, EXACTLY (TASK-2365). Both of
// TASK-2364's KNOWN DIVERGENCES lived in this loop and both are closed here,
// on the side the preflight already took:
//
//   - an UNDECLARED key is refused (UndeclaredOverrideError), not merged into
//     items.fields as an orphan the destination schema never mentions;
//   - a NULL value DELETES the key rather than assigning nil. ValidateFields
//     treats a nil value as absent for the required check but LEAVES IT IN THE
//     MAP, so assigning nil persisted a literal `"key": null` that the preview
//     reported as unset. Deleting also lets the destination schema's DEFAULT
//     re-apply, which is what the preflight shows.
//
// Anything added to this loop has a counterpart in
// handleCopyItemPreflight's, and TestCopyEndpoint_PreflightAndCopyAgree*
// fails when the two drift.
//
// NOT BYTE-FAITHFUL, and deliberately not fixed here (Codex round 26).
// Decoding items.fields into map[string]any and re-encoding it turns every
// JSON number into a float64, so an integer past 2^53 is rounded, and key
// order, escaping and number formatting are normalised rather than preserved.
// That is how EVERY field write in Pad works — handleMoveItem, handleUpdateItem
// and items.ValidateFields all operate on map[string]any — and, decisively,
// the preflight does the identical round-trip, so the preview and the copy
// AGREE. Making the copy alone byte-faithful would break that agreement, which
// is the one thing this pipeline exists to preserve. It belongs with BUG-2367,
// as a change to the field model for all callers at once.
//
// Returns the final field map (the planner's input, pre-rewrite) and the keys
// migration dropped.
func (s *Store) migrateCopyFields(q Queryer, destWorkspaceID, sourceFieldsJSON, sourceSchemaJSON, targetSchemaJSON string, overrides map[string]any, scope items.MigrateScope) (map[string]any, []string, error) {
	var sourceSchema, targetSchema models.CollectionSchema
	if err := json.Unmarshal([]byte(sourceSchemaJSON), &sourceSchema); err != nil {
		return nil, nil, fmt.Errorf("copy item across workspaces: parse source schema: %w", err)
	}
	if err := json.Unmarshal([]byte(targetSchemaJSON), &targetSchema); err != nil {
		return nil, nil, fmt.Errorf("copy item across workspaces: parse target schema: %w", err)
	}

	// Refused BEFORE the source item's fields are even parsed, so the
	// rejection cannot depend on the source's contents — same ordering the
	// preflight uses for the same reason.
	if bad := items.UndeclaredOverrideKeys(overrides, items.SchemaForMigratedFields(targetSchema).Fields); len(bad) > 0 {
		return nil, nil, &UndeclaredOverrideError{Keys: bad}
	}

	currentFields := map[string]any{}
	if strings.TrimSpace(sourceFieldsJSON) != "" {
		if err := json.Unmarshal([]byte(sourceFieldsJSON), &currentFields); err != nil {
			// A source row with unparseable fields migrates as if it had none,
			// matching handleMoveItem's tolerance. Refusing would strand the
			// item in A with no way out.
			currentFields = map[string]any{}
		}
	}

	migrated := items.MigrateFields(currentFields, sourceSchema.Fields, targetSchema.Fields, scope)
	for k, v := range overrides {
		if v == nil {
			// An explicit null means "leave this unset". DELETE rather than
			// assign nil: ValidateFields leaves a nil value in the map, so
			// assignment persists a literal `"key": null` the preflight
			// reported as unset — and suppresses the schema default that the
			// preflight shows re-applying.
			delete(migrated.Fields, k)
			continue
		}
		migrated.Fields[k] = v
	}
	// Coerce strings to their declared types before validating (BUG-2850).
	// MUST match the preflight (handlers_items_copy_preflight.go) — see the
	// note there; these two live in different PACKAGES, which is exactly how
	// they would drift unnoticed.
	migrated.Fields = items.CoerceFields(migrated.Fields, items.SchemaForMigratedFields(targetSchema))
	// Relation referents (TASK-2878) — the eighth and last coercion door, and
	// the only one that refuses from inside `store`.
	//
	// A migrate door, so PROVENANCE decides rather than the door: a SUPPLIED
	// override is an ordinary write and an unresolvable one is refused; a
	// CARRIED value was asserted by nobody, so it is dropped and reported
	// through the same `dropped_fields` channel BUG-2674 established. The
	// alternative — refusing carried values — would make every legacy item
	// uncopyable, and `internal/items` has accepted any string for a relation
	// all along, so "legacy" is most of them.
	//
	// MODE COMES FROM `scope`, the same value MigrateFields was given, rather
	// than a second flag derived here. Two independent answers to "is this
	// crossing a workspace boundary" is how one request gets migrated one way
	// and validated the other; this path also serves a copy whose target IS
	// the source workspace, where relations resolve and survive exactly as
	// they do on a move.
	mode := RelationCarryWithinWorkspace
	if scope == items.CrossWorkspace {
		mode = RelationCarryCrossWorkspace
	}
	relRefusals, relDropped, relErr := s.MigrateRelationReferentsQ(q, destWorkspaceID,
		items.SchemaForMigratedFields(targetSchema), migrated.Fields, overrides, currentFields, mode)
	if relErr != nil {
		return nil, nil, fmt.Errorf("copy item across workspaces: resolve relation referents: %w", relErr)
	}
	if len(relRefusals) > 0 {
		// The same 400 validation_error the preflight's refuseRelationIssues
		// emits, through the channel this function already uses for a failed
		// destination validation — so the copy and its preview refuse one
		// request with one code and one sentence.
		return nil, nil, &FieldValidationError{Err: errors.New(RelationIssuesMessage(relRefusals))}
	}
	// Appended to Dropped rather than reported separately: StillDropped below
	// filters this list against the FINAL map, and MigrateRelationReferentsQ
	// has already deleted these keys from it, so they survive that filter and
	// reach `warnings.dropped_fields` exactly as a type-mismatch drop does.
	for _, ri := range relDropped {
		migrated.Dropped = append(migrated.Dropped, ri.Key)
	}
	// Snapshot AFTER the pass above and BEFORE validation: what this needs to
	// identify is exactly what VALIDATION adds — which includes a default the
	// pass just deleted as unresolvable and validation puts straight back.
	// Snapshotting before the pass would treat that key as already examined
	// and skip it, which is the arrangement that hid it.
	relBefore := RelationKeysPresent(items.SchemaForMigratedFields(targetSchema), migrated.Fields)
	if err := items.ValidateFields(migrated.Fields, items.SchemaForMigratedFields(targetSchema)); err != nil {
		return nil, nil, &FieldValidationError{Err: err}
	}
	// Relation defaults ValidateFields just injected, which the pass above
	// could not have seen (codex round 2).
	lateDropped, lateErr := s.ResolveLateRelationDefaultsQ(q, destWorkspaceID,
		items.SchemaForMigratedFields(targetSchema), migrated.Fields, relBefore)
	if lateErr != nil {
		return nil, nil, fmt.Errorf("copy item across workspaces: resolve relation defaults: %w", lateErr)
	}
	// A REQUIRED relation whose default did not resolve cannot be left as a
	// drop: the key is deleted AFTER validation passed, so nothing re-checks
	// it and the item would land with a required field absent, reported valid
	// (codex round 3). Re-running validation is not the answer — it would
	// re-inject the same broken default. There is no valid value, so this
	// refuses.
	if req := RequiredRelationIssues(items.SchemaForMigratedFields(targetSchema), lateDropped); len(req) > 0 {
		return nil, nil, &FieldValidationError{Err: errors.New(RelationIssuesMessage(req))}
	}
	for _, ri := range lateDropped {
		migrated.Dropped = append(migrated.Dropped, ri.Key)
	}
	// Filtered against the FINAL map, matching the preflight (Codex round 3).
	// migrated.Dropped is computed before overrides merge and before defaults
	// are injected, and this list is exposed to the caller as the 201
	// response's warnings.dropped_fields — so without this the preview said
	// "carried", the copy PERSISTED the key, and the copy's own response
	// still reported it dropped. Three surfaces, two answers, one request.
	return migrated.Fields, items.StillDropped(migrated.Dropped, migrated.Fields), nil
}

// carryAssigneeTx implements DR-8's assignee rule: the source's assignee
// carries only when that user is a member of the DESTINATION workspace, and
// otherwise clears. Returns the value to write and whether it was dropped.
//
// createItemTxWithID would reject a non-member outright
// (validateAssignmentScopeQ), so this is not belt-and-braces — it is the
// difference between a copy that quietly drops an assignment and a copy that
// refuses because someone left workspace B.
func (s *Store) carryAssigneeTx(tx *sql.Tx, targetWorkspaceID string, sourceAssignee *string) (*string, bool, error) {
	if sourceAssignee == nil || *sourceAssignee == "" {
		return nil, false, nil
	}
	var count int
	if err := tx.QueryRow(
		s.q("SELECT COUNT(*) FROM workspace_members WHERE workspace_id = ? AND user_id = ?"),
		targetWorkspaceID, *sourceAssignee,
	).Scan(&count); err != nil {
		return nil, false, fmt.Errorf("copy item across workspaces: check destination membership: %w", err)
	}
	if count == 0 {
		return nil, true, nil
	}
	carried := *sourceAssignee
	return &carried, false, nil
}

// archiveItemForCopyTx soft-deletes the source inside the copy's transaction
// and returns the workspace-A seq the archive assigned.
//
// This REPRODUCES DeleteItem rather than calling it: DeleteItem opens and
// commits its own transaction, so calling it would put the archive outside the
// copy's atomic boundary — a crash between the two would strand a live source
// alongside a committed duplicate. The pieces that matter are the seq bump
// (nextWorkspaceSeqSubquery under the workspace advisory lock) and the
// `deleted_at IS NULL` guard that makes a concurrent archive a no-op rather
// than a second tombstone.
//
// The lock re-acquisition is a no-op — acquireWorkspaceLocksOrdered already
// holds workspace A's key and advisory xact locks are re-entrant — but taking
// it explicitly keeps this function correct on its own terms rather than by
// the grace of its only caller.
//
// The assigned seq is read back inside the transaction, under the still-held
// lock, so the value handed to the provenance row is exactly the one A's
// delta-sync clients will see on the tombstone.
func (s *Store) archiveItemForCopyTx(tx *sql.Tx, workspaceID, itemID, ts string) (int64, error) {
	if err := s.acquireWorkspaceSeqLock(tx, workspaceID); err != nil {
		return 0, err
	}

	// The event has to be reproduced here for the SAME reason the archive is
	// (TASK-2658). Because this path deliberately does not call DeleteItem, it
	// does not inherit DeleteItem's item.deleted emit either — so a
	// cross-workspace MOVE would archive the source with no event while an
	// ordinary archive of the same item emits one. That asymmetry is invisible
	// until something consumes the outbox, at which point moves silently stop
	// being observable; catching it now is cheaper than discovering it as a
	// missing-event bug later.
	//
	// Same ordering as DeleteItem: snapshot BEFORE the UPDATE, in-tx, while
	// the row is still live (getItemTx filters archived rows), because SPEC-3
	// requires the final pre-archive state.
	preArchive, err := s.getItemTx(tx, itemID)
	if err != nil {
		return 0, fmt.Errorf("copy item across workspaces: read pre-archive snapshot: %w", err)
	}

	res, err := tx.Exec(s.q(`
		UPDATE items SET deleted_at = ?, updated_at = ?, seq = `+nextWorkspaceSeqSubquery+`
		WHERE id = ? AND deleted_at IS NULL
	`), ts, ts, workspaceID, itemID)
	if err != nil {
		return 0, fmt.Errorf("copy item across workspaces: archive source: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("copy item across workspaces: archive source: %w", err)
	}
	if affected == 0 {
		// The source was re-read live under the lock moments ago, so this
		// cannot happen without the lock protocol being broken. Fail loudly
		// rather than record a provenance row claiming a move that did not
		// happen.
		return 0, fmt.Errorf("copy item across workspaces: source item %s was not archived", itemID)
	}
	if preArchive != nil {
		if err := s.emitItemEventTx(tx, kernelevents.ItemDeleted, preArchive, nil, ""); err != nil {
			return 0, err
		}
	}

	var seq int64
	if err := tx.QueryRow(s.q(`SELECT seq FROM items WHERE id = ?`), itemID).Scan(&seq); err != nil {
		return 0, fmt.Errorf("copy item across workspaces: read archive seq: %w", err)
	}
	return seq, nil
}

// isExpectedCopyRejection reports whether err is a refusal the CALLER is meant
// to render — a 4xx — rather than an incident an operator should see.
//
// Kept as one predicate so the set is stated in a single place: field
// validation (DR-12), an undeclared field override, the item quota (DR-16,
// which logs its own bounded line), a source that is missing or already
// archived, a caller-supplied PreCheck refusal (the HTTP layer's in-tx
// authorization re-check, which is a 403/404 the caller renders), the v1
// cross-backend attachment refusal, and a unique-constraint violation.
//
// The unique violation is here because the HTTP layer maps it to a
// caller-facing 409 (writeCopyError in handlers_items_copy.go), so it is a
// routine answer, not an incident — a workspace-unique field such as a
// playbook's invocation_slug colliding in the destination reaches this path on
// ordinary input. Classifying it as unexpected would fire an operator warning
// per 409 and bury the deadlock signal this log exists to surface. The two
// classifications must agree; they disagreed until PLAN-2357's final review.
//
// Everything else — a DB error, a deadlock, any other constraint violation —
// is unexpected and gets logged.
func isExpectedCopyRejection(err error) bool {
	var validation *FieldValidationError
	var undeclared *UndeclaredOverrideError
	var limit *ItemLimitError
	var precheck *CopyPreCheckError
	return errors.As(err, &validation) ||
		errors.As(err, &undeclared) ||
		errors.As(err, &limit) ||
		errors.As(err, &precheck) ||
		errors.Is(err, sql.ErrNoRows) ||
		errors.Is(err, ErrCopySourceCollectionMissing) ||
		errors.Is(err, ErrCopyTargetCollectionMissing) ||
		errors.Is(err, ErrCopyCrossBackendAttachments) ||
		IsUniqueViolation(err)
}

// isDeadlockError reports whether err is a genuine deadlock — Postgres'
// SQLSTATE 40P01. It deliberately does NOT match SQLite's "database is
// locked"; see isLockTimeoutError for why the two must stay apart.
//
// String matching rather than a driver type assertion because internal/store
// is driver-agnostic and both drivers are behind database/sql here; the
// strings are stable parts of each engine's user-facing error text.
func isDeadlockError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "deadlock detected") ||
		strings.Contains(msg, "40p01")
}

// isLockTimeoutError reports whether err is SQLite's writer-contention
// timeout.
//
// This is a SATURATION signal, not a correctness one. The SQLite DSN makes
// every transaction BEGIN IMMEDIATE with a 30-second busy timeout (store.go),
// so a single writer holding the lock through a burst produces "database is
// locked" on ordinary, correct traffic. A deadlock means the opposite — that
// DR-9's lock ordering, which is supposed to make 40P01 impossible, is wrong.
// An operator has to be able to tell those apart from the log line alone.
func isLockTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "database is locked")
}
