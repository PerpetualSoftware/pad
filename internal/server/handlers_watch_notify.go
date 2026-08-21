package server

import (
	"fmt"

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
		s.watchEvents.Publish(watchevents.Notification{
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
		s.watchEvents.Publish(watchevents.Notification{
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
