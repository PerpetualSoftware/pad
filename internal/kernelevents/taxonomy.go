// Package kernelevents defines the events/1 contract surface: the closed set of
// canonical event names and the subject kind each one is about.
//
// This package is the contract, not the plumbing. SPEC-3 (DOC-2653) calls the
// taxonomy a PUBLIC contract rather than internal wiring: connected apps
// consume these names through webhooks, so a name here is as load-bearing as
// an HTTP route. Storage lives in the store's event_outbox table.
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
//   - THE CHOKE POINT WILL OWN THE MAPPING — and does not yet. Today SSE uses
//     snake_case names published from one set of hand-calls and webhooks use
//     dot-form string literals passed at a different set of hand-calls; the
//     two vocabularies drifted because nothing ties them together. SPEC-3 v1.1
//     rules that both surfaces derive from one canonical name, but THIS
//     PACKAGE DOES NOT IMPLEMENT THAT MAPPING: it maps a canonical name to a
//     subject kind and nothing else. The surface mapping, the drain that would
//     use it, and the retirement of the legacy hand-calls are TASK-2714.
//
// SO, PLAINLY, SO NOBODY READS AN INTENTION AS A DESCRIPTION: as of this
// package's introduction the outbox FILLS and nothing drains it. Every legacy
// SSE publish and hand-called webhook dispatch still fires exactly as before.
// ListPendingOutboxEvents has no production caller. That is the agreed shape
// of this change, not an unfinished edge.
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

	// CommentCreated fires on comment creation. Payload: the stored comment
	// row (body, author, item_id), not the item it hangs off — a binding that
	// wants "a comment landed on an item matching X" filters the payload's
	// item_id.
	CommentCreated = "comment.created"

	// CommentUpdated fires when a comment's body actually changes. A re-save
	// of an identical body emits nothing, matching the item events, which emit
	// only when a slice the taxonomy names moved. Payload: the post-update
	// comment row.
	CommentUpdated = "comment.updated"

	// CommentDeleted fires on the HARD delete of a comment. Its payload is
	// REF-ONLY (SPEC-3 v1.4) — ids and parent refs, never the deleted body.
	//
	// The asymmetry with ItemDeleted is deliberate and load-bearing. An item
	// "delete" is an archive: the row survives, so its snapshot stays
	// addressable and carrying it costs nothing that is not already there. A
	// comment delete is a hard delete, and a deletion event that re-ships the
	// content it deletes hands every consumer a durable copy of exactly what
	// the user asked to remove. The consumer needs to reconcile its model,
	// not to receive the body again.
	CommentDeleted = "comment.deleted"

	AttachmentAdded = "attachment.added"

	// AttachmentRemoved fires when an attachment row is hard-deleted (orphan
	// GC). REF-ONLY for the same reason as CommentDeleted, and with a sharper
	// edge: an attachment payload carries the filename, content hash and
	// STORAGE KEY, so re-shipping it on removal would hand out a locator for
	// bytes the system just reclaimed.
	AttachmentRemoved = "attachment.removed"

	// MemberJoined fires when a user is added to a workspace. Payload: the
	// membership's own columns (workspace_id, user_id, role, created_at). The
	// subject is the USER — a membership's key is the (workspace, user) pair
	// and the workspace is already on the envelope. There is deliberately no
	// member.left in v1: nothing forced it, and the closure rule makes that
	// silence a versioned fact.
	MemberJoined = "member.joined"

	// Pack lifecycle events. Declared in the taxonomy because events/1 is the
	// PUBLIC contract and its shape should not shift when packs arrive, but
	// NOTHING EMITS THEM TODAY — there is no pack subsystem yet (phase 2+).
	// A reader looking for their producers will not find one; that is the
	// current state, not a missing wire-up.
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

// eventSpec is everything the kernel knows about one canonical event.
//
// ONE TABLE, not two maps keyed on the same names. A second map is a second
// source of truth that can disagree with the first — and it disagreed
// dangerously: a canonical event missing from a separate family map would
// resolve to the empty family, which a caller declaring nothing would then
// MATCH, so the check would fail OPEN exactly when it was needed (Codex round
// 11, on code round 10 had just added). Co-locating the fields makes the drift
// unrepresentable rather than tested-for.
type eventSpec struct {
	subject string
	payload string

	// sse is the snake_case name this event publishes under on the SSE
	// surface, or "" when the event has no SSE surface at all.
	//
	// SPEC-3 §"The choke point owns the canonical→surface name mapping":
	// SSE (snake_case) and webhooks (dot-form) drifted because nothing tied
	// their vocabularies together. This field is the tie. SPEC-3 v1.5 pins
	// what "derive" means — NAME derivation, not delivery path: webhooks
	// deliver through the drain, SSE stays direct-published at the mutation
	// site (it carries request-scoped Actor/ActorName/Source that a frozen
	// payload deliberately does not hold) and takes only its NAME from here.
	//
	// The empty string is a real value, not a gap: attachment, member and
	// pack events have no SSE surface, and SurfaceSSE reports false for them
	// so a caller cannot mistake silence for a name. Several canonical events
	// map onto ONE SSE name — status_changed and moved both surface as
	// item_updated — because the SSE vocabulary is coarser than events/1 and
	// the UI has never distinguished them. That is a mapping, not a loss:
	// the fine-grained name is what the webhook wire and bindings receive.
	sse string
}

