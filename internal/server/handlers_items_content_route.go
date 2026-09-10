package server

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
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
//
// IT IS A RE-DECISION BUDGET, NOT A HARD REQUEST BOUND, and the difference is worth
// stating because the name suggests otherwise (codex round 1, this unit). The
// deadline is only consulted between attempts, and an attempt calls PruneAndApply,
// which can itself block on the per-item lock or on appendMu while a restore holds
// the room. A request can therefore exceed this budget under contention. What the
// budget bounds is how long the route keeps ASKING, not how long the request takes.
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
	var updated *models.Item
	outcome, paErr := settleContentRoute(
		r.Context(),
		func() bool { return s.collab.HasElectableApplier(item.ID) },
		func() error {
			return s.collab.PruneAndApply(item.ID, func() error {
				precheck := composePruneWithPrecheck(s, item.ID, openChildrenPrecheck)
				u, uerr := s.store.UpdateItemWithParentLink(item.ID, *input, precheck, parentLink)
				if uerr != nil {
					return uerr
				}
				updated = u
				return nil
			})
		},
		applierSettleBudget, applierSettlePoll,
	)

	switch outcome {
	case settleElectApplier:
		return s.applierFirstWrite(w, item, input, openChildrenPrecheck, parentLink, content)

	case settleDirectWrote:
		return contentRouteDirectWrote, updated, nil

	case settleUnsettled:
		slog.Info("collab: room neither electable nor peerless within the settle budget; refusing",
			"item_id", item.ID,
			"budget", applierSettleBudget,
		)
		writeRoomSettlingError(w, itemRefOrSlug(*item))
		return contentRouteHandled, nil, nil

	default: // settleDirectFailed
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

// settleOutcome is what one pass of the route decision concluded.
type settleOutcome int

const (
	// settleElectApplier — an applier is electable; take the reordered path.
	settleElectApplier settleOutcome = iota
	// settleDirectWrote — the room had no live writer and the direct write ran.
	settleDirectWrote
	// settleUnsettled — the budget expired with the room neither electable nor
	// peerless. NOTHING has been written.
	settleUnsettled
	// settleDirectFailed — the direct write itself failed; the error is returned.
	settleDirectFailed
)

// settleContentRoute is the route decision, extracted from its I/O so the budget
// expiry is testable without a seam into the conn anchoring machinery — which is
// where a test-only lever would otherwise have to reach, and which PLAN-2975 fences
// off as its own unit.
//
// The standoff it bounds is not a race the server can win by trying harder.
// PruneAndApply blocks on ANY conn with canWrite; election additionally requires the
// conn to be unfrozen and past its replay. A room whose only writer has joined and
// not yet anchored satisfies the first and fails the second, so neither path can run,
// and only that conn anchoring resolves it — which this request does not control.
//
// Re-deciding here, before anything is written, is also what closes the route-flip
// the predecessor had: retrying inside applyContentViaCollab re-called
// ApplyExternalContent, which could succeed through a freshly joined applier and let
// the row write run last after all.
func settleContentRoute(
	ctx context.Context,
	hasElectableApplier func() bool,
	tryDirectWrite func() error,
	budget, poll time.Duration,
) (settleOutcome, error) {
	deadline := time.Now().Add(budget)
	for {
		// The caller going away ends the wait immediately. This is the only NEW
		// blocking wait the reorder introduces — the rest of this path was already
		// context-blind on main and stays that way, deliberately, since threading a
		// context into the store and the applier round-trip is a change of a
		// different size (codex round 2). Bounding what this unit ADDED is the part
		// that belongs to this unit.
		if err := ctx.Err(); err != nil {
			return settleUnsettled, nil
		}
		if hasElectableApplier() {
			return settleElectApplier, nil
		}
		err := tryDirectWrite()
		switch {
		case err == nil:
			return settleDirectWrote, nil
		case errors.Is(err, collab.ErrRoomActiveDuringPrune):
			// A writer exists but is not electable, or is about to become so.
			// PruneAndApply returns this BEFORE it calls applyFn, so nothing has
			// been written and waiting is free of consequence.
			if time.Now().After(deadline) {
				return settleUnsettled, nil
			}
			select {
			case <-time.After(poll):
			case <-ctx.Done():
				return settleUnsettled, nil
			}
		default:
			return settleDirectFailed, err
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
		outcome := classifyApplyOutcome(aerr)
		slog.Warn("collab: row write landed but the apply did not confirm; answering content_not_applied",
			"item_id", item.ID,
			"content_outcome", outcome,
			"error", aerr,
		)
		writeContentNotAppliedError(w, itemRefOrSlug(*item), landedFieldNames(input), updated.UpdatedAt, outcome, aerr.Error())
		return contentRouteHandled, nil, nil
	}

	return contentRouteApplierWrote, updated, nil
}

// classifyApplyOutcome decides whether a failed apply DEMONSTRABLY left the content
// out of the collaborative document, or merely failed to confirm.
//
// It is written as a WHITELIST — unknown unless proven otherwise — and that is the
// correction from codex round 2 rather than the original shape. The first version
// asked whether the error was a timeout and called everything else "not applied", on
// the reasoning that ApplyExternalContent's own anyWriteSucceeded tracking already
// separated the two. That reasoning was one file short of true: anyWriteSucceeded is
// tracked PER ELECTION, and two paths escaped it — a restore storm returning after
// several elections that may each have sent a request, and a registerPendingAck
// failure on a retry attempt returning a raw error after an earlier attempt had
// already put bytes on the wire. Both would have answered "content_landed: false"
// about content that may well have landed.
//
// The storm path is fixed at its source (ApplyExternalContent now carries sentAny
// across restarts). This function covers the rest by construction: only the two
// sentinels that MEAN nothing reached a peer are allowed to make the claim, and every
// other error — sentinel, wrapped, or entirely unforeseen — is unknown. A new error
// added upstream therefore degrades to the honest answer rather than to a false one.
func classifyApplyOutcome(err error) string {
	switch {
	case errors.Is(err, collab.ErrNoActiveRoom), errors.Is(err, collab.ErrNoApplierAvailable):
		return contentOutcomeNotApplied
	default:
		return contentOutcomeUnknown
	}
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
	// A nil error is not a refusal. The typed arms below all tolerate nil (errors.As
	// and the write helpers check), but the string-matching arm dereferences, so
	// without this a caller asking "is this a refusal?" about success panics. Caught
	// by the nil control leg in TestWriteTypedItemRefusalIncludesTitleRefusal the
	// moment that arm was added — which is what the control is for.
	if err == nil {
		return false
	}
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
	// The FIFTH arm, and the one this function was built without — found by codex
	// round 5 as a REGRESSION, not a gap. The ordinary path maps a UNIQUE-constraint
	// race to a 409 (a concurrent update that passes checkUniqueFields and then hits
	// the partial unique index on invocation_slug), and before the reorder the
	// applier path's row write ran through that block and inherited it. Routing the
	// applier path through a helper built from "the four typed refusals" turned a
	// benign race into a 500 on that path only.
	//
	// The irony is the lesson: this function exists BECAUSE this handler's refusal
	// set has been under-counted three times, and building it I under-counted the set
	// again — by taking the count from the typed errors rather than from the block
	// that actually answers them. The population is what the ordinary path maps, not
	// what has a Go type.
	//
	// It stays a string match here for the same reason it is one there: the store
	// returns the driver's error verbatim and SQLite and Postgres word it
	// differently. Kept LAST, after every typed arm, because a substring match can
	// swallow a typed refusal whose message happens to contain the text.
	if strings.Contains(err.Error(), "UNIQUE constraint") || strings.Contains(err.Error(), "duplicate key") {
		writeError(w, http.StatusConflict, "conflict",
			"An item conflicts with an existing record (duplicate slug, title, or invocation slug)")
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
