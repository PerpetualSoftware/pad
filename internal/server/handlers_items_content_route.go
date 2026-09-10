package server

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/PerpetualSoftware/pad/internal/collab"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// Write-first-apply-second routing for a content PATCH (PLAN-2975, BUG-2840 half A).
//
// The defect this closes: the applier path used to push content into the live Y.Doc
// BEFORE the row write, so a refused write answered 4xx while the collaborative
// document had already moved. The refusal was true of the row and false of the
// document, and the next ?source=collab-snapshot flush carried the refused content
// into items.content.
//
// The ordering is possible because HasElectableApplier (TASK-2987) answers "would
// this route through an applier?" without applying. It is a HINT — an apply can
// still fail after a yes — which is why contentRouteHandled exists: an apply that
// fails after the row write has committed answers the typed content_not_applied 409
// rather than a 200 that would read as success.

type contentRoute int

const (
	// contentRouteHandled — a response has already been written (a typed refusal,
	// content_not_applied, or room_settling). The caller must return immediately.
	contentRouteHandled contentRoute = iota
	// contentRouteApplierWrote — the row write committed and the apply succeeded.
	contentRouteApplierWrote
	// contentRouteDirectWrote — PruneAndApply ran the full write for a room with no
	// live writer.
	contentRouteDirectWrote
	// contentRouteFallThrough — nothing was written and no live writer holds a
	// diverging document, so the caller's ordinary row write should carry the
	// content as it always did.
	contentRouteFallThrough
)

// applierSettleBudget bounds the wait for a room that is neither settled enough to
// elect an applier nor empty enough to write directly.
//
// RECEIPT. Measured on this deployment's shape (TASK-2989): dial the collab socket
// through the real HTTP/WS handler chain against the real SQLite store, then poll
// HasElectableApplier until it answers true. n=5 per bucket, loopback:
//
//	rows=0     mean 3.23ms   max 5.60ms
//	rows=10    mean 2.45ms   max 3.08ms
//	rows=100   mean 2.85ms   max 3.81ms
//	rows=1000  mean 6.33ms   max 8.51ms
//	rows=5000  mean 38.75ms  max 46.41ms
//
// Flat at ~3ms up to 100 op-log rows (fixed handshake cost), then roughly linear at
// about 7.7µs/row. Worst single observation across all 25 runs: 46.41ms. The budget
// is ~10x that, and the headroom is spent deliberately on what the measurement does
// NOT cover: Postgres rather than SQLite, a real browser across a real network, and
// a store under concurrent load.
//
// It does NOT cover a conn that has not dialled yet, or one wedged behind a stalled
// write — those are meant to reach the room_settling refusal rather than be waited
// out. Error direction is safe in both senses: too long merely delays a request that
// was going to be refused, and too short converts a room that would have settled
// into a retryable refusal. Nothing is written on either side of the bound.
const applierSettleBudget = 500 * time.Millisecond

// applierSettlePoll is the re-decision interval inside that budget. HasElectableApplier
// is two uncontended mutex acquisitions, so polling costs nothing measurable; 10ms
// keeps the common case (a conn that anchors in ~3ms) from paying a full extra tick.
const applierSettlePoll = 10 * time.Millisecond

// routeContentUpdate decides which ordering a content PATCH takes and executes it.
//
// It owns the re-decision deliberately. The predecessor helper retried
// ErrRoomActiveDuringPrune INSIDE applyContentViaCollab and re-called
// ApplyExternalContent, which could succeed through a freshly-joined applier and
// return nil — after which the handler's row write still ran last, reproducing the
// very defect this change removes. Re-deciding HERE, before anything is written, is
// what makes that impossible rather than unlikely.
func (s *Server) routeContentUpdate(
	w http.ResponseWriter,
	r *http.Request,
	item *models.Item,
	input *models.ItemUpdate,
	openChildrenPrecheck func(*sql.Tx, *models.Item) error,
	parentLink *store.ParentLinkUpdate,
	content string,
) (contentRoute, *models.Item, error) {
	deadline := time.Now().Add(applierSettleBudget)

	for {
		if s.collab.HasElectableApplier(item.ID) {
			return s.applierFirstWrite(w, item, input, openChildrenPrecheck, parentLink, content)
		}

		// No electable applier. PruneAndApply is the authority on whether the room
		// is genuinely peerless: its scan blocks on ANY conn with canWrite, which is
		// a wider set than election's (election also demands unfrozen and
		// replay-done). Going straight to it — rather than through
		// ApplyExternalContent — is what keeps a writer that anchors underneath us
		// from applying content ahead of the row write.
		var updated *models.Item
		paErr := s.collab.PruneAndApply(item.ID, func() error {
			precheck := composePruneWithPrecheck(s, item.ID, openChildrenPrecheck)
			u, uerr := s.store.UpdateItemWithParentLink(item.ID, *input, precheck, parentLink)
			if uerr != nil {
				return uerr
			}
			updated = u
			return nil
		})

		switch {
		case paErr == nil:
			return contentRouteDirectWrote, updated, nil

		case errors.Is(paErr, collab.ErrRoomActiveDuringPrune):
			// The standoff, or a writer that is about to become electable. Neither
			// path can run right now and NOTHING has been written — PruneAndApply
			// returns before it calls applyFn. Wait a bounded moment and re-decide.
			if time.Now().After(deadline) {
				slog.Info("collab: room neither electable nor peerless within the settle budget; refusing",
					"item_id", item.ID,
					"budget", applierSettleBudget,
				)
				writeRoomSettlingError(w, itemRefOrSlug(*item))
				return contentRouteHandled, nil, nil
			}
			time.Sleep(applierSettlePoll)
			continue

		default:
			if s.writeTypedItemRefusal(w, item, paErr) {
				return contentRouteHandled, nil, nil
			}
			slog.Warn("collab: direct-write path failed; falling through to the ordinary row write",
				"item_id", item.ID,
				"error", paErr,
			)
			return contentRouteFallThrough, nil, paErr
		}
	}
}

