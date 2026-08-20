// Package kernelevents defines the events/1 contract surface — the closed set
// of canonical event names, the envelope every event carries, and the mapping
// from a canonical name onto the per-surface wire vocabularies.
//
// This package is the contract, not the plumbing. SPEC-3 (DOC-2653) calls the
// taxonomy a PUBLIC contract rather than internal wiring: connected apps
// consume these names through webhooks, so a name here is as load-bearing as
// an HTTP route. Storage lives in the store's event_outbox table; dispatch
// lives in the server's drain loop; both refer back to the names defined here.
//
// Two rules from SPEC-3 shape everything below:
//
//   - THE CLOSURE RULE. A mutation whose event name is not in the canonical
//     set emits nothing in v1. The set grows additively with the contract
//     version, so silence about a mutation type is a versioned fact rather
//     than an oversight. This is why Canonical is an explicit list and why
//     IsCanonical exists — a typo'd name must fail loudly at the choke point
//     rather than travel to a consumer that will never recognize it.
//
//   - THE CHOKE POINT OWNS THE MAPPING. Before this package, SSE used
//     snake_case names published from one set of hand-calls and webhooks used
//     dot-form string literals passed at a different set of hand-calls; the
//     two vocabularies drifted because nothing tied them together. Under
//     events/1 both surfaces derive from one canonical name (SPEC-3 v1.1).
package kernelevents

// Canonical event names — the events/1 set (SPEC-3 §Taxonomy, v1.1).
//
// item.restored and item.bulk_updated were admitted in v1.1 during TASK-2658
// recon: restore is a live first-class mutation whose silence would let an
// item reappear unobserved, and the batch event preserves TASK-1668's
// anti-flood decision for lane-wide mutations.
const (
	// ItemCreated fires on item creation. Payload carries the post-create
	// snapshot.
	ItemCreated = "item.created"

	// ItemUpdated fires on a field/content mutation that is not a status
	// change. Payload carries the post-update snapshot.
	ItemUpdated = "item.updated"

	// ItemStatusChanged is first-class rather than inferred from
	// ItemUpdated, because most bindings want exactly this — including
	// terminality transitions. Payload carries the post-change snapshot plus
	// the prior_status envelope pseudo-field.
	ItemStatusChanged = "item.status_changed"

	// ItemMoved fires when an item changes collection. Payload carries the
	// post-move snapshot.
	ItemMoved = "item.moved"

	// ItemDeleted is pinned to the SOFT-DELETE/archive mutation (SPEC-3
	// v1.1) — this codebase has no hard item delete on the request path.
	// Payload carries the final pre-archive snapshot, which is what keeps a
	// deleted item addressable by binding predicates.
	ItemDeleted = "item.deleted"

	// ItemRestored fires on un-archive. Payload carries the post-restore
	// snapshot.
	ItemRestored = "item.restored"

	// ItemBulkUpdated is the canonical BATCH event: one wire event for a
	// whole lane-wide mutation. Binding evaluation is still per-member —
	// the dispatcher runs item-level selectors against each member snapshot
	// — so bindings never miss a bulk mutation; only wire delivery is
	// batched.
	ItemBulkUpdated = "item.bulk_updated"

	CommentCreated = "comment.created"
	CommentUpdated = "comment.updated"

	AttachmentAdded = "attachment.added"

	MemberJoined = "member.joined"

	PackInstalled = "pack.installed"
	PackUpgraded  = "pack.upgraded"
	PackDisabled  = "pack.disabled"
)

// Subject kinds — what an event is about. Stored alongside the event so a
// consumer (and the drain loop) can route without parsing the name.
const (
	SubjectItem       = "item"
	SubjectItemBatch  = "item_batch"
	SubjectComment    = "comment"
	SubjectAttachment = "attachment"
	SubjectMember     = "member"
	SubjectPack       = "pack"
)

// canonical is the closed events/1 set mapped to its subject kind.
//
// A map rather than a slice because every consumer of this list is asking a
// membership question — the closure rule is enforced by lookup, and the
// subject kind falls out of the same entry instead of living in a second
// table that could disagree with this one.
var canonical = map[string]string{
	ItemCreated:       SubjectItem,
	ItemUpdated:       SubjectItem,
	ItemStatusChanged: SubjectItem,
	ItemMoved:         SubjectItem,
	ItemDeleted:       SubjectItem,
	ItemRestored:      SubjectItem,
	ItemBulkUpdated:   SubjectItemBatch,
	CommentCreated:    SubjectComment,
	CommentUpdated:    SubjectComment,
	AttachmentAdded:   SubjectAttachment,
	MemberJoined:      SubjectMember,
	PackInstalled:     SubjectPack,
	PackUpgraded:      SubjectPack,
	PackDisabled:      SubjectPack,
}

// IsCanonical reports whether name is in the events/1 set.
//
// The choke point calls this before writing an outbox row. A name that fails
// here is a programming error, not a runtime condition: it means a caller
// invented an event the contract does not define, and letting it through
// would put a name on the public webhook surface that no consumer can
// recognize and no version note explains.
func IsCanonical(name string) bool {
	_, ok := canonical[name]
	return ok
}

// SubjectKind returns the subject kind for a canonical event name, and false
// if the name is not canonical.
func SubjectKind(name string) (string, bool) {
	kind, ok := canonical[name]
	return kind, ok
}

// Canonical returns the events/1 set. The returned slice is freshly built on
// each call, so a caller cannot mutate the contract out from under everyone
// else — the closed set is closed at runtime too, not merely by convention.
//
// Order is unspecified (map iteration); callers that render this for humans
// sort it themselves.
func Canonical() []string {
	names := make([]string, 0, len(canonical))
	for name := range canonical {
		names = append(names, name)
	}
	return names
}