// canonical is the closed events/1 set. Adding an entry here is the ONLY way
// to add a canonical event, and the compiler requires both fields, so a new
// event cannot arrive half-declared.
// Payload families. Every canonical event produces exactly one payload shape,
// and the family is what lets a writer check that an event and a payload were
// meant for each other.
//
// Membership in the canonical set says the NAME is real; it says nothing about
// whether the bytes attached to it are the right shape. Without families, a
// caller could pair item.created with a ref-only deletion payload and the write
// would be accepted — the taxonomy would have validated the half that was
// already obviously correct (Codex round 10).
const (
	// PayloadItemSnapshot: a full item snapshot with the prior_status envelope
	// pseudo-field.
	PayloadItemSnapshot = "item_snapshot"

	// PayloadItemBatch: member refs, the shared delta, and per-member
	// snapshots.
	PayloadItemBatch = "item_batch"

	// PayloadCommentSnapshot / PayloadAttachmentSnapshot: the stored row.
	PayloadCommentSnapshot    = "comment_snapshot"
	PayloadAttachmentSnapshot = "attachment_snapshot"

	// PayloadMember: the membership's own columns.
	PayloadMember = "member"

	// PayloadRefOnly: ids and parent refs, never content. The shape every
	// hard-delete event uses, so a deletion can never re-ship what it deleted.
	PayloadRefOnly = "ref_only"

	// PayloadPack: reserved with the pack events; no producer yet.
	PayloadPack = "pack"
)

var canonical = map[string]eventSpec{
	ItemCreated:       {SubjectItem, PayloadItemSnapshot, "item_created"},
	ItemUpdated:       {SubjectItem, PayloadItemSnapshot, "item_updated"},
	ItemStatusChanged: {SubjectItem, PayloadItemSnapshot, "item_updated"},
	ItemMoved:         {SubjectItem, PayloadItemSnapshot, "item_updated"},
	ItemDeleted:       {SubjectItem, PayloadItemSnapshot, "item_archived"},
	ItemRestored:      {SubjectItem, PayloadItemSnapshot, "item_restored"},
	ItemBulkUpdated:   {SubjectItemBatch, PayloadItemBatch, "items_bulk_updated"},
	CommentCreated:    {SubjectComment, PayloadCommentSnapshot, "comment_created"},
	CommentUpdated:    {SubjectComment, PayloadCommentSnapshot, "comment_updated"},
	CommentDeleted:    {SubjectComment, PayloadRefOnly, "comment_deleted"},
	AttachmentAdded:   {SubjectAttachment, PayloadAttachmentSnapshot, ""},
	AttachmentRemoved: {SubjectAttachment, PayloadRefOnly, ""},
	MemberJoined:      {SubjectMember, PayloadMember, ""},
	PackInstalled:     {SubjectPack, PayloadPack, ""},
	PackUpgraded:      {SubjectPack, PayloadPack, ""},
	PackDisabled:      {SubjectPack, PayloadPack, ""},
}

// PayloadFamily returns the payload shape a canonical event carries, and false
// if the name is not canonical.
//
// A writer that knows which shape it is about to marshal calls this to confirm
// the event name agrees, so an event and a payload that were not meant for each
// other cannot be stored together.
func PayloadFamily(name string) (string, bool) {
	spec, ok := canonical[name]
	return spec.payload, ok
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
	spec, ok := canonical[name]
	return spec.subject, ok
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

// SurfaceSSE returns the SSE wire name a canonical event publishes under, and
// false when it has none — either because the name is not canonical, or
// because the event is deliberately absent from the SSE surface.
//
// ONE BOOL FOR BOTH CASES, deliberately. A caller has the same job either way:
// do not publish. Splitting them would invite a caller to branch on
// "canonical but unmapped" and invent a name, which is the drift this table
// exists to end.
//
// The webhook wire name needs no accessor: it IS the canonical name. That
// asymmetry is the point of SPEC-3's mapping sentence — the dot-form vocabulary
// is the contract, and SSE's snake_case is a surface rendering of it.
func SurfaceSSE(name string) (string, bool) {
	spec, ok := canonical[name]
	if !ok || spec.sse == "" {
		return "", false
	}
	return spec.sse, true
}