// applierFirstWrite is the reordered applier path: the row write commits WITHOUT
// content, then the apply runs against a committed row and no held transaction
// (PLAN-2975 decision 3).
func (s *Server) applierFirstWrite(
	w http.ResponseWriter,
	item *models.Item,
	input *models.ItemUpdate,
	openChildrenPrecheck func(*sql.Tx, *models.Item) error,
	parentLink *store.ParentLinkUpdate,
	content string,
) (contentRoute, *models.Item, error) {
	rowInput := *input
	// The content half travels through the applier, not the row. Every store content
	// write is gated on Content != nil, so a nil here means the row write touches
	// neither items.content nor the version chain's content bracket.
	rowInput.Content = nil

	updated, uerr := s.store.UpdateItemWithParentLink(item.ID, rowInput, openChildrenPrecheck, parentLink)
	if uerr != nil {
		// THE POINT OF THE WHOLE CHANGE: this refusal happens before
		// ApplyExternalContent is ever called, so there is no Y.Doc write, no
		// op-log row, and nothing for a later flush to carry.
		if s.writeTypedItemRefusal(w, item, uerr) {
			return contentRouteHandled, nil, nil
		}
		writeInternalError(w, uerr)
		return contentRouteHandled, nil, nil
	}

	if aerr := s.collab.ApplyExternalContent(item.ID, content); aerr != nil {
		if errors.Is(aerr, collab.ErrApplierAmbiguous) {
			// Untouched by this change, deliberately. A legacy round-trip caught by
			// a restore MIGHT have persisted; claiming "content was not applied"
			// would be a false statement, so it keeps its own retryable answer.
			writeError(w, http.StatusConflict, "applier_ambiguous",
				"A concurrent version restore made this edit's outcome ambiguous; please retry.")
			return contentRouteHandled, nil, nil
		}
		slog.Warn("collab: row write landed but the apply failed; answering content_not_applied",
			"item_id", item.ID,
			"error", aerr,
		)
		writeContentNotAppliedError(w, itemRefOrSlug(*item), landedFieldNames(input), updated.UpdatedAt, aerr.Error())
		return contentRouteHandled, nil, nil
	}

	return contentRouteApplierWrote, updated, nil
}

// composePruneWithPrecheck rides the op-log prune inside the write's own transaction
// (BUG-2840 half B) by composing it onto the precheck hook UpdateItemWithParentLink
// runs there, so a refusal from either rolls the prune back.
func composePruneWithPrecheck(s *Server, itemID string, inner func(*sql.Tx, *models.Item) error) func(*sql.Tx, *models.Item) error {
	return func(tx *sql.Tx, existing *models.Item) error {
		if inner != nil {
			if err := inner(tx, existing); err != nil {
				return err
			}
		}
		return s.store.PruneItemOpLogTx(tx, itemID)
	}
}

// writeTypedItemRefusal writes the structured refusal for any of the four typed,
// permanent failures store.UpdateItem can return, and reports whether it did.
//
// It exists because this handler's refusal set is a CLASS that has been under-counted
// three separate times (BUG-2804 and BUG-2833 each added an arm a previous unit had
// missed, and isDeterministicWriteFailure carries a comment saying so). One function
// consulted by every ordering is what stops the count drifting again: anyone adding a
// typed refusal to store.UpdateItem changes this and every path inherits it.
func (s *Server) writeTypedItemRefusal(w http.ResponseWriter, item *models.Item, err error) bool {
	if details, ok := asOpenChildrenGuardError(err); ok {
		writeOpenChildrenError(w, itemRefOrSlug(*item), details)
		return true
	}
	if conflict, ok := asUpdateConflictError(err); ok {
		writeUpdateConflictError(w, itemRefOrSlug(*item), conflict)
		return true
	}
	if writeItemRenameCascadeTooLarge(w, err) {
		return true
	}
	if writeInvalidItemTitle(w, err) {
		return true
	}
	return false
}

// landedFieldNames names what the row write actually stored, so a content_not_applied
// answer can tell the caller which half of its PATCH is done and must not be re-sent
// blind. Content is never listed: by construction it is the half that did not land.
func landedFieldNames(input *models.ItemUpdate) []string {
	var names []string
	if input.Title != nil {
		names = append(names, "title")
	}
	if input.Fields != nil {
		names = append(names, "fields")
	}
	for k := range input.FieldsPatch {
		names = append(names, "fields."+k)
	}
	if input.Tags != nil {
		names = append(names, "tags")
	}
	if input.Pinned != nil {
		names = append(names, "pinned")
	}
	if input.SortOrder != nil {
		names = append(names, "sort_order")
	}
	if input.ParentID != nil {
		names = append(names, "parent_id")
	}
	if input.AssignedUserID != nil || input.ClearAssignedUser {
		names = append(names, "assigned_user_id")
	}
	if input.AgentRoleID != nil || input.ClearAgentRole {
		names = append(names, "agent_role_id")
	}
	// Deterministic order: this is a wire value, and a set iterated in map order
	// would make the response non-reproducible for no reason.
	sort.Strings(names)
	return names
}
