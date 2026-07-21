package server

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/events"
)

// TestSSEEventVisibleFor covers the visibility matrix, with focus on the
// itemless batch event (items_bulk_updated, TASK-1668): an item-grant-
// only subscriber must NOT receive a collection-scoped event that carries
// no item ID, since it can't be item-filtered and would leak op/count/
// timing for items they can't see.
func TestSSEEventVisibleFor(t *testing.T) {
	bulk := func(coll string) events.Event {
		return events.Event{Type: events.ItemsBulkUpdated, Collection: coll, Op: "archive", Count: 3}
	}
	perItem := func(coll, itemID string) events.Event {
		return events.Event{Type: events.ItemUpdated, Collection: coll, ItemID: itemID}
	}
	collUpdated := func(coll string) events.Event {
		return events.Event{Type: events.CollectionUpdated, Collection: coll}
	}
	collRenamed := func(oldSlug, newSlug string) events.Event {
		return events.Event{Type: events.CollectionUpdated, Collection: oldSlug, NewSlug: newSlug}
	}

	// A subscriber that revalidated AFTER a rename knows only the NEW slug.
	newSlugOnly := sseVisibility{
		visibleSlugSet: map[string]bool{"renamed-tasks": true},
	}

	allAccess := sseVisibility{visibleSlugSet: nil}

	fullColl := sseVisibility{
		visibleSlugSet: map[string]bool{"tasks": true},
		grantedItemSet: map[string]bool{"item-1": true},
		fullCollSet:    map[string]bool{"tasks": true},
	}

	itemGrantOnly := sseVisibility{
		visibleSlugSet: map[string]bool{"tasks": true},
		grantedItemSet: map[string]bool{"item-1": true},
		fullCollSet:    map[string]bool{}, // no full access to tasks
		isGuest:        true,
	}

	cases := []struct {
		name string
		vis  sseVisibility
		ev   events.Event
		want bool
	}{
		{"all-access sees bulk", allAccess, bulk("tasks"), true},
		{"full-collection access sees bulk", fullColl, bulk("tasks"), true},
		{"item-grant-only suppressed for itemless bulk", itemGrantOnly, bulk("tasks"), false},
		{"item-grant-only suppressed for bulk in invisible collection", itemGrantOnly, bulk("secrets"), false},
		{"item-grant-only still gets granted per-item event", itemGrantOnly, perItem("tasks", "item-1"), true},
		{"item-grant-only denied non-granted per-item event", itemGrantOnly, perItem("tasks", "item-2"), false},
		{"any subscriber denied bulk in unseen collection", fullColl, bulk("other"), false},
		// BUG-2265: collection.updated is itemless but leak-free (only the
		// collection slug), so item-grant subscribers DO receive it for a
		// visible collection — but still not for an invisible one.
		{"item-grant-only gets collection.updated for visible collection", itemGrantOnly, collUpdated("tasks"), true},
		{"item-grant-only denied collection.updated for invisible collection", itemGrantOnly, collUpdated("secrets"), false},
		{"full-collection access gets collection.updated", fullColl, collUpdated("tasks"), true},
		{"all-access gets collection.updated", allAccess, collUpdated("tasks"), true},
		// Rename: a subscriber that only knows the NEW slug (revalidated after
		// the rename) still receives the event routed by the OLD slug, so it
		// learns the mapping (Codex round 2).
		{"rename delivered when only new slug is visible", newSlugOnly, collRenamed("tasks", "renamed-tasks"), true},
		{"rename still dropped when neither slug is visible", newSlugOnly, collRenamed("tasks", "other-new"), false},
		{"rename delivered when only old slug is visible", fullColl, collRenamed("tasks", "renamed-tasks"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sseEventVisibleFor(tc.vis, "user-x", tc.ev); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
