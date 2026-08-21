package store

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/PerpetualSoftware/pad/internal/kernelevents"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// The transactional event outbox (SPEC-3 §choke point, TASK-2658).
//
// WHY THE WRITE LIVES IN THE STORE AND TAKES A *sql.Tx.
//
// Before this file, events were emitted from HTTP handlers after the store
// call returned — outside any transaction, and in a layer that has to infer
// what happened from what it asked for. Both halves were wrong:
//
//   - Outside the transaction, a commit followed by a crash loses the event
//     with nothing recording that it should have existed, and an emit followed
//     by a later failure leaks an event for a mutation that never committed.
//   - Outside the store, the caller does not actually know what the mutation
//     DID. Whether an update changed status — the difference between
//     item.updated and the first-class item.status_changed — is known here,
//     where the old and new rows are both in hand, and is guesswork anywhere
//     else.
//
// So the outbox write is a plain INSERT on the caller's transaction. It has no
// retry, no fallback, and no error swallowing: if the outbox INSERT fails, the
// mutation must fail with it. That is the entire guarantee. A "best-effort"
// outbox write would be strictly worse than no outbox at all, because it would
// look durable while silently reintroducing the loss it exists to prevent.

// OutboxEvent is one row of the event outbox — a canonical events/1 event
// awaiting dispatch.
type OutboxEvent struct {
	ID          string
	WorkspaceID string
	EventType   string
	SubjectKind string
	SubjectID   string
	Payload     []byte
	Hop         int
	OccurredAt  string

	// BatchID correlates every event of ONE handler-path bulk operation.
	// Empty on every single-item mutation, which is almost all of them.
	//
	// SPEC-3 v1.5: the drain folds a batch into one wire item.bulk_updated
	// while the member rows stay individually addressable for bindings. The
	// correlation is RECORDED here rather than inferred at delivery, because
	// a wire event saying "these five items changed together" is only true if
	// something recorded that they did — grouping pending rows by a time
	// window would fold two unrelated updates into somebody's bulk event.
	BatchID string

	// PayloadFamily is the shape the caller marshalled into Payload. It is a
	// WRITE-SIDE assertion, checked against the taxonomy and never stored:
	// the event name already determines the shape, so persisting it would
	// create a second source of truth that could disagree with the first.
	PayloadFamily string

	// Attempts and LastError are drain bookkeeping, populated on read.
	Attempts  int
	LastError string
}

// MutationOption tunes the event side of a store mutation. Nothing about the
// mutation itself changes — these options exist so a CALLER that knows
// something the store cannot (that this write is one member of a bulk
// operation) can say so.
//
// Variadic rather than new required parameters: every existing call site is a
// single-item mutation that has nothing to declare, and making all of them
// pass a zero value would bury the one case that matters.
type MutationOption func(*mutationOptions)

type mutationOptions struct {
	batchID string
}

// WithEventBatch marks every canonical event this mutation emits as a member
// of the named batch.
//
// The caller mints the id per bulk OPERATION, not per item — that is the whole
// content of the correlation. See the batch_id column comment (migration 082)
// for why the drain cannot work this out for itself.
func WithEventBatch(batchID string) MutationOption {
	return func(o *mutationOptions) { o.batchID = batchID }
}

func newMutationOptions(opts []MutationOption) mutationOptions {
	var o mutationOptions
	for _, fn := range opts {
		if fn != nil {
			fn(&o)
		}
	}
	return o
}

// maxOutboxHop is SPEC-3 §L5's synchronous cascade bound: a binding-triggered
// mutation inherits hop+1 and the kernel drops past depth 4.
//
// NOT YET EXERCISED IN PRODUCTION, and worth saying so rather than letting the
// paragraph below read as a description of running behaviour. There is no
// binding kernel yet (phase 2+), so nothing propagates a hop and every
// production write leaves Hop at 0 — this bound and the column behind it are
// the contract's shape, landed with the schema so the field does not have to
// be retrofitted onto rows later. Its enforcement is covered by tests only.
// Per-pack quota accounting, the other half of §L5, is likewise unimplemented.
//
// The rule it will enforce: bounds SYNCHRONOUS cascades only, deliberately —
// a queued playbook run executed later, or a webhook consumer calling back
// through the API, legitimately starts fresh at hop 0. Pretending otherwise
// would be unenforceable, which is why SPEC-3 pairs it with per-pack quotas on
// durable output rather than resting containment on the hop count alone.
const maxOutboxHop = 4

