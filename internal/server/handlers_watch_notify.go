package server

import (
	"fmt"
	"log/slog"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

// publishWatchNotifications inspects updated.LastMutation (TASK-2533,
// models.ItemMutationSignal — set race-free inside the SAME transaction
// that writes status_transitions / assigned_user_id, see
// internal/store/items.go) and publishes the corresponding
// watchevents.Notification(s): at most one status-change and one
// assignment notification, matching the two independent facets a single
// mutation signal can carry.
//
// No-op when s.watchEvents is nil (feature disabled) or LastMutation is
// nil (this particular mutation touched neither the done-field nor
// assigned_user_id — e.g. a title rename, a tag change, a content edit).
//
// Producer coverage (TASK-2533 audit — call this from every path that can
// produce a LastMutation signal):
//   - handleUpdateItem (single-item PATCH, including its collab sub-paths —
//     they all funnel through the same `updated` variable at the point
//     this is called)
//   - handleMoveItem (single-item move, status-override case)
//   - the bulk-items loop in handleBulkItems (covers every bulk op
//     variant — archive/restore/move/set-priority/tag/untag/assign — via
//     one call site, since LastMutation is simply nil for the ops that
//     don't touch status/assignment)
//   - version restore (handlers_item_versions.go) — called for
//     completeness; LastMutation is nil in practice there because restore
//     only ever rewrites content, never fields/assignment
//
// NOT called from (named bypasses, not silent gaps):
//   - import bundle (handlers_import_bundle.go) — bulk import writes
//     items directly via store.CreateItem, not through UpdateItem; a
//     freshly-imported item can't have a pre-existing watch anyway
//   - status_transitions backfill (BackfillStatusTransitions) — a one-time
//     historical reconciliation, not a live mutation
//   - workspace restore / purge — administrative, not a live human action
//
// Known limitation (flagged, not fixed, in Phase 1): unlike the SSE bulk path
// (publishBulkItemsEvent) and the outbox drain's batch fold, this does NOT
// collapse
// N per-item bulk mutations into one batch notification. A bulk mutation
// touching 50 watched items still surfaces 50 individual notifications
// to the plugin monitor in a burst — in tension with PLAN-2469's noise-
// discipline principle. Each notification is still correctly scoped by
// the caller's own watch filter (a fundamentally narrower audience than
// the workspace-wide SSE firehose the existing batching was built to
// protect), so this is a burst-noise concern, not a leak — deferred
// rather than building ad-hoc coalescing beyond what DOC-2479 specs.
//
// IDEA-2544 Phase 2 (TASK-2551) shrank the worst case: the motivating
// example was "bulk-assign 50 items to me", which sprayed 50
// addressed-to-you notifications at someone who had asked for none of
// them. Assignment no longer delivers that way, so the remaining burst
// requires 50 items the caller explicitly watched — noise they opted
// into, one notification per fact they asked to hear about.
func (s *Server) publishWatchNotifications(workspaceID string, updated *models.Item, actor, actorName string) {
	if s.watchEvents == nil || updated == nil || updated.LastMutation == nil {
		return
	}
	sig := updated.LastMutation

	if sig.StatusChanged {
		s.publishWatchNotification(watchevents.Notification{
			WorkspaceID:    workspaceID,
			ItemID:         updated.ID,
			CollectionID:   updated.CollectionID,
			ItemRef:        updated.Ref,
			Kind:           watchevents.KindStatusChange,
			Actor:          actor,
			ActorName:      actorName,
			Summary:        fmt.Sprintf("%s → %s", orNoneLabel(sig.FromStatus), orNoneLabel(sig.ToStatus)),
			StatusFieldKey: sig.StatusFieldKey,
			ToStatus:       sig.ToStatus,
		})
	}

	if sig.AssignmentChanged {
		summary := "unassigned"
		if sig.ToAssignedUserID != "" {
			name := updated.AssignedUserName
			if name == "" {
				name = sig.ToAssignedUserID
			}
			summary = fmt.Sprintf("assigned to %s", name)
		}
		s.publishWatchNotification(watchevents.Notification{
			WorkspaceID:    workspaceID,
			ItemID:         updated.ID,
			CollectionID:   updated.CollectionID,
			ItemRef:        updated.Ref,
			Kind:           watchevents.KindAssignment,
			Actor:          actor,
			ActorName:      actorName,
			Summary:        summary,
			AssignedUserID: sig.ToAssignedUserID,
		})
	}
}

// orNoneLabel renders an empty done-field value as "(none)" for a
// notification summary line, rather than an ugly bare arrow ("→ done").
func orNoneLabel(status string) string {
	if status == "" {
		return "(none)"
	}
	return status
}

// publishWatchNotification is the BEST-EFFORT publish path, and the one
// ruling behind every non-push producer (BUG-2699).
//
// Bus.Publish reports acceptance since BUG-2699, which raised the
// question of what each of the seven production call sites should DO
// with that answer. Six of them — comment created, comment reply, item
// created-with-assignee, comment-on-update, status change, assignment
// change — publish a notification LAYERED ON A DURABLE STORE WRITE THAT
// HAS ALREADY COMMITTED. The item exists, the comment exists, the
// activity row exists, and the SSE event carrying the same fact went out
// on a different bus. A watch notification that fails to publish costs a
// subscriber one line about a fact that is still fully recoverable by
// reading the item. Failing the caller's request over it would be the
// wrong trade in the other direction: a 500 on a PATCH that already
// committed tells the client its write failed when it did not.
//
// So these six discard the result — deliberately, and here rather than
// six times over.
//
// DISCARDED, BUT NOT UNOBSERVED, and this helper is what makes that true
// (codex round 10). An earlier version of this comment claimed both bus
// implementations already log a failed publish, so discarding cost no
// visibility. That is right for a transport failure and WRONG for the one
// case most likely to matter: ErrBusClosed returns without logging in
// either implementation, so a producer publishing into a bus closed during
// shutdown vanished in silence. The helper logs it here instead — once,
// at the layer that decided to ignore it.
//
// The SEVENTH site, handlePushToItem, does not use this helper and must
// not: a push has no durable backing at all (no inbox, nothing to read
// back), so a dropped publish loses the instruction outright and the
// caller has to hear about it. That asymmetry is enforced structurally —
// after BUG-2699 the push handler is the ONLY direct s.watchEvents.Publish
// call in this package besides the one just below, and that split is
// CHECKED rather than asserted: publish_sites_ruled_test.go enumerates
// the call sites and fails by name when a new producer publishes
// directly. A comment saying "don't do X" protects whoever reads it
// before doing X, which is not the person this needs protecting from.
//
// Also absorbs the nil-bus check every one of those sites was repeating.
func (s *Server) publishWatchNotification(n watchevents.Notification) {
	if s.watchEvents == nil {
		return
	}
	if err := s.watchEvents.Publish(n); err != nil {
		// Warn, not Error: the durable write this notification sits on top
		// of already committed, so nothing is lost that a reader cannot
		// recover from the item itself. An operator still wants to see it,
		// because a run of these means the bus is unhealthy.
		slog.Warn("watch notification not published; the underlying write still committed",
			"error", err, "kind", n.Kind, "item_ref", n.ItemRef)
	}
}
