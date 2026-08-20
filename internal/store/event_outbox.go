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

	// Attempts and LastError are drain bookkeeping, populated on read.
	Attempts  int
	LastError string
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
	if ev.OccurredAt == "" {
		ev.OccurredAt = now()
	}
	if ev.SubjectKind == "" {
		kind, ok := kernelevents.SubjectKind(ev.EventType)
		if !ok {
			// Unreachable: IsCanonical above already established membership,
			// and both answers come from the same map. Guarded anyway so a
			// future split of those two lookups cannot silently write rows
			// with an empty subject_kind.
			return fmt.Errorf("outbox: no subject kind for canonical event %q", ev.EventType)
		}
		ev.SubjectKind = kind
	}

	_, err := tx.Exec(s.q(`
		INSERT INTO event_outbox (id, workspace_id, event_type, subject_kind, subject_id, payload, hop, occurred_at)
		VALUES (?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?)
	`), ev.ID, ev.WorkspaceID, ev.EventType, ev.SubjectKind, ev.SubjectID, string(ev.Payload), ev.Hop, ev.OccurredAt)
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
// status change and item.status_changed fires for it. With omitempty on a
// plain string that transition dropped the key entirely, leaving a predicate
// unable to tell "the prior status was empty" from "this event has no prior
// status" (Codex round 6). My original reasoning — that an empty string should
// never appear "where a prior status is meaningless" — was right about the
// events where it IS meaningless and wrong about the one where it is not.
//
// So: nil on every event that has no prior status, and present-and-possibly-
// empty on item.status_changed, where the empty value is data.
type itemEventPayload struct {
	*models.Item
	PriorStatus *string `json:"prior_status,omitempty"`
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
func (s *Store) emitItemEventTx(tx *sql.Tx, eventType string, item *models.Item, priorStatus *string) error {
	if item == nil {
		return fmt.Errorf("outbox: %s has no item snapshot", eventType)
	}
	payload, err := marshalEventPayload(itemEventPayload{Item: item, PriorStatus: priorStatus})
	if err != nil {
		return err
	}
	return writeOutboxTx(tx, s, OutboxEvent{
		WorkspaceID: item.WorkspaceID,
		EventType:   eventType,
		SubjectID:   item.ID,
		Payload:     payload,
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
		WorkspaceID: comment.WorkspaceID,
		EventType:   eventType,
		SubjectID:   comment.ID,
		Payload:     payload,
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
		WorkspaceID: a.WorkspaceID,
		EventType:   eventType,
		SubjectID:   a.ID,
		Payload:     payload,
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
		WorkspaceID: workspaceID,
		EventType:   eventType,
		SubjectID:   userID,
		Payload:     payload,
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
			WorkspaceID: ws,
			EventType:   kernelevents.ItemBulkUpdated,
			Payload:     payload,
		}); err != nil {
			return err
		}
	}
	return nil
}

// itemSnapshotsTx reads the given items in-tx, de-duplicated, skipping any
// that no longer resolve.
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
func (s *Store) itemSnapshotsTx(tx *sql.Tx, ids []string) ([]*models.Item, error) {
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
		out = append(out, item)
	}
	return out, nil
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
func (s *Store) emitItemUpdateEventsTx(tx *sql.Tx, before, after *models.Item, statusChanged bool, priorStatus, statusKey string) error {
	if statusChanged {
		// Taken by address unconditionally: an empty prior status is a real
		// prior status here, not an absent one.
		prior := priorStatus
		if err := s.emitItemEventTx(tx, kernelevents.ItemStatusChanged, after, &prior); err != nil {
			return err
		}
	}

	otherChanged, err := itemUpdatedSliceChanged(before, after, statusKey)
	if err != nil {
		return err
	}
	if otherChanged {
		if err := s.emitItemEventTx(tx, kernelevents.ItemUpdated, after, nil); err != nil {
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
		SELECT id, workspace_id, event_type, subject_kind, COALESCE(subject_id, ''), payload, hop, occurred_at, attempts, COALESCE(last_error, '')
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
			&payload, &ev.Hop, &ev.OccurredAt, &ev.Attempts, &ev.LastError); err != nil {
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
func (s *Store) MarkOutboxAttemptFailed(id, reason string) error {
	if _, err := s.db.Exec(s.q(`
		UPDATE event_outbox SET attempts = attempts + 1, last_error = ? WHERE id = ? AND dispatched_at IS NULL
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