// writeOutboxTx appends one canonical event to the outbox on the caller's
// transaction.
//
// Two rejections, both deliberately hard errors that fail the enclosing
// mutation rather than dropping the event:
//
//   - A non-canonical event name. The events/1 set is closed (SPEC-3
//     §Taxonomy); a name outside it is a programming error, and letting it
//     through would put a name on the PUBLIC webhook surface that no consumer
//     recognizes and no version note explains.
//   - A nil/empty payload. Binding predicates evaluate against the payload
//     snapshot and never against the live store, so an event with no snapshot
//     is undeliverable by construction — better to fail the mutation now than
//     to dispatch something no consumer can act on.
//
// A hop past the cascade bound is NOT an error: it is the bound working. The
// event is dropped and the mutation stands, because the mutation itself was
// legitimate — only the cascade it would extend is not.
func writeOutboxTx(tx *sql.Tx, s *Store, ev OutboxEvent) error {
	if !kernelevents.IsCanonical(ev.EventType) {
		return fmt.Errorf("outbox: %q is not a canonical events/1 event", ev.EventType)
	}
	// The caller declares which payload shape it just marshalled, and the
	// taxonomy says which shape this event carries. Canonical membership only
	// ever validated the NAME; without this, item.created could be stored with
	// a ref-only deletion payload and the write would be accepted (Codex round
	// 10). Pairing them here means an event and a payload that were not meant
	// for each other cannot reach the table at all.
	want, known := kernelevents.PayloadFamilies(ev.EventType)
	if !known || len(want) == 0 {
		// FAIL CLOSED on an event the taxonomy cannot describe. Discarding the
		// ok meant an unknown family resolved to "", which a caller declaring
		// nothing would then MATCH — the check passing precisely when it had
		// no idea what the answer should be.
		//
		// UNREACHABLE TODAY, and said plainly rather than left to imply it is
		// the protection: the canonical table co-locates subject kind and
		// payload family in one entry, so a canonical event cannot be missing
		// a family, and IsCanonical above has already rejected everything
		// else. Verified by mutation — disabling this arm changes no test,
		// because the mismatch check below catches the reachable cases. It
		// stays as the guard for a future table that separates the two again.
		return fmt.Errorf("outbox: event %s has no declared payload family", ev.EventType)
	}
	if !kernelevents.AllowsPayload(ev.EventType, ev.PayloadFamily) {
		return fmt.Errorf("outbox: event %s carries a %q payload, want one of %v",
			ev.EventType, ev.PayloadFamily, want)
	}
	if len(ev.Payload) == 0 {
		return fmt.Errorf("outbox: event %s has an empty payload", ev.EventType)
	}
	// Validate here rather than leaning on the column type, because the column
	// types DISAGREE: Postgres stores payload as JSONB and rejects malformed
	// JSON at the INSERT, SQLite stores it as unconstrained TEXT and accepts
	// it. Without this, the same bad payload fails a mutation on one backend
	// and silently persists an undeliverable event on the other — a
	// dual-dialect divergence in the one place the whole design is trying to
	// make trustworthy (Codex round 3). Validating in Go makes the failure
	// identical on both.
	if !json.Valid(ev.Payload) {
		return fmt.Errorf("outbox: event %s has a malformed JSON payload", ev.EventType)
	}
	if ev.Hop > maxOutboxHop {
		// Cascade bound reached: drop the event, keep the mutation. No
		// production caller can reach this today (see maxOutboxHop — nothing
		// propagates a hop yet); it is here so the bound exists before the
		// thing that needs it.
		//
		// When it does become reachable, a silent drop would make a runaway
		// binding indistinguishable from one that never fired, so surfacing it
		// is owed then — via the dispatcher's quota accounting, which is also
		// still unimplemented. Recorded as an obligation rather than described
		// as if it were already in place.
		return nil
	}

	if ev.ID == "" {
		ev.ID = newID()
	}
	// occurred_at is STAMPED HERE, never accepted from the caller — same
	// reasoning as subject_kind, applied to the rest of the class rather than
	// waiting for a review to name the next member (CONVE-18).
	//
	// SPEC-3 §Bindings pins time-relative predicates (`within`) to this value,
	// so a wrong one silently changes how a predicate evaluates — the same
	// shape of silent harm a wrong subject_kind causes at routing time. No
	// caller sets it today, and "the moment the event was written" is the only
	// honest value while the write is transactional with the mutation.
	//
	// The enumeration, so the next reader does not have to redo it: of the
	// eight fields on OutboxEvent, event_type is validated against the closed
	// set, payload is validated as non-empty JSON, hop is bounded, subject_kind
	// and occurred_at are derived, and id defaults but fails LOUDLY on a
	// duplicate (primary key). That leaves workspace_id and subject_id as
	// genuine caller inputs — neither is derivable, both are checked at their
	// own call sites, and the bulk emitter partitions rather than trusting the
	// workspace it is handed.
	ev.OccurredAt = now()
	// SUBJECT KIND IS DERIVED, NEVER TRUSTED. It is a pure function of the
	// event name, so a caller-supplied value can only ever agree with the
	// taxonomy or be wrong — and a wrong one persists silently and misroutes
	// the event at drain time (Codex round 9). Before this, a non-empty value
	// was taken as given, so `item.created` could be stored with
	// subject_kind "comment" and nothing would notice; every test happened to
	// pass either the correct value or none, which is exactly the blind spot
	// that lets a bug like this survive eight review rounds.
	kind, ok := kernelevents.SubjectKind(ev.EventType)
	if !ok {
		// Unreachable: IsCanonical above already established membership, and
		// both answers come from the same map. Guarded anyway so a future
		// split of those two lookups cannot silently write rows with an empty
		// subject_kind.
		return fmt.Errorf("outbox: no subject kind for canonical event %q", ev.EventType)
	}
	// A caller that supplied a DIFFERENT kind believes something false about
	// the taxonomy. Silently correcting it would fix this row and leave the
	// belief in place, so it is an error rather than an overwrite.
	if ev.SubjectKind != "" && ev.SubjectKind != kind {
		return fmt.Errorf("outbox: event %s has subject kind %q, want %q",
			ev.EventType, ev.SubjectKind, kind)
	}
	ev.SubjectKind = kind

	_, err := tx.Exec(s.q(`
		INSERT INTO event_outbox (id, workspace_id, event_type, subject_kind, subject_id, payload, hop, occurred_at, batch_id)
		VALUES (?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, NULLIF(?, ''))
	`), ev.ID, ev.WorkspaceID, ev.EventType, ev.SubjectKind, ev.SubjectID, string(ev.Payload), ev.Hop, ev.OccurredAt, ev.BatchID)
	if err != nil {
		return fmt.Errorf("outbox: write %s: %w", ev.EventType, err)
	}
	return nil
}

// marshalEventPayload renders an event payload, returning an error rather than
// an empty payload on failure.
//
// Callers write the result straight into the mutation's transaction, so a
// marshal failure has to propagate: a payload that cannot be rendered is an
// event that cannot be delivered, and swallowing it here would produce exactly
// the silent gap the outbox exists to close.
func marshalEventPayload(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("outbox: marshal payload: %w", err)
	}
	return b, nil
}

// itemEventPayload is the wire shape of an item-subject event payload.
//
// models.Item is EMBEDDED rather than nested under an "item" key, deliberately.
// SPEC-3 §Bindings says predicates are query/1 #where fragments applied
// verbatim — no second filter language — and query/1 addresses item fields by
// their own names. Nesting the snapshot would force every predicate to carry an
// "item." prefix that exists nowhere else in the query grammar, which is how a
// second dialect gets invented by accident. Embedding promotes the snapshot's
// fields to the top level, so a predicate that works against an item in a query
// works unchanged against an item in an event.
//
// prior_status sits alongside them as the envelope pseudo-field SPEC-3 names,
// which is what makes "nonterminal → terminal" filterable.
//
// A POINTER, not a string with omitempty, and the distinction is the whole
// point. An item can transition FROM no status at all — "" → "open" is a real
// status change and item.status_changed fires for it — so with omitempty on a
// plain string that transition dropped the key entirely, leaving a predicate
// unable to tell "the prior status was empty" from "this event has no prior
// status".
//
// So: nil on every event that has no prior status, and present-and-possibly-
// empty on item.status_changed, where the empty value is data.
type itemEventPayload struct {
	*models.Item
	PriorStatus *string `json:"prior_status,omitempty"`
}

