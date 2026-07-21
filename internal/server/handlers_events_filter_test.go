package server

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/events"
)

// TestSSEEventVisibleFor covers the visibility matrix:
//   - itemless batch events (items_bulk_updated, TASK-1668) are suppressed for
//     item-grant-only subscribers (would leak op/count/timing for hidden items);
//   - collection_updated (BUG-2265) matches on the STABLE collection ID, not the
//     mutable slug, so a replayed rename event whose OLD slug was re-owned by a
//     DIFFERENT collection can't pass visibility for a collection the subscriber
//     can't actually see.
func TestSSEEventVisibleFor(t *testing.T) {
	const tasksID = "coll-tasks"
	const secretsID = "coll-secrets"
	const otherID = "coll-other"

	bulk := func(coll string) events.Event {
		return events.Event{Type: events.ItemsBulkUpdated, Collection: coll, Op: "archive", Count: 3}
	}
	perItem := func(coll, itemID string) events.Event {
		return events.Event{Type: events.ItemUpdated, Collection: coll, ItemID: itemID}
	}
	collUpd := func(id, slug string) events.Event {
		return events.Event{Type: events.CollectionUpdated, CollectionID: id, Collection: slug}
	}
	collMigrated := func(id, slug string) events.Event {
		return events.Event{Type: events.CollectionUpdated, CollectionID: id, Collection: slug, ItemsChanged: true}
	}
	collRenamed := func(id, oldSlug, newSlug string) events.Event {
		return events.Event{Type: events.CollectionUpdated, CollectionID: id, Collection: oldSlug, NewSlug: newSlug}
	}

	allAccess := sseVisibility{visibleSlugSet: nil}

	fullColl := sseVisibility{
		visibleSlugSet:   map[string]bool{"tasks": true},
		visibleCollIDSet: map[string]bool{tasksID: true},
		grantedItemSet:   map[string]bool{"item-1": true},
		fullCollSet:      map[string]bool{"tasks": true},
	}

	itemGrantOnly := sseVisibility{
		visibleSlugSet:   map[string]bool{"tasks": true},
		visibleCollIDSet: map[string]bool{tasksID: true},
		grantedItemSet:   map[string]bool{"item-1": true},
		fullCollSet:      map[string]bool{}, // no full access to tasks
		isGuest:          true,
	}

	// A subscriber that can see the collection whose CURRENT slug is "tasks",
	// but that collection's id is `otherID` — a DIFFERENT collection re-owns the
	// slug "tasks" after the original was renamed away. Slug matching would
	// wrongly deliver an event for the original (tasksID); ID matching drops it.
	slugReuse := sseVisibility{
		visibleSlugSet:   map[string]bool{"tasks": true},
		visibleCollIDSet: map[string]bool{otherID: true},
	}

	cases := []struct {
		name string
		vis  sseVisibility
		ev   events.Event
		want bool
	}{
		// items_bulk_updated (slug-based, unchanged).
		{"all-access sees bulk", allAccess, bulk("tasks"), true},
		{"full-collection access sees bulk", fullColl, bulk("tasks"), true},
		{"item-grant-only suppressed for itemless bulk", itemGrantOnly, bulk("tasks"), false},
		{"item-grant-only suppressed for bulk in invisible collection", itemGrantOnly, bulk("secrets"), false},
		{"item-grant-only still gets granted per-item event", itemGrantOnly, perItem("tasks", "item-1"), true},
		{"item-grant-only denied non-granted per-item event", itemGrantOnly, perItem("tasks", "item-2"), false},
		{"any subscriber denied bulk in unseen collection", fullColl, bulk("other"), false},

		// collection_updated matches by STABLE id (BUG-2265). Delivered to
		// anyone who can see the collection (full OR item-grant), since it's
		// sanitized; denied for a collection the subscriber can't see by id.
		{"all-access gets collection.updated", allAccess, collUpd(tasksID, "tasks"), true},
		{"full access gets collection.updated by id", fullColl, collUpd(tasksID, "tasks"), true},
		{"item-grant-only gets collection.updated by id", itemGrantOnly, collUpd(tasksID, "tasks"), true},
		{"item-grant-only denied collection.updated for unseen id", itemGrantOnly, collUpd(secretsID, "secrets"), false},
		// Pattern A: the migration-reconcile variant reaches item-grant editors.
		{"item-grant-only gets migration collection.updated by id", itemGrantOnly, collMigrated(tasksID, "tasks"), true},
		{"item-grant-only denied migration collection.updated for unseen id", itemGrantOnly, collMigrated(secretsID, "secrets"), false},
		// Rename: identity is the id (unchanged by a rename), so a subscriber who
		// can see the collection receives it regardless of which slug they know.
		{"rename delivered by id", fullColl, collRenamed(tasksID, "tasks", "renamed-tasks"), true},

		// SLUG-REUSE LEAK FIX (Codex round 7): the subscriber sees a DIFFERENT
		// collection (otherID) that now owns slug "tasks"; a replayed event for
		// the ORIGINAL collection (tasksID) must be DROPPED even though its slug
		// is in visibleSlugSet — otherwise it misroutes / leaks the new slug.
		{"slug-reuse: event for original id dropped despite visible slug", slugReuse, collUpd(tasksID, "tasks"), false},
		{"slug-reuse: rename for original id dropped (no new-slug leak)", slugReuse, collRenamed(tasksID, "tasks", "secret-new"), false},
		{"slug-reuse: event for the re-owning id is delivered", slugReuse, collUpd(otherID, "tasks"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sseEventVisibleFor(tc.vis, "user-x", tc.ev); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
