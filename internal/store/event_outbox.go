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
// Bounds SYNCHRONOUS cascades only, and deliberately so — a queued playbook
// run executed later, or a webhook consumer calling back through the API,
// legitimately starts fresh at hop 0. Pretending otherwise would be
// unenforceable, which is why SPEC-3 pairs this with per-pack quotas on
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
	if ev.Hop > maxOutboxHop {
		// Cascade bound reached. Dropping is the designed behaviour, but a
		// silent drop would make a runaway binding indistinguishable from a
		// binding that never fired, so the drop is the caller's to observe:
		// it is reported as a nil error with no row written, and the
		// dispatcher's quota accounting is what surfaces the pattern.
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
// which is what makes "nonterminal → terminal" filterable. Omitted entirely
// unless set, so it never appears as an empty string on events where a prior
// status is meaningless.
type itemEventPayload struct {
	*models.Item
	PriorStatus string `json:"prior_status,omitempty"`
}

// emitItemEventTx writes one item-subject event to the outbox on the caller's
// transaction, carrying the post-mutation snapshot.
//
// The snapshot must be read back INSIDE the transaction before this is called.
// A snapshot assembled from the caller's input rather than from the row is the
// bug this design exists to prevent: it would describe what the caller asked
// for, while the event claims to describe what happened.
func (s *Store) emitItemEventTx(tx *sql.Tx, eventType string, item *models.Item, priorStatus string) error {
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

// itemDeltaExcludedKeys are the snapshot keys that must not count as a change
// when deciding whether item.updated's slice moved.
//
// AN EXCLUSION LIST, NOT AN INCLUSION LIST, and that direction is the point. A
// column added to items later is included in the comparison by default, so the
// worst a future schema change can do is emit an extra item.updated — visible,
// arguable, fixable. An inclusion list would fail the other way: the new column
// would be silently invisible to the delta, and a mutation that changed it
// would emit nothing at all. Silent absence is the failure mode this whole unit
// exists to eliminate, so it must not be reintroduced by the mechanism that
// decides what to emit.
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
		if statusKey != "" {
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
				// An unparseable fields blob is left verbatim. It compares
				// byte-for-byte on both sides, so a corrupt blob still yields
				// a correct "changed / did not change" answer; it just cannot
				// have the status key masked out of it.
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
		if err := s.emitItemEventTx(tx, kernelevents.ItemStatusChanged, after, priorStatus); err != nil {
			return err
		}
	}

	otherChanged, err := itemUpdatedSliceChanged(before, after, statusKey)
	if err != nil {
		return err
	}
	if otherChanged {
		if err := s.emitItemEventTx(tx, kernelevents.ItemUpdated, after, ""); err != nil {
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