// scrubItemPII returns a copy of the item with the JOIN-POPULATED assignee
// identity removed, for storage in an event payload.
//
// An outbox payload is a FROZEN snapshot that outlives its subject by design.
// Account deletion's de-identify posture (DeleteAccountAtomic) nulls
// comments.user_id, items.created_by_user_id and so on precisely so a departed
// user's identity stops being readable while the rows survive — but it reaches
// only LIVE rows. A frozen payload keeps whatever it captured, so an assignee's
// NAME AND EMAIL ADDRESS would remain legible in the outbox after the account
// that owned them was deleted, and today nothing drains or prunes the table.
//
// THE RULE APPLIED, stated as the rule rather than as a proxy for it: remove
// DIRECTLY IDENTIFYING personal data — a human name, an email address — and
// keep opaque identifiers and row state. assigned_user_id stays: a binding
// predicate filters on it, and after the account is gone it is a dangling
// reference to nobody rather than a way to identify anyone. The name and email
// are denormalized lookups a consumer can resolve for itself if it is
// entitled to.
//
// Deliberately NOT scrubbed: collection_*/ref and the parent fields, which are
// derived metadata rather than identity — predicates address items by
// collection, so removing them would cost real expressiveness for no privacy
// gain.
//
// POPULATION, enumerated rather than fixed one instance at a time (CONVE-18).
// Five payload shapes carry data into the outbox:
//
//	item (single)   — JOIN-populated assignee name + email. SCRUBBED, here.
//	item (bulk)     — same snapshots, same scrub, applied in outboxMemberSnapshotsTx.
//	comment         — `author` is the comments table's OWN column, and account
//	                  de-identification does not clear it on live rows either
//	                  (it nulls user_id only), so a frozen copy is no more
//	                  legible than the live row. Left alone deliberately.
//	attachment      — uploaded_by, the attachments table's own column. Same.
//	member.joined   — user_id only, an opaque identifier.
//
// So exactly one shape needed scrubbing, and the reason it stood out is that
// it was the only one carrying a JOIN rather than the row.
func scrubItemPII(item *models.Item) *models.Item {
	clone := *item
	clone.AssignedUserName = ""
	clone.AssignedUserEmail = ""
	return &clone
}

// emitItemEventTx writes one item-subject event to the outbox on the caller's
// transaction, carrying the post-mutation snapshot.
//
// The snapshot must be read back INSIDE the transaction before this is called.
// A snapshot assembled from the caller's input rather than from the row is the
// bug this design exists to prevent: it would describe what the caller asked
// for, while the event claims to describe what happened.
// priorStatus is a POINTER so a transition from an empty prior status is
// distinguishable from an event that has none — pass nil for every event other
// than item.status_changed.
func (s *Store) emitItemEventTx(tx *sql.Tx, eventType string, item *models.Item, priorStatus *string, batchID string) error {
	if item == nil {
		return fmt.Errorf("outbox: %s has no item snapshot", eventType)
	}
	snapshot := scrubItemPII(item)
	payload, err := marshalEventPayload(itemEventPayload{Item: snapshot, PriorStatus: priorStatus})
	if err != nil {
		return err
	}
	return writeOutboxTx(tx, s, OutboxEvent{
		WorkspaceID:   item.WorkspaceID,
		EventType:     eventType,
		SubjectID:     item.ID,
		Payload:       payload,
		PayloadFamily: kernelevents.PayloadItemSnapshot,
		BatchID:       batchID,
	})
}

// emitCommentEventTx writes one comment-subject event to the outbox on the
// caller's transaction.
//
// The subject is the COMMENT, not the item it hangs off. A binding that wants
// "a comment landed on an item matching X" filters the payload's item_id; a
// binding keyed on the item as subject would be unable to distinguish a
// comment from an edit to the item itself.
func (s *Store) emitCommentEventTx(tx *sql.Tx, eventType string, comment *models.Comment) error {
	if comment == nil {
		return fmt.Errorf("outbox: %s has no comment snapshot", eventType)
	}
	payload, err := marshalEventPayload(comment)
	if err != nil {
		return err
	}
	return writeOutboxTx(tx, s, OutboxEvent{
		WorkspaceID:   comment.WorkspaceID,
		EventType:     eventType,
		SubjectID:     comment.ID,
		Payload:       payload,
		PayloadFamily: kernelevents.PayloadCommentSnapshot,
	})
}

// emitAttachmentEventTx writes one attachment-subject event to the outbox on
// the caller's transaction.
//
// Callers own the decision about WHICH attachment rows are event-worthy —
// variants and derived rows are attachments too, and this helper deliberately
// does not second-guess that gate, because the caller is the only place that
// knows how the row was produced.
func (s *Store) emitAttachmentEventTx(tx *sql.Tx, eventType string, a *models.Attachment) error {
	if a == nil {
		return fmt.Errorf("outbox: %s has no attachment snapshot", eventType)
	}
	payload, err := marshalEventPayload(a)
	if err != nil {
		return err
	}
	return writeOutboxTx(tx, s, OutboxEvent{
		WorkspaceID:   a.WorkspaceID,
		EventType:     eventType,
		SubjectID:     a.ID,
		Payload:       payload,
		PayloadFamily: kernelevents.PayloadAttachmentSnapshot,
	})
}

// memberEventPayload is the wire shape of a member-subject event.
//
// A hand-built struct rather than a model, because workspace_members has no
// model type — it is a join row. The keys are the row's own columns so a
// binding predicate addresses them by the names they have in the database,
// consistent with how the item payload works.
type memberEventPayload struct {
	WorkspaceID string `json:"workspace_id"`
	UserID      string `json:"user_id"`
	Role        string `json:"role"`
	CreatedAt   string `json:"created_at"`
}

// emitMemberEventTx writes one member-subject event to the outbox on the
// caller's transaction.
//
// The subject id is the USER, which is the only identifier a membership has —
// the row's key is the (workspace, user) pair and the workspace is already on
// the envelope.
func (s *Store) emitMemberEventTx(tx *sql.Tx, eventType, workspaceID, userID, role, ts string) error {
	payload, err := marshalEventPayload(memberEventPayload{
		WorkspaceID: workspaceID,
		UserID:      userID,
		Role:        role,
		CreatedAt:   ts,
	})
	if err != nil {
		return err
	}
	return writeOutboxTx(tx, s, OutboxEvent{
		WorkspaceID:   workspaceID,
		EventType:     eventType,
		SubjectID:     userID,
		Payload:       payload,
		PayloadFamily: kernelevents.PayloadMember,
	})
}

