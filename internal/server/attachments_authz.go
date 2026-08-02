package server

import (
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/models"
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
	if att.ItemID == nil || *att.ItemID == "" {
		return nil, attachmentParentOrphan, nil
	}

	var (
		item *models.Item
		err  error
	)
	if includeArchived {
		item, err = s.store.GetItemIncludeDeleted(*att.ItemID)
	} else {
		item, err = s.store.GetItem(*att.ItemID)
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
	fullCollIDs, grantedItemIDs, err := s.guestResourceFilter(r, workspaceID)
	if err != nil {
		return false, err
	}
	return fullCollIDs != nil || grantedItemIDs != nil, nil
}
