package server

import (
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// attachmentParentOutcome classifies the result of resolving an attachment
// row's parent item.
//
// The four cases exist because attachments.item_id carries NO foreign key and
// NO same-workspace constraint (fixed at the source by TASK-2400, repaired for
// existing rows by PLAN-2397). "Resolves to an item" and "is a legitimate
// parent for THIS row" are therefore different questions, and every caller has
// to ask both. Splitting the outcomes rather than returning a bare bool keeps
// the malformed cases distinguishable for callers that want to log them
// (derivation does) without forcing that distinction onto callers that must
// NOT surface it (the HTTP paths, where any split is an existence oracle).
type attachmentParentOutcome int

const (
	// attachmentParentOrphan — item_id IS NULL. There is no parent to
	// authorize against, so the caller applies its own orphan rule; see
	// attachmentCallerIsRestricted for the one the HTTP paths share.
	attachmentParentOrphan attachmentParentOutcome = iota
	// attachmentParentOK — resolved to an item of the attachment's own
	// workspace. Live, unless the caller passed includeArchived.
	attachmentParentOK
	// attachmentParentGone — item_id names nothing the lookup returned:
	// soft-deleted (when includeArchived is false), hard-deleted, or a
	// dangling id that never named an item.
	attachmentParentGone
	// attachmentParentForeign — resolves to an item in ANOTHER workspace.
	// Malformed for this row: a grant on that foreign item must never
	// authorize anything against this workspace's bytes.
	attachmentParentForeign
)

// resolveAttachmentParentItem loads and validates the parent item of an
// attachment row that has ALREADY been established as live and belonging to
// the request workspace.
//
// This is the one place the attachment-parent invariant lives. Before it, the
// blob read, the transform, thumbnail derivation and the delete path each
// hand-rolled "load the parent, check workspace identity, check liveness" in
// its own shape, and the three implementations drifted apart far enough to
// open two real gaps (see the commit that introduced this helper). The
// invariant is now shared; the DENIAL behaviour deliberately is not.
//
// What the callers keep for themselves, and why:
//
//   - the blob read (handleGetAttachment) and transform
//     (handleTransformAttachment) funnel every outcome except OK through
//     writeAttachmentNotFound, so a missing attachment, a foreign parent, an
//     archived parent and an invisible item are byte-identical;
//   - derivation (deriveThumbnails) has no HTTP response to shape at all: it
//     logs a distinct WARN per outcome — the malformed cases are meant to be
//     greppable ahead of PLAN-2397's repair — and skips;
//   - the delete path passes includeArchived, because the storage listing
//     intentionally surfaces attachments whose parent item is soft-deleted:
//     they still consume quota and the user needs a path to delete the blob.
//
// includeArchived selects GetItemIncludeDeleted over GetItem. With it false,
// "live" means exactly what the read gate (PLAN-2391 DR-13) means by it, and
// an archived parent reports Gone.
//
// Point-in-time, by construction: this is a read, and item deletion commits in
// its own transaction, so a caller that does unbounded work between this call
// and a write has a window. Transform closes its window with
// store.CreateAttachmentForLiveItem; derivation deliberately does not (see the
// comment on deriveThumbnails). Do not read a passing check here as a lock.
func (s *Server) resolveAttachmentParentItem(att *models.Attachment, includeArchived bool) (*models.Item, attachmentParentOutcome, error) {
	return s.resolveAttachmentParentItemQ(s.store.Q(), att, includeArchived)
}

// resolveAttachmentParentItemQ is resolveAttachmentParentItem parameterized
// over its executor — the cross-workspace copy's attachment authorizer runs
// it on the copy transaction's connection (BUG-2409); every other caller
// uses the pool wrapper above. One body, two executors, so the invariant
// stays in one place.
func (s *Server) resolveAttachmentParentItemQ(q store.Queryer, att *models.Attachment, includeArchived bool) (*models.Item, attachmentParentOutcome, error) {
	if att.ItemID == nil || *att.ItemID == "" {
		return nil, attachmentParentOrphan, nil
	}

	var (
		item *models.Item
		err  error
	)
	if includeArchived {
		item, err = s.store.GetItemIncludeDeletedQ(q, *att.ItemID)
	} else {
		item, err = s.store.GetItemQ(q, *att.ItemID)
	}
	if err != nil {
		return nil, attachmentParentGone, err
	}
	if item == nil {
		return nil, attachmentParentGone, nil
	}
	// Workspace identity is checked HERE, ahead of every caller's own
	// visibility/permission logic, because none of those catch a foreign
	// parent on their own: checkItemVisible admits any collection id when
	// the caller is unrestricted, and both the item-grant lookup and
	// requireEditPermission match by item id. Without this guard a grant on
	// a foreign item would authorize acting on THIS workspace's bytes.
	if item.WorkspaceID != att.WorkspaceID {
		return item, attachmentParentForeign, nil
	}
	return item, attachmentParentOK, nil
}

// attachmentCallerIsRestricted reports whether the caller's access to the
// workspace is narrowed to specific collections or items rather than being
// full workspace access.
//
// It is the orphan-row gate (PLAN-2382 DR-4), shared by the read, transform
// and delete paths. An orphan attachment (item_id IS NULL) belongs to no
// collection, so collection visibility cannot gate it — and the storage
// LISTING already hides orphans from restricted members. Without this, a
// restricted member who guesses an orphan's UUID gets confirmation it exists,
// which is the same disclosure the item-bound paths refuse.
//
// Callers MUST apply it AHEAD of any role gate that would answer 403: a 403
// reached only for rows that exist is itself the oracle.
func (s *Server) attachmentCallerIsRestricted(r *http.Request, workspaceID string) (bool, error) {
	return s.attachmentCallerIsRestrictedQ(s.store.Q(), r, workspaceID)
}

// attachmentCallerIsRestrictedQ is attachmentCallerIsRestricted
// parameterized over its executor (see resolveAttachmentParentItemQ).
func (s *Server) attachmentCallerIsRestrictedQ(q store.Queryer, r *http.Request, workspaceID string) (bool, error) {
	fullCollIDs, grantedItemIDs, err := s.guestResourceFilterCoreQ(q, r, workspaceID, false)
	if err != nil {
		return false, err
	}
	return fullCollIDs != nil || grantedItemIDs != nil, nil
}

// attachmentCopyAuthorizer builds the per-row visibility check the
// cross-workspace copy planner applies to every attachment reference it
// resolves in the SOURCE workspace (TASK-2408 / BUG-2407).
//
// It is the read path's rule, not a weaker cousin of it: the same
// resolveAttachmentParentItem outcomes, the same includeArchived=false
// (DR-13 — an archived parent is not readable, so it is not copyable), the
// same checkItemVisible, and the same orphan gate in the same ORDER, ahead
// of the role check. handleGetAttachment is the sibling to compare against
// line for line; the only difference is the shape of the denial, and that
// difference is forced: this one has no response to write, so it answers
// false and the planner turns that into "unresolvable".
//
// workspaceID is the SOURCE workspace, which is also the request's own
// workspace — the planner never resolves anything outside it, so
// guestResourceFilter and workspaceRole(r), both of which describe the
// caller's standing in the request workspace, are the right inputs. The
// destination side is authorized separately and earlier, by the ladder in
// resolveAuthorizedCopy.
//
// MEMOIZED PER PARENT ITEM, which is not an optimization detail but the
// difference between one query and N. With workspaceID and the caller
// fixed, the verdict is a pure function of the row's item_id: nothing
// below reads any other column. So the ordinary body — one item's images
// plus two thumbnail variants each — costs ONE item load and ONE
// visibility query instead of three per image, and the planner cannot turn
// a long reference list into an N+1 storm inside the copy's transaction,
// where both workspace advisory locks are held (Codex round 4). The
// restricted-ness verdict, a membership query used only by the orphan
// branch, is resolved once for the same reason. Errors are memoized too: a
// database that is failing should be asked once, not once per row.
//
// The memo therefore pins the caller's ACLs for the duration of ONE
// planner pass, which is the same point-in-time semantics the rest of this
// file has (resolveAttachmentParentItem's "do not read a passing check as
// a lock") and the same the plan itself has (PlanAttachmentCopy's
// STALENESS CONTRACT): a grant revoked mid-pass may not be observed by the
// remaining rows. Nothing is cached ACROSS requests — the closure is built
// per call of resolveAuthorizedCopy, so the preflight and the copy each
// re-derive every verdict from scratch.
//
// RESIDUAL: the query count is still linear in the number of DISTINCT
// parent items referenced. Bounding that would mean batching the item
// loads, which the per-row callback shape cannot express; it is bounded in
// practice by the attachments that actually exist in the source workspace,
// since an unresolvable reference never reaches this function at all.
//
// TRANSACTION-BOUND READS (BUG-2409). Every read below runs on the Queryer
// the PLANNER passes in — the pool on the preflight path, the copy's own
// in-flight transaction on the mutating path. That matters because the
// mutating planner runs inside a transaction holding advisory locks on
// BOTH workspaces: a read routed through the connection pool there could
// wait for a free connection while every pooled connection is itself
// occupied by a copy waiting on those locks — starvation presenting as a
// hang. On the transaction's own connection the reads cannot wait on the
// pool at all. The memo is what keeps the read count bounded rather than
// per-row; the q-threading is what keeps the bounded reads off the pool.
// NOTE the memo is keyed per parent item, not per (q, item) — sound
// because one closure only ever sees one executor: it is built per
// resolveAuthorizedCopy call and consumed by exactly one planner pass.
func (s *Server) attachmentCopyAuthorizer(r *http.Request, workspaceID string) store.AttachmentAuthorizer {
	type verdict struct {
		allowed bool
		err     error
	}
	var (
		restricted      bool
		restrictedKnown bool
		byParentItem    = map[string]verdict{}
	)

	decide := func(q store.Queryer, att models.Attachment) (bool, error) {
		item, outcome, err := s.resolveAttachmentParentItemQ(q, &att, false)
		if err != nil {
			return false, err
		}
		switch outcome {
		case attachmentParentOK:
			return s.checkItemVisibleQ(q, workspaceID, item, currentUser(r), workspaceRole(r), isBearerAuth(r))
		case attachmentParentOrphan:
			// The orphan rule (PLAN-2382 DR-4), and the restriction check
			// BEFORE the role check for the reason every sibling path
			// documents: a denial reachable only for rows that exist is
			// itself the oracle. Here both denials collapse into the same
			// "unresolvable" count anyway, but keeping the order identical
			// to the read path is what makes the two comparable.
			if !restrictedKnown {
				isRestricted, err := s.attachmentCallerIsRestrictedQ(q, r, workspaceID)
				if err != nil {
					return false, err
				}
				restricted = isRestricted
				restrictedKnown = true
			}
			if restricted {
				return false, nil
			}
			return requireRole(r, "viewer"), nil
		default: // Gone (missing, hard-deleted, or ARCHIVED parent), Foreign
			return false, nil
		}
	}

	return func(q store.Queryer, att models.Attachment) (bool, error) {
		// Defense in depth: the planner scopes every query to the source
		// workspace already, so this cannot fire today. It costs nothing
		// and means the authorizer is safe to hand to any future caller
		// without re-deriving that guarantee.
		//
		// Checked BEFORE the memo, because the memo is keyed by item_id
		// alone and is only sound while every row belongs to workspaceID.
		if att.WorkspaceID != workspaceID {
			return false, nil
		}

		// Orphans are not memoized: they have no item_id to key on, and
		// their branch runs no per-row query anyway.
		if att.ItemID == nil || *att.ItemID == "" {
			return decide(q, att)
		}
		if v, ok := byParentItem[*att.ItemID]; ok {
			return v.allowed, v.err
		}
		allowed, err := decide(q, att)
		byParentItem[*att.ItemID] = verdict{allowed: allowed, err: err}
		return allowed, err
	}
}
