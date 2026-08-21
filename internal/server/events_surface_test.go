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
// The assertion runs against events.* rather than against fresh literals on
// purpose: those constants are what the CLIENTS are pinned to, so comparing
// against them is comparing against the thing that actually has to hold.
func TestDerivedSSENamesMatchTheLegacyWireVocabulary(t *testing.T) {
	for _, tc := range []struct {
		name    string
		derived string
		legacy  string
	}{
		{"item created", sseItemCreated, events.ItemCreated},
		{"item updated", sseItemUpdated, events.ItemUpdated},
		{"item moved surfaces as updated", sseItemMoved, events.ItemUpdated},
		{"item deleted surfaces as archived", sseItemArchived, events.ItemArchived},
		{"item restored", sseItemRestored, events.ItemRestored},
		{"bulk", sseItemsBulk, events.ItemsBulkUpdated},
		{"comment created", sseCommentCreated, events.CommentCreated},
		{"comment updated", sseCommentUpdated, events.CommentUpdated},
	} {
		if tc.derived != tc.legacy {
			t.Errorf("%s: derived SSE name = %q, legacy wire name = %q — clients match the legacy string",
				tc.name, tc.derived, tc.legacy)
		}
		if tc.derived == "" {
			t.Errorf("%s: derived SSE name is empty", tc.name)
		}
	}
}