// bulkItemEventPayload is the wire shape of item.bulk_updated (SPEC-3 v1.1):
// member refs, the shared delta, and PER-MEMBER SNAPSHOTS.
//
// The per-member snapshots are not optional decoration. v1.1 makes wire
// delivery batched but binding evaluation PER-MEMBER — the dispatcher runs
// item-level selectors against each member snapshot — so a payload without
// them would silently make bulk mutations invisible to every item.updated and
// status_changed binding, which is the gap the batch event was admitted to
// avoid rather than create.
type bulkItemEventPayload struct {
	// Delta describes what the one mutation did, shared across members
	// (e.g. {"kind":"field_option_renamed","field":"status","from":"wip","to":"in-progress"}).
	Delta map[string]any `json:"delta"`

	MemberCount int            `json:"member_count"`
	Members     []*models.Item `json:"members"`
}

// emitBulkItemEventTx writes one item.bulk_updated event covering every member
// of a single-transaction bulk mutation.
//
// ONE EVENT, NOT N — this is TASK-1668's anti-flood decision, which SPEC-3
// v1.1 made canonical: a mutation the user experiences as ONE action (renaming
// a select option, renaming an item and cascading its backlinks) must not
// arrive at a webhook consumer as hundreds of separate deliveries.
//
// PAYLOAD SIZE IS DELIBERATELY UNBOUNDED IN V1, and that is a decision rather
// than an oversight. The alternatives both cost correctness: capping the
// member list silently drops binding evaluation for the tail, and omitting
// `content` from the snapshots would break precisely the bindings that matter
// for the wiki-title cascade, whose entire delta IS content. One large row is
// the trade the batch event exists to make — the flood argument was about wire
// deliveries and consumer rate limits, not about a single stored row — and the
// payload is bounded by data the mutation already touched. If a real workspace
// shows this producing unreasonable rows, bounding it is a measured follow-up,
// not a guess made here.
//
// A bulk mutation that touched nothing writes nothing: an empty member set is
// not an event.
func (s *Store) emitBulkItemEventTx(tx *sql.Tx, workspaceID string, members []*models.Item, delta map[string]any) error {
	if len(members) == 0 {
		return nil
	}

	// PARTITION BY THE MEMBERS' OWN WORKSPACE, never by the caller's.
	//
	// The caller passes the workspace it thinks the mutation belongs to, and
	// for the collection-option rename that is exactly right. The wiki-title
	// cascade is not so obviously safe: its source query selects on
	// `target_item_id` alone and carries each source row's workspace_id
	// per-row rather than assuming the renamed item's — so a member in a
	// different workspace is not excluded by construction.
	//
	// Publishing a member snapshot under someone else's workspace_id would put
	// one workspace's item content on another workspace's webhook. Whether
	// that is reachable today is not the question worth answering: partitioning
	// costs one map and makes it impossible, and "unreachable" is a property of
	// today's queries rather than of this function.
	//
	// The caller's workspaceID is used only when a member somehow carries none.
	byWorkspace := map[string][]*models.Item{}
	var order []string
	for _, m := range members {
		ws := m.WorkspaceID
		if ws == "" {
			ws = workspaceID
		}
		if _, seen := byWorkspace[ws]; !seen {
			order = append(order, ws)
		}
		byWorkspace[ws] = append(byWorkspace[ws], m)
	}

	for _, ws := range order {
		group := byWorkspace[ws]
		payload, err := marshalEventPayload(bulkItemEventPayload{
			Delta:       delta,
			MemberCount: len(group),
			Members:     group,
		})
		if err != nil {
			return err
		}
		if err := writeOutboxTx(tx, s, OutboxEvent{
			WorkspaceID:   ws,
			EventType:     kernelevents.ItemBulkUpdated,
			Payload:       payload,
			PayloadFamily: kernelevents.PayloadItemBatch,
		}); err != nil {
			return err
		}
	}
	return nil
}

// outboxMemberSnapshotsTx builds the MEMBER LIST FOR AN item.bulk_updated
// PAYLOAD. It is not a general "read these items" helper, and the name says so
// on purpose (Codex round 10): it applies three outbox policies a maintainer
// reusing it for ordinary snapshots would not expect and would not see fail —
// it DE-DUPLICATES, it SILENTLY SKIPS rows that no longer resolve, and it
// SCRUBS assignee identity. Any of those makes a general-purpose caller's
// result quietly incomplete rather than wrong-looking.
//
// De-duplication is the point of taking a slice rather than reading as it
// goes: a bulk mutation can touch the same row twice (two option renames on
// different fields of one item), and a member list carrying that row twice —
// once in an intermediate state — would make per-member binding evaluation
// fire twice on one item, the second time against a state that never existed
// as a final value.
//
// COST, stated rather than discovered later: this is N sequential joined reads
// for N members, inside the caller's transaction and under whatever lock the
// caller holds (Codex round 6). The honest framing is that it roughly DOUBLES
// an already-N-long lock hold rather than introducing one — the migration loop
// this serves already issues N sequential UPDATEs under the same lock, by
// design, so each row gets its own seq. A single batched read would halve it;
// that is BUG-2718, deliberately not done here because it means a new joined
// query on the least-reviewed path of a heavily-reviewed change.
func (s *Store) outboxMemberSnapshotsTx(tx *sql.Tx, ids []string) ([]*models.Item, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(ids))
	out := make([]*models.Item, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		item, err := s.getItemTx(tx, id)
		if err != nil {
			return nil, fmt.Errorf("outbox: read bulk member snapshot %s: %w", id, err)
		}
		if item == nil {
			// Archived or gone between the write and here. Skipping is
			// correct: a member snapshot the event cannot produce is one no
			// predicate could have evaluated anyway, and inventing a
			// placeholder would put a shape in the payload that never existed.
			continue
		}
		// Same scrub the single-item events get — a bulk payload is no less
		// durable, and this is the SECOND of the two places item snapshots
		// enter a payload. Population: 2 producers (emitItemEventTx, here),
		// both scrubbed.
		out = append(out, scrubItemPII(item))
	}
	return out, nil
}

// refOnlyDeletionPayload is the wire shape of a hard-delete event
// (comment.deleted, attachment.removed) — SPEC-3 v1.4.
//
// IDS AND PARENT REFS, NEVER CONTENT. A deletion event exists so a consumer
// can reconcile its model; shipping the deleted body or an attachment's
// storage key would hand out a durable copy of exactly what was removed. Every
// field here is an identifier: nothing a consumer did not already receive on
// the create event, and nothing that re-exposes the subject.
type refOnlyDeletionPayload struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	ItemID      string `json:"item_id,omitempty"`
	ParentID    string `json:"parent_id,omitempty"`
}

