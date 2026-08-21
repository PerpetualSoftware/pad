package server

import (
	"fmt"

	"github.com/PerpetualSoftware/pad/internal/kernelevents"
)

// The SSE wire names, DERIVED from the events/1 taxonomy rather than written
// out again here.
//
// SPEC-3 §"The choke point owns the canonical→surface name mapping" is the
// reason this file exists: SSE's snake_case vocabulary and the webhook
// dot-form vocabulary drifted because nothing tied them together, each being
// hand-passed at its own call sites. v1.5 pins what "derive" means — NAME
// derivation, not delivery path. Webhooks deliver through the outbox drain;
// SSE stays direct-published at the mutation site, because it carries
// request-scoped attribution (Actor / ActorName / Source) that a frozen outbox
// payload deliberately does not hold. What changes is only where the NAME
// comes from.
//
// DERIVED AT INIT, not per publish, and that is the whole safety argument. A
// canonical event with no SSE surface is a programming error at these call
// sites — every one of them is a compile-time constant — so the check belongs
// where a failure is impossible to miss and impossible to reach a user:
// process start. A per-request lookup would have to decide what to do with the
// "no surface" answer in a void helper, and every available answer (log and
// drop, publish under an empty name) is worse than not starting.
//
// Several canonical events derive the SAME name: status_changed and moved both
// surface as item_updated, because the SSE vocabulary is coarser than events/1
// and the UI has never distinguished them. The finer name is what the webhook
// wire and bindings receive.
var (
	sseItemCreated    = mustSurfaceSSE(kernelevents.ItemCreated)
	sseItemUpdated    = mustSurfaceSSE(kernelevents.ItemUpdated)
	sseItemMoved      = mustSurfaceSSE(kernelevents.ItemMoved)
	sseItemArchived   = mustSurfaceSSE(kernelevents.ItemDeleted)
	sseItemRestored   = mustSurfaceSSE(kernelevents.ItemRestored)
	sseItemsBulk      = mustSurfaceSSE(kernelevents.ItemBulkUpdated)
	sseCommentCreated = mustSurfaceSSE(kernelevents.CommentCreated)
	sseCommentUpdated = mustSurfaceSSE(kernelevents.CommentUpdated)
)

// mustSurfaceSSE resolves a canonical event's SSE wire name or panics.
//
// The panic is deliberate and it fires at package init, before the server
// listens: a canonical event that reaches here without an SSE surface means
// the taxonomy and this file disagree, and the only honest outcomes are
// "publish nothing" (silently breaking the live UI) or "refuse to start".
// Refusing to start is the one a person notices.
func mustSurfaceSSE(canonical string) string {
	name, ok := kernelevents.SurfaceSSE(canonical)
	if !ok {
		panic(fmt.Sprintf("kernelevents: %q has no SSE surface name", canonical))
	}
	return name
}
