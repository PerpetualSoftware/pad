package cli

import (
	"fmt"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// RenderRelationValue is the ONE place a hydrated `relation` value becomes text
// for a human (PLAN-2857 U6).
//
// A relation stores an item id, so the raw stored value is a UUID and reading
// it tells a person nothing. `relation_targets` already carries what is needed
// to render it, so this is a formatter over data the response holds — no
// lookup, and nothing here can fail.
//
//	resolved  -> "COLO-3 · Red"
//	id-only   -> "6f2c… (unavailable)"
//
// WHY "unavailable" AND NOT "deleted". An id-only entry has TWO causes that
// this layer cannot tell apart: the target is gone, or the requester may not
// see it. "(deleted)" would assert non-existence for both, which is false for
// half of them and is an existence claim the response does not support — the
// same defect codex round 12 found in the copy dialog, where the UI read
// `not_found` as "does not exist" when the server also emits it for a target
// the caller merely cannot see. The server works to make those two
// indistinguishable; the renderer must not undo that in the last inch.
func RenderRelationValue(target models.RelationTarget) string {
	if target.Ref == "" {
		return fmt.Sprintf("%s (unavailable)", target.ID)
	}
	if target.Title == "" {
		// Ref without a title should not happen — they come from the same row —
		// but rendering "COLO-3 · " would look like a bug in the data rather
		// than in this function.
		return target.Ref
	}
	return fmt.Sprintf("%s · %s", target.Ref, target.Title)
}