// emitRefOnlyDeletionTx writes one ref-only hard-delete event.
//
// Kept as its own helper rather than a flag on the existing emitters so the
// ref-only shape cannot be reached with a full snapshot by accident: there is
// no parameter here that could carry one.
func (s *Store) emitRefOnlyDeletionTx(tx *sql.Tx, eventType, workspaceID, subjectID, itemID, parentID string) error {
	payload, err := marshalEventPayload(refOnlyDeletionPayload{
		ID:          subjectID,
		WorkspaceID: workspaceID,
		ItemID:      itemID,
		ParentID:    parentID,
	})
	if err != nil {
		return err
	}
	return writeOutboxTx(tx, s, OutboxEvent{
		WorkspaceID:   workspaceID,
		EventType:     eventType,
		SubjectID:     subjectID,
		Payload:       payload,
		PayloadFamily: kernelevents.PayloadRefOnly,
	})
}

// itemDeltaExcludedKeys are the snapshot keys that must not count as a change
// when deciding whether item.updated's slice moved.
//
// AN EXCLUSION LIST, NOT AN INCLUSION LIST, and that direction is the point. A
// new column that REACHES models.Item's JSON is compared by default, so the
// worst such a schema change can do is emit an extra item.updated — visible,
// arguable, fixable. An inclusion list would fail the other way: the new column
// would be silently invisible, and a mutation that changed it would emit
// nothing at all.
//
// THE LIMIT OF THAT GUARANTEE, stated precisely because the sloppy version of
// this comment overclaimed it (Codex round 3): the diff sees the SNAPSHOT, not
// the row. A column that models.Item does not carry — or carries as `json:"-"`
// — is invisible here no matter what this list says. `items.last_restore_seq`
// and the content-flush watermarks are exactly that, so a write touching ONLY
// one of them emits nothing. Today that is unreachable in practice: every
// caller that moves them also writes content or fields in the same mutation.
// It is not structurally guaranteed, and a future column added to the table but
// not to the model would inherit the same blind spot — so ADDING A PERSISTED
// COLUMN THAT MATTERS TO CONSUMERS MEANS ADDING IT TO models.Item, not just to
// the schema.
//
// Each exclusion is one of three things: bookkeeping that changes on EVERY
// mutation (updated_at, seq, last_modified_by) and would therefore make the
// disjoint-delta rule vacuous by always reporting a change; a field owned by a
// DIFFERENT canonical event under the rule (collection_id and the derived
// collection_*/ref keys belong to item.moved; deleted_at belongs to
// item.deleted / item.restored); or a value that cannot change after create
// (id, workspace_id, created_at, created_by, item_number).
var itemDeltaExcludedKeys = []string{
	"updated_at",
	"seq",
	"last_modified_by",

	"collection_id",
	"collection_slug",
	"collection_name",
	"collection_icon",
	"collection_prefix",
	"ref",

	"deleted_at",

	"id",
	"workspace_id",
	"created_at",
	"created_by",
	"item_number",
}

// fieldValueIsString reports whether the item's fields blob holds a JSON string
// at key — the only shape extractFieldValue, and therefore the whole
// status-transition path, can read. A missing key counts as a string ("" is
// what extractFieldValue returns for it, and a cleared status is a legitimate
// status transition).
func fieldValueIsString(it *models.Item, key string) bool {
	if it == nil {
		return false
	}
	var f map[string]any
	if err := json.Unmarshal([]byte(it.Fields), &f); err != nil {
		return false
	}
	v, present := f[key]
	if !present || v == nil {
		return true
	}
	_, ok := v.(string)
	return ok
}

