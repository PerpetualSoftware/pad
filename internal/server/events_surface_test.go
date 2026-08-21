package server

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/events"
)

// TestDerivedSSENamesMatchTheLegacyWireVocabulary is the compat guard for the
// rewire: deriving the SSE names from the events/1 taxonomy must reproduce the
// strings the bus already published, byte for byte.
//
// This is what makes the rewire a REFACTOR rather than a wire change. Every
// live client — the web app's SSE handler, delta-sync's cursor logic — matches
// on these literals, so a derivation that quietly produced "item.created" or
// "item_deleted" would break the UI while every Go test still passed.
//
// THE EXPECTED SIDE IS A LITERAL COPY OF THE CLIENT'S STRINGS, not anything
// the Go side derives, and that is the whole design of this test. Comparing
// the derivation against events.* would pass for a coordinated rename of the
// taxonomy AND the constants — which is exactly the change that breaks the
// browser, because the client is pinned to the STRINGS.
//
// Source of truth for the wanted column: ITEM_EVENTS and the bulk listener in
// web/src/lib/services/sse.svelte.ts (see also the `items_bulk_updated`
// listener registered separately around line 270). Same disagree-with-the-
// table principle as TestCanonicalEventsAreFullyDeclared, one layer out: this
// file must be edited by hand when the wire vocabulary intentionally changes,
// and that edit is the moment someone has to go change the client too.
//
// events.* is asserted alongside as a second leg, so a drift between the Go
// constants and the client's strings is attributed rather than just reported.
func TestDerivedSSENamesMatchTheLegacyWireVocabulary(t *testing.T) {
	for _, tc := range []struct {
		name       string
		derived    string
		clientWire string
		legacy     string
	}{
		{"item created", sseItemCreated, "item_created", events.ItemCreated},
		{"item updated", sseItemUpdated, "item_updated", events.ItemUpdated},
		{"item moved surfaces as updated", sseItemMoved, "item_updated", events.ItemUpdated},
		{"item deleted surfaces as archived", sseItemArchived, "item_archived", events.ItemArchived},
		{"item restored", sseItemRestored, "item_restored", events.ItemRestored},
		{"bulk", sseItemsBulk, "items_bulk_updated", events.ItemsBulkUpdated},
		{"comment created", sseCommentCreated, "comment_created", events.CommentCreated},
		{"comment updated", sseCommentUpdated, "comment_updated", events.CommentUpdated},
	} {
		if tc.derived != tc.clientWire {
			t.Errorf("%s: derived SSE name = %q, but web/src/lib/services/sse.svelte.ts listens for %q — the browser would stop receiving this event",
				tc.name, tc.derived, tc.clientWire)
		}
		if tc.legacy != tc.clientWire {
			t.Errorf("%s: events.* constant = %q, but the client listens for %q — the Go constants drifted from the client, independently of the derivation",
				tc.name, tc.legacy, tc.clientWire)
		}
	}
}