// itemUpdatedSliceChanged reports whether anything outside the status and
// location slices differs between two in-transaction snapshots of one item.
//
// Both snapshots MUST come from getItemTx. It is the same SQL that produced
// `existing` under the write lock and `updated` after the write, so
// join-populated fields are rendered identically on both sides and cannot
// register as a spurious delta. Comparing a getItemTx snapshot against one
// from a different query would silently compare rendering differences as if
// they were changes.
func itemUpdatedSliceChanged(before, after *models.Item, statusKey string) (bool, error) {
	// MASK THE DONE KEY ONLY WHEN item.status_changed CAN ACTUALLY SEE IT.
	//
	// The status machinery (extractFieldValue) reads a done-key value only when
	// it is a JSON STRING; anything else — a number, a bool, an object — reads
	// as "". So for a numeric done field, `{"stage":1}` → `{"stage":2}` is a
	// change status_changed will never report. Masking the key unconditionally
	// then deleted it from BOTH snapshots, the remainder compared equal, and the
	// mutation emitted NOTHING AT ALL: a real field change, silently unobservable
	// (Codex round 3).
	//
	// The rule that fixes it is the disjoint-delta rule read carefully: a slice
	// belongs to status_changed only if status_changed will describe it. When it
	// will not, the change falls back to item.updated's slice, where something
	// can. So mask only when both sides hold a string at that key — which is
	// exactly the condition under which the status delta is visible.
	maskStatus := statusKey == "" ||
		(fieldValueIsString(before, statusKey) && fieldValueIsString(after, statusKey))

	normalize := func(it *models.Item) (string, error) {
		raw, err := json.Marshal(it)
		if err != nil {
			return "", fmt.Errorf("outbox: marshal item snapshot: %w", err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return "", fmt.Errorf("outbox: re-read item snapshot: %w", err)
		}
		for _, k := range itemDeltaExcludedKeys {
			delete(m, k)
		}
		// The status field lives INSIDE the fields blob, so excluding it is a
		// nested delete rather than a top-level one. Re-marshalling the blob
		// through a map also normalizes key order, which means a rewrite that
		// only reorders keys is correctly NOT a change.
		if statusKey != "" && maskStatus {
			if blob, ok := m["fields"].(string); ok {
				var f map[string]any
				if err := json.Unmarshal([]byte(blob), &f); err == nil {
					delete(f, statusKey)
					nb, err := json.Marshal(f)
					if err != nil {
						return "", fmt.Errorf("outbox: re-marshal fields blob: %w", err)
					}
					m["fields"] = string(nb)
				}
				// An unparseable fields blob (an array, a scalar, corrupt JSON)
				// is left verbatim. It compares byte-for-byte on both sides, so
				// a corrupt blob still yields a correct changed / did-not-change
				// answer; it just cannot have the status key masked out of it.
				// Leaving such a change to item.updated is the honest outcome:
				// the status machinery cannot see it either, so item.updated is
				// the only event that can describe it at all.
			}
		}
		// Map marshalling sorts keys, so this is a stable canonical form.
		out, err := json.Marshal(m)
		if err != nil {
			return "", fmt.Errorf("outbox: canonicalize item snapshot: %w", err)
		}
		return string(out), nil
	}

	a, err := normalize(before)
	if err != nil {
		return false, err
	}
	b, err := normalize(after)
	if err != nil {
		return false, err
	}
	return a != b, nil
}

// emitItemUpdateEventsTx writes the canonical event(s) for one item UPDATE,
// applying SPEC-3 v1.3's DISJOINT-DELTA RULE.
//
// Canonical events PARTITION a mutation's delta rather than competing to
// describe it: item.status_changed owns the status field, item.moved owns
// location, item.updated owns everything else. A mutation emits every event
// whose slice actually changed — so a bare status flip emits status_changed
// alone, and a single update that changes status AND priority emits BOTH,
// because two things happened and each event describes its own slice exactly
// once.
//
// The rule is what keeps item.updated honest: it does not mean "changed,
// except status" as a special case, it owns a defined slice the same way the
// others do. It also means the decision here has to DIFF SLICES rather than
// branch on "was this a status update" — branching would drop the item.updated
// half of every mixed update, silently, which is the shape of bug that
// motivated the rule.
func (s *Store) emitItemUpdateEventsTx(tx *sql.Tx, before, after *models.Item, statusChanged bool, priorStatus, statusKey, batchID string) error {
	if statusChanged {
		// Taken by address unconditionally: an empty prior status is a real
		// prior status here, not an absent one.
		prior := priorStatus
		if err := s.emitItemEventTx(tx, kernelevents.ItemStatusChanged, after, &prior, batchID); err != nil {
			return err
		}
	}

	otherChanged, err := itemUpdatedSliceChanged(before, after, statusKey)
	if err != nil {
		return err
	}
	if otherChanged {
		if err := s.emitItemEventTx(tx, kernelevents.ItemUpdated, after, nil, batchID); err != nil {
			return err
		}
	}
	return nil
}

// ListPendingOutboxEvents returns up to limit undispatched events in drain
// order.
//
// Ordering is (occurred_at, id) and is NOT a delivery-order contract — SPEC-3
// v1 promises at-least-once with duplicates possible, not ordering. The id tie
// break exists so the drain itself is deterministic when timestamps collide,
// not so consumers can infer sequence.
//
// DELIBERATELY CROSS-WORKSPACE AND UNAUTHORIZED, which is safe only because of
// who may call it. The drain is a single server-internal loop that must serve
// every workspace in the instance; scoping it per workspace would mean asking
// it to enumerate workspaces first, which is both slower and no more secure.
// The authorization boundary for these payloads is the DISPATCHER — it decides
// which surface each event reaches, and a webhook endpoint is registered by the
// workspace's own owner.
//
// The rule that keeps that true: this returns raw payloads containing full item
// content and comment bodies, so it must never be reachable from an HTTP
// handler, an MCP tool, or anything else carrying a user's identity. If you
// find yourself calling it from a request path, the answer is a
// workspace-scoped query with an authorization check, not this (Codex round 2).
func (s *Store) ListPendingOutboxEvents(limit int) ([]OutboxEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(s.q(`
		SELECT id, workspace_id, event_type, subject_kind, COALESCE(subject_id, ''), payload, hop, occurred_at, attempts, COALESCE(last_error, ''), COALESCE(batch_id, '')
		FROM event_outbox
		WHERE dispatched_at IS NULL
		ORDER BY occurred_at, id
		LIMIT ?
	`), limit)
	if err != nil {
		return nil, fmt.Errorf("list pending outbox events: %w", err)
	}
	defer rows.Close()

	var out []OutboxEvent
	for rows.Next() {
		var ev OutboxEvent
		var payload string
		if err := rows.Scan(&ev.ID, &ev.WorkspaceID, &ev.EventType, &ev.SubjectKind, &ev.SubjectID,
			&payload, &ev.Hop, &ev.OccurredAt, &ev.Attempts, &ev.LastError, &ev.BatchID); err != nil {
			return nil, fmt.Errorf("scan outbox event: %w", err)
		}
		ev.Payload = []byte(payload)
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate outbox events: %w", err)
	}
	return out, nil
}

// MarkOutboxDispatched stamps events as delivered.
//
// Called AFTER the surfaces have been handed the event, never before. The
// ordering is what makes the guarantee at-least-once rather than at-most-once:
// a crash between dispatch and this stamp re-delivers on the next drain, which
// SPEC-3 §Delivery guarantees explicitly permits and consumers dedupe on the
// event id.
func (s *Store) MarkOutboxDispatched(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	ts := now()
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("mark outbox dispatched: %w", err)
	}
	defer tx.Rollback()

	for _, id := range ids {
		if _, err := tx.Exec(s.q(`
			UPDATE event_outbox SET dispatched_at = ? WHERE id = ? AND dispatched_at IS NULL
		`), ts, id); err != nil {
			return fmt.Errorf("mark outbox dispatched %s: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mark outbox dispatched: %w", err)
	}
	return nil
}

// MarkOutboxAttemptFailed records a failed dispatch attempt, leaving the event
// pending so the next drain retries it.
//
// The CLAIM IS RELEASED here, not left to expire. A transient failure means
// the event is still owed and nothing is in flight for it; holding the claim
// until the lease times out would idle the row for the whole lease window for
// no reason, and on a single-instance deployment that is the only reason it
// would ever wait at all.
func (s *Store) MarkOutboxAttemptFailed(id, reason string) error {
	if _, err := s.db.Exec(s.q(`
		UPDATE event_outbox
		SET attempts = attempts + 1, last_error = ?, claimed_at = NULL, claimed_by = NULL
		WHERE id = ? AND dispatched_at IS NULL
	`), reason, id); err != nil {
		return fmt.Errorf("mark outbox attempt failed %s: %w", id, err)
	}
	return nil
}

// PruneDispatchedOutbox deletes dispatched events stamped before the given
// timestamp, returning how many rows went.
//
// Retention rather than delete-on-dispatch, for two reasons. The table is the
// only durable record that an event existed at all, so keeping a window of it
// is what makes "did this mutation emit?" answerable after the fact. And
// because the outbox intentionally carries no foreign keys — rows outlive
// their subjects by design — retention is also the mechanism that keeps the
// table bounded, which referential integrity would otherwise have done.
func (s *Store) PruneDispatchedOutbox(before string) (int64, error) {
	res, err := s.db.Exec(s.q(`
		DELETE FROM event_outbox WHERE dispatched_at IS NOT NULL AND dispatched_at < ?
	`), before)
	if err != nil {
		return 0, fmt.Errorf("prune dispatched outbox: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// RowsAffected is advisory here — the DELETE already succeeded, and
		// reporting a count failure as a prune failure would make a caller
		// retry a completed prune.
		return 0, nil
	}
	return n, nil
}

// PruneUndispatchedOutbox deletes events that were never dispatched and are
// older than the given timestamp, returning how many rows went.
//
// This one DROPS COMMITTED EVENTS, which is the opposite of everything else in
// this file, so the reason has to be explicit rather than inferred from the
// name.
//
// SPEC-3 makes payload privacy TEMPORAL: an outbox payload is a frozen
// snapshot, and account deletion's de-identify posture reaches only live rows,
// so a payload that never drains keeps whatever it captured for as long as the
// row survives. PruneDispatchedOutbox cannot reach these — it filters on
// dispatched_at IS NOT NULL — so without this, a row that can never be
// delivered (a workspace whose only webhook was deleted, an endpoint that 4xxs
// forever, an event whose surfaces all reject it) keeps its frozen payload
// indefinitely. The retention window is what makes the privacy claim finite,
// and a window only one of its two halves can close is not a window.
//
// So the trade is stated plainly: at-least-once delivery holds WITHIN the
// retention window and not past it. That is why the caller's max-age must be
// far larger than any retry schedule — the rows this reaches are ones no
// further attempt would help, and the alternative to dropping them is keeping
// user content in a table nothing will ever read.
//
// Deleting is deliberate rather than stamping them dispatched: a dispatched
// stamp would be a lie in the durable record, and this table is the only
// evidence of what the kernel emitted. A row that leaves is honestly absent; a
// row marked delivered that never was would corrupt every later answer to "did
// this mutation emit?".
//
// Callers log the count — an undispatchable event reaching its max age is a
// delivery problem that has been failing for the whole window, and the number
// is the only place it becomes visible.
func (s *Store) PruneUndispatchedOutbox(before string) (int64, error) {
	res, err := s.db.Exec(s.q(`
		DELETE FROM event_outbox WHERE dispatched_at IS NULL AND occurred_at < ?
	`), before)
	if err != nil {
		return 0, fmt.Errorf("prune undispatched outbox: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// Advisory, exactly as in PruneDispatchedOutbox: the DELETE already
		// succeeded, and reporting a count failure as a prune failure would
		// make a caller retry a completed prune.
		return 0, nil
	}
	return n, nil
}

// ClaimPendingOutboxEvents claims up to limit pending events for one drain
// pass and returns them, oldest first.
//
// WHY A CLAIM AT ALL. Pad Cloud runs N instances against one database and each
// runs the drain, so an unclaimed pending row is delivered N times BY
// CONSTRUCTION. SPEC-3 permits duplicates — consumers dedupe on the event id —
// but "occasionally, after a crash" and "always, once per instance" are
// different promises, and only the first is one a consumer can budget for.
//
// The arbiter is the CONDITIONAL UPDATE, not the SELECT that finds candidates.
// Two instances reading the same candidate list is expected and harmless: each
// row's UPDATE carries its own claim-availability predicate, so one instance
// writes it and the other matches zero rows. This is BUG-2415's orphan-GC
// protocol, and it is dialect-uniform on purpose — Postgres FOR UPDATE SKIP
// LOCKED plus a separate SQLite path would be two implementations of one
// behaviour, only one of which ever runs where it matters.
//
// leaseCutoff is the claim expiry: a row claimed before it is claimable again,
// because an instance that dies between claiming and dispatching must not
// strand its events. At-least-once is exactly the promise that makes
// re-claiming safe.
//
// BATCHES ARE CLAIMED WHOLE, past the limit if necessary. A batch split across
// two passes would be folded into two wire events each reporting a partial
// member count — the limit is a throughput knob, and letting it decide what a
// bulk operation "was" would make the wire event's truth depend on queue depth.
// Rows of a batch another instance already holds are simply not ours; the wire
// payload carries the batch id so a consumer can correlate a split that a
// concurrent claim did produce.
func (s *Store) ClaimPendingOutboxEvents(drainer string, limit int, leaseCutoff string) ([]OutboxEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	// The token, not the drainer name, is what identifies THIS pass: a
	// drainer that claims twice in the same second would otherwise read back
	// its previous pass's rows as well.
	token := drainer + ":" + newID()

	ids, err := s.pendingClaimCandidates(limit, leaseCutoff)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}

	if err := s.claimOutboxIDs(token, ids, leaseCutoff); err != nil {
		return nil, err
	}
	return s.outboxEventsClaimedBy(token)
}

// claimOutboxIDs is the ARBITER: one conditional UPDATE per candidate, each
// carrying its own claim-availability predicate.
//
// Split out from ClaimPendingOutboxEvents so a test can drive it with a
// deliberately STALE candidate list — the race this exists for. Called with
// ids the caller selected earlier, it must claim only those still available,
// which is not observable through the public entry point (whose candidate
// query has already filtered them).
func (s *Store) claimOutboxIDs(token string, ids []string, leaseCutoff string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("claim outbox events: %w", err)
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.Exec(s.q(`
			UPDATE event_outbox
			SET claimed_at = ?, claimed_by = ?
			WHERE id = ?
			  AND dispatched_at IS NULL
			  AND (claimed_at IS NULL OR claimed_at < ?)
		`), now(), token, id, leaseCutoff); err != nil {
			return fmt.Errorf("claim outbox event %s: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("claim outbox events: %w", err)
	}
	return nil
}

// pendingClaimCandidates returns the ids a claim pass should attempt: the
// oldest claimable pending rows, plus every claimable sibling of any batch
// they belong to.
func (s *Store) pendingClaimCandidates(limit int, leaseCutoff string) ([]string, error) {
	rows, err := s.db.Query(s.q(`
		SELECT id, COALESCE(batch_id, '')
		FROM event_outbox
		WHERE dispatched_at IS NULL
		  AND (claimed_at IS NULL OR claimed_at < ?)
		ORDER BY occurred_at, id
		LIMIT ?
	`), leaseCutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("list claimable outbox events: %w", err)
	}
	defer rows.Close()

	seen := map[string]bool{}
	var ids []string
	var batches []string
	for rows.Next() {
		var id, batch string
		if err := rows.Scan(&id, &batch); err != nil {
			return nil, fmt.Errorf("scan claimable outbox event: %w", err)
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
		if batch != "" {
			batches = append(batches, batch)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claimable outbox events: %w", err)
	}

	for _, batch := range batches {
		siblings, err := s.claimableBatchSiblings(batch, leaseCutoff)
		if err != nil {
			return nil, err
		}
		for _, id := range siblings {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids, nil
}

func (s *Store) claimableBatchSiblings(batchID, leaseCutoff string) ([]string, error) {
	rows, err := s.db.Query(s.q(`
		SELECT id
		FROM event_outbox
		WHERE batch_id = ?
		  AND dispatched_at IS NULL
		  AND (claimed_at IS NULL OR claimed_at < ?)
	`), batchID, leaseCutoff)
	if err != nil {
		return nil, fmt.Errorf("list batch siblings: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan batch sibling: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate batch siblings: %w", err)
	}
	return ids, nil
}

func (s *Store) outboxEventsClaimedBy(token string) ([]OutboxEvent, error) {
	rows, err := s.db.Query(s.q(`
		SELECT id, workspace_id, event_type, subject_kind, COALESCE(subject_id, ''), payload, hop, occurred_at, attempts, COALESCE(last_error, ''), COALESCE(batch_id, '')
		FROM event_outbox
		WHERE claimed_by = ? AND dispatched_at IS NULL
		ORDER BY occurred_at, id
	`), token)
	if err != nil {
		return nil, fmt.Errorf("read claimed outbox events: %w", err)
	}
	defer rows.Close()

	var out []OutboxEvent
	for rows.Next() {
		var ev OutboxEvent
		var payload string
		if err := rows.Scan(&ev.ID, &ev.WorkspaceID, &ev.EventType, &ev.SubjectKind, &ev.SubjectID,
			&payload, &ev.Hop, &ev.OccurredAt, &ev.Attempts, &ev.LastError, &ev.BatchID); err != nil {
			return nil, fmt.Errorf("scan claimed outbox event: %w", err)
		}
		ev.Payload = []byte(payload)
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claimed outbox events: %w", err)
	}
	return out, nil
}

// bulkHeaderPayload is the handler-path batch HEADER: what the operation was,
// which items it touched, and the delta they share.
//
// The three legacy keys (op / count / item_ids) are the shape today's webhook
// consumers already receive, kept verbatim so the drain's arrival is not also
// a payload rename. batch_id and delta are additions: batch_id because
// multi-instance claiming can in principle split one operation across two wire
// events and a consumer needs to correlate them (SPEC-3 v1.6 demotes "one wire
// event per batch" to the normal case for exactly this reason), and delta
// because SPEC-3's item_batch payload names it and the handler is the only
// place that knows it.
//
// NO SNAPSHOTS HERE. They live on the member rows until the drain folds them
// in — which is the honest shape, not a gap: at header-write time the loop has
// already committed each member's own event, and re-reading every row to
// duplicate them into the header would double the storage for the same facts
// and give the two copies a chance to disagree.
type bulkHeaderPayload struct {
	BatchID   string            `json:"batch_id"`
	Op        string            `json:"op"`
	Count     int               `json:"count"`
	ItemIDs   []string          `json:"item_ids"`
	Delta     map[string]any    `json:"delta,omitempty"`
	MemberSet []json.RawMessage `json:"members,omitempty"`
}

// EmitBulkHeaderEvent writes the batch header for one handler-path bulk
// operation, in its own transaction after the member loop.
//
// WHY THIS IS NOT A TRANSACTIONAL EMIT, said plainly because the rest of this
// file argues the opposite for everything else: a handler-path bulk mutation
// has no enclosing transaction to write it in. Each member committed
// separately, carrying its own canonical event, and that is what keeps
// per-member binding evaluation free. The header is delivery-side aggregation
// (SPEC-3 v1.6), and its failure mode is bounded accordingly — if the process
// dies between the members committing and this landing, the members simply
// deliver individually. A flood in the crash case, never a loss.
//
// Single-workspace by construction: the bulk endpoint is workspace-scoped, so
// unlike emitBulkItemEventTx there is no member set that could span workspaces
// and nothing to partition.
func (s *Store) EmitBulkHeaderEvent(workspaceID, batchID, op string, itemIDs []string, delta map[string]any) error {
	if batchID == "" {
		return fmt.Errorf("outbox: bulk header has no batch id")
	}
	// A bulk operation that changed nothing is not an event, same rule as the
	// transactional bulk emitter.
	if len(itemIDs) == 0 {
		return nil
	}
	payload, err := marshalEventPayload(bulkHeaderPayload{
		BatchID: batchID,
		Op:      op,
		Count:   len(itemIDs),
		ItemIDs: itemIDs,
		Delta:   delta,
	})
	if err != nil {
		return err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("emit bulk header: %w", err)
	}
	defer tx.Rollback()
	if err := writeOutboxTx(tx, s, OutboxEvent{
		WorkspaceID:   workspaceID,
		EventType:     kernelevents.ItemBulkUpdated,
		SubjectID:     batchID,
		Payload:       payload,
		PayloadFamily: kernelevents.PayloadItemBatchHeader,
		BatchID:       batchID,
	}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("emit bulk header: %w", err)
	}
	return nil
}

// FoldBulkHeader returns the wire payload for a folded batch: the stored
// header enriched with the member snapshots the drain claimed alongside it.
//
// The fold lives here, next to the shape it produces, so the drain does not
// have to know how a header is spelled. It takes the members it was GIVEN
// rather than reading them back: SPEC-3 v1.6 defines the fold as "header plus
// whatever member rows of that batch are still undispatched", so the set the
// drain holds IS the answer — re-reading would quietly re-include members a
// previous tick already delivered individually.
//
// Member payloads are embedded VERBATIM. They are already the exact snapshots
// the single-item events carry, and re-marshalling them through a Go type here
// would let the batch and single views of one mutation drift apart.
func FoldBulkHeader(header []byte, members [][]byte) ([]byte, error) {
	var h bulkHeaderPayload
	if err := json.Unmarshal(header, &h); err != nil {
		return nil, fmt.Errorf("fold bulk header: %w", err)
	}
	for _, m := range members {
		h.MemberSet = append(h.MemberSet, json.RawMessage(m))
	}
	return marshalEventPayload(h)
}
