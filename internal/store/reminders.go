package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PerpetualSoftware/pad/internal/kernelevents"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// Item reminders — the fire-at-an-instant primitive (IDEA-2641, GitHub #1010).
//
// See migration 085 for why this is a table rather than an annotation on a
// schema field, and models.Reminder for the three-state lifecycle.

const reminderColumns = `id, workspace_id, item_id, remind_at, fired_at, acked_at, created_at, updated_at`

// defaultReminderFireLimit bounds one tick's work. Reminders arrive at a rate
// set by users arming them, not by traffic, so a tick has no reason to be
// large — but a backlog is possible after downtime, and an unbounded pass
// would try to fire every overdue reminder in one transaction storm. The
// remainder is not lost: it is still armed, and the next tick takes the next
// batch, oldest first.
const defaultReminderFireLimit = 100

// defaultPendingReminderLimit bounds the poll surface's window.
//
// THE RECEIPT: this is a NOTIFICATION list a human or an agent reads at a
// glance, not a queue to drain, so the bound is set by what is worth showing
// rather than by what the database can return. Fifty unacknowledged reminders
// already means the surface is not being used as intended; showing five
// hundred would not help, and the payload is embedded in every dashboard
// response, which is the hottest read in the product. The truncation is
// REPORTED rather than silent, so a caller that genuinely has more can tell.
const defaultPendingReminderLimit = 50

func scanReminder(row interface{ Scan(...any) error }) (*models.Reminder, error) {
	var r models.Reminder
	var firedAt, ackedAt sql.NullString
	if err := row.Scan(&r.ID, &r.WorkspaceID, &r.ItemID, &r.RemindAt, &firedAt, &ackedAt, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	if firedAt.Valid {
		r.FiredAt = &firedAt.String
	}
	if ackedAt.Valid {
		r.AckedAt = &ackedAt.String
	}
	return &r, nil
}

// normalizeRemindAt re-parses and re-formats an instant, and REFUSES anything
// that is not RFC3339.
//
// The HTTP edge already normalizes, so on that path this is a second check of
// a value that is already correct — and it is here anyway, because the
// alternative was a doc comment saying "the caller normalizes", which protects
// nothing: the store is callable from anywhere in the process and a doc
// comment does not travel with the argument. Enforcing it means the stored
// value is always machine-produced from a parsed time, so no caller bytes
// reach the column at all. That is what lets remind_at sit outside the NUL
// census's protected set on a positive argument rather than an assumption.
func normalizeRemindAt(remindAt string) (string, error) {
	t, err := time.Parse(time.RFC3339, remindAt)
	if err != nil {
		return "", fmt.Errorf("remind_at must be an RFC3339 instant: %w", err)
	}
	return NormalizeInstant(t), nil
}

// CreateReminder arms a reminder on an item.
func (s *Store) CreateReminder(workspaceID, itemID, remindAt string) (*models.Reminder, error) {
	remindAt, err := normalizeRemindAt(remindAt)
	if err != nil {
		return nil, err
	}
	id := newID()
	ts := now()
	_, err = s.db.Exec(s.q(`
		INSERT INTO item_reminders (id, workspace_id, item_id, remind_at, fired_at, acked_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, NULL, NULL, ?, ?)
	`), id, workspaceID, itemID, remindAt, ts, ts)
	if err != nil {
		return nil, fmt.Errorf("create reminder: %w", err)
	}
	return s.GetReminder(workspaceID, id)
}

// GetReminder returns one reminder scoped to a workspace, or (nil, nil) when
// no such row exists.
//
// WORKSPACE-SCOPED ON PURPOSE, even though the id is a UUID and collisions are
// not the concern: an unscoped lookup would answer "does this id exist" for
// every workspace on the instance, which is the existence-oracle shape a
// sibling handler family already had to be fixed for. The caller has the
// workspace; requiring it costs nothing.
func (s *Store) GetReminder(workspaceID, id string) (*models.Reminder, error) {
	row := s.db.QueryRow(s.q(`SELECT `+reminderColumns+` FROM item_reminders WHERE id = ? AND workspace_id = ?`), id, workspaceID)
	r, err := scanReminder(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get reminder: %w", err)
	}
	return r, nil
}

// ListRemindersForItem returns every reminder on an item, armed or not,
// soonest first. History is included because a fired reminder is the record
// that a reminder existed and went out.
func (s *Store) ListRemindersForItem(itemID string) ([]*models.Reminder, error) {
	rows, err := s.db.Query(s.q(`SELECT `+reminderColumns+` FROM item_reminders WHERE item_id = ? ORDER BY remind_at, id`), itemID)
	if err != nil {
		return nil, fmt.Errorf("list reminders: %w", err)
	}
	defer rows.Close()

	var out []*models.Reminder
	for rows.Next() {
		r, err := scanReminder(rows)
		if err != nil {
			return nil, fmt.Errorf("scan reminder: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reminders: %w", err)
	}
	return out, nil
}

// RearmReminder moves a reminder's instant and clears BOTH fire marks, so a
// reminder that already fired becomes armed again.
//
// The clear is unconditional rather than "only when fired_at is set" because
// the two cases must not diverge: on an armed row the marks are already NULL
// and the write is a no-op, and making it conditional would create a path
// where a re-arm leaves a stale acked_at behind on a row that is armed —
// a state models.Reminder's lifecycle does not have a name for.
func (s *Store) RearmReminder(workspaceID, id, remindAt string) (*models.Reminder, error) {
	remindAt, err := normalizeRemindAt(remindAt)
	if err != nil {
		return nil, err
	}
	res, err := s.db.Exec(s.q(`
		UPDATE item_reminders
		SET remind_at = ?, fired_at = NULL, acked_at = NULL, updated_at = ?
		WHERE id = ? AND workspace_id = ?
	`), remindAt, now(), id, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("rearm reminder: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return nil, nil
	}
	return s.GetReminder(workspaceID, id)
}

// AckReminder acknowledges a FIRED reminder.
//
// The `fired_at IS NOT NULL` predicate is what makes acking an armed reminder
// impossible rather than merely discouraged: an acked-but-never-fired row
// would sit in a state the lifecycle has no name for, and it would be
// invisible — the poll surface reads fired-unacked, so the row would simply
// never appear again. Returns (nil, nil) when nothing matched, which the
// handler distinguishes from a missing reminder by re-reading.
//
// The ack is IDEMPOTENT by leaving acked_at alone once set: a second ack finds
// no matching row and reports nothing changed, rather than moving the
// timestamp and rewriting when the acknowledgement happened.
func (s *Store) AckReminder(workspaceID, id string) (*models.Reminder, error) {
	res, err := s.db.Exec(s.q(`
		UPDATE item_reminders
		SET acked_at = ?, updated_at = ?
		WHERE id = ? AND workspace_id = ? AND fired_at IS NOT NULL AND acked_at IS NULL
	`), now(), now(), id, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("ack reminder: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return nil, nil
	}
	return s.GetReminder(workspaceID, id)
}

// DeleteReminder removes a reminder outright. Disarming by deletion is the
// only disarm: there is no "cancelled" state, because a cancelled reminder and
// an absent one are indistinguishable to every surface that reads them.
func (s *Store) DeleteReminder(workspaceID, id string) (bool, error) {
	res, err := s.db.Exec(s.q(`DELETE FROM item_reminders WHERE id = ? AND workspace_id = ?`), id, workspaceID)
	if err != nil {
		return false, fmt.Errorf("delete reminder: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, nil
	}
	return n > 0, nil
}

// ListPendingReminders returns a workspace's fired-and-unacked reminders,
// joined to the items they are about, soonest-fired first.
//
// This is the AGENT POLL SURFACE, and it is not optional. The outbox drain
// acks an event immediately when no webhook dispatcher is configured — the
// common self-hosted shape — so a webhook-only reminder would be a no-op on
// most installs. The row this query returns is the only thing that survives
// on those instances.
//
// Terminal-item filtering happens in the CALLER, not here, because terminality
// is schema-defined (a collection's terminal_options) and lives in JSON the
// SQL layer would have to parse. The caller already builds that context for
// the dashboard; ItemFields and CollectionID are carried for it.
// PendingReminderScope narrows the query to what a caller may see. Nil
// CollectionIDs means unrestricted; a NON-NIL EMPTY slice with no ItemIDs
// means nothing is visible, matching models.ItemListParams so the two
// visibility paths cannot drift into opposite readings of the same value.
type PendingReminderScope struct {
	CollectionIDs []string
	ItemIDs       []string
}

// The window is BOUNDED because this list feeds a payload that is otherwise
// capped: every pending reminder became a suggestion prepended to a
// three-entry list, so a workspace with five hundred unacknowledged reminders
// returned five hundred suggestions and grew without limit until somebody
// acknowledged them (codex round 3). Oldest-fired first, so the window holds
// the reminders that have been waiting longest rather than an arbitrary slice.
//
// The caller is told when the window was not the whole set — but as a BOOLEAN,
// not a count. A count would have to be stated post-visibility-filter to be
// true for the caller reading it, and this query cannot compute that: the
// filter runs above, per item. "There are more than you can see here" is the
// strongest claim the data supports, so it is the one made.
// VISIBILITY IS SCOPED IN SQL, not filtered afterwards, and that is the
// round-4 correction. The round-3 bound took the first N rows and let the
// caller discard the ones it could not show — which recreated, in the READ
// path, the exact starvation the round-1 fix removed from the FIRE path:
// fifty rows the caller must drop can hide a visible reminder behind them
// forever, with no continuation to reach it. A bounded window is only safe
// when the discarding happens BEFORE the bound.
//
// One filter necessarily stays above: terminality is defined by a collection's
// schema, which SQL cannot read. That one is handled by paging (the caller
// asks for the next page when a page comes back short), which is why this
// takes an offset at all.
func (s *Store) ListPendingReminders(workspaceID string, scope PendingReminderScope, limit, offset int) ([]*models.PendingReminder, bool, error) {
	if limit <= 0 {
		limit = defaultPendingReminderLimit
	}
	if offset < 0 {
		offset = 0
	}
	// Nothing visible at all: answer without touching the database, and say
	// there is no more — a truncation flag here would send the caller paging
	// through a set it can never see into.
	if scope.CollectionIDs != nil && len(scope.CollectionIDs) == 0 && len(scope.ItemIDs) == 0 {
		return nil, false, nil
	}
	query := `
		SELECT r.id, r.workspace_id, r.item_id, r.remind_at, r.fired_at, r.acked_at, r.created_at, r.updated_at,
		       i.slug, i.title, i.fields, i.collection_id, c.slug, c.prefix, i.item_number
		FROM item_reminders r
		JOIN items i ON i.id = r.item_id
		JOIN collections c ON c.id = i.collection_id
		WHERE r.workspace_id = ?
		  AND r.fired_at IS NOT NULL
		  AND r.acked_at IS NULL
		  AND i.deleted_at IS NULL`
	args := []any{workspaceID}

	// Same three-way shape as models.ItemListParams: a guest may hold
	// collection-level grants, item-level grants, or both, and "both" is an OR
	// rather than an AND — an item in a fully granted collection qualifies
	// even when it is not individually granted.
	switch {
	case len(scope.CollectionIDs) > 0 && len(scope.ItemIDs) > 0:
		query += " AND (i.collection_id IN (" + placeholders(len(scope.CollectionIDs)) +
			") OR i.id IN (" + placeholders(len(scope.ItemIDs)) + "))"
		for _, id := range scope.CollectionIDs {
			args = append(args, id)
		}
		for _, id := range scope.ItemIDs {
			args = append(args, id)
		}
	case len(scope.CollectionIDs) > 0:
		query += " AND i.collection_id IN (" + placeholders(len(scope.CollectionIDs)) + ")"
		for _, id := range scope.CollectionIDs {
			args = append(args, id)
		}
	case len(scope.ItemIDs) > 0:
		query += " AND i.id IN (" + placeholders(len(scope.ItemIDs)) + ")"
		for _, id := range scope.ItemIDs {
			args = append(args, id)
		}
	}

	query += " ORDER BY r.fired_at, r.id LIMIT ? OFFSET ?"
	args = append(args, limit+1, offset)

	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		return nil, false, fmt.Errorf("list pending reminders: %w", err)
	}
	defer rows.Close()

	var out []*models.PendingReminder
	for rows.Next() {
		var p models.PendingReminder
		var firedAt, ackedAt sql.NullString
		var prefix string
		var number int
		if err := rows.Scan(
			&p.ID, &p.WorkspaceID, &p.ItemID, &p.RemindAt, &firedAt, &ackedAt, &p.CreatedAt, &p.UpdatedAt,
			&p.ItemSlug, &p.ItemTitle, &p.ItemFields, &p.CollectionID, &p.CollectionSlug, &prefix, &number,
		); err != nil {
			return nil, false, fmt.Errorf("scan pending reminder: %w", err)
		}
		if firedAt.Valid {
			p.FiredAt = &firedAt.String
		}
		if ackedAt.Valid {
			p.AckedAt = &ackedAt.String
		}
		p.ItemRef = fmt.Sprintf("%s-%d", prefix, number)
		out = append(out, &p)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate pending reminders: %w", err)
	}
	// The extra row is the probe, never a result.
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// dueReminderCandidates returns the ids of armed reminders whose instant has
// arrived, oldest first.
//
// Split from FireDueReminders so a test can drive the arbiter below with a
// deliberately STALE candidate list — the race this shape exists for. Through
// the public entry point that race is unobservable, because this query has
// already filtered the rows it is about to hand over. Same split, and the same
// reason, as the outbox claim's pendingClaimCandidates / claimOutboxIDs.
func (s *Store) dueReminderCandidates(nowTS string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = defaultReminderFireLimit
	}
	// SOFT-DELETED ITEMS ARE EXCLUDED HERE, not merely skipped downstream
	// (codex round 1). fireOneReminder rolls back when it finds the item gone,
	// which leaves the reminder ARMED and therefore a candidate again on the
	// next pass — so a batch bounded at `limit` and ordered oldest-first can be
	// filled entirely by archived items, and no live reminder ever fires. The
	// starvation is permanent and silent: the tick reports zero fired and looks
	// idle. Filtering in the candidate query means those rows never occupy a
	// slot, while the reminders themselves are kept, so restoring the item
	// restores its reminder with it.
	rows, err := s.db.Query(s.q(`
		SELECT r.id FROM item_reminders r
		JOIN items i ON i.id = r.item_id
		WHERE r.fired_at IS NULL AND r.remind_at <= ? AND i.deleted_at IS NULL
		ORDER BY r.remind_at, r.id
		LIMIT ?
	`), nowTS, limit)
	if err != nil {
		return nil, fmt.Errorf("due reminder candidates: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan due reminder id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate due reminders: %w", err)
	}
	return ids, nil
}

// FireDueReminders marks every arrived reminder as fired and writes its event,
// returning the reminders this pass actually fired.
//
// ONE TRANSACTION PER REMINDER, carrying both the fired_at write and the
// outbox insert. That pairing is the whole point and it is not an efficiency
// choice: a fired_at committed without its event is a reminder that silently
// never notifies anyone and can never be retried, because the row has left the
// armed set. An event committed without fired_at fires again every tick. The
// transaction is what makes both unrepresentable — the same discipline the
// outbox itself exists to provide for ordinary mutations.
//
// Per-reminder rather than per-batch so one unfireable row (a deleted item
// racing the tick, a payload that will not marshal) cannot hold back every
// other reminder in the pass.
//
// The UPDATE re-checks BOTH the fire mark and the instant, so it arbitrates
// against two different actors — and getting only the first was the round-3
// defect. Against a concurrent TICK, `fired_at IS NULL` means both instances
// see the same candidate and exactly one gets RowsAffected 1; the loser does
// no work and emits nothing. Against a concurrent USER, `remind_at <= nowTS`
// means a reminder deferred between the scan and the fire is not fired — which
// the fire mark alone could not catch, because a re-arm CLEARS that mark.
//
// The distinction is worth keeping in view: an arbiter is only an arbiter with
// respect to the writers it can see, and this one was written with ticks in
// mind while a user edit went straight past it.
func (s *Store) FireDueReminders(nowTS string, limit int) ([]*models.Reminder, error) {
	ids, err := s.dueReminderCandidates(nowTS, limit)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}

	// ONE FAILURE DOES NOT END THE PASS (codex round 1). The per-reminder
	// transaction above exists precisely so that one unfireable row cannot
	// hold back the rest — and returning on the first error made that comment
	// false, since candidates are ordered oldest-first and a persistently
	// broken old reminder would then block every newer one forever. The errors
	// are collected rather than dropped: a pass that failed on three rows and
	// fired seven must report both halves, or the tick's log reads like a
	// clean pass.
	return fireEachReminder(ids, nowTS, s.fireOneReminder)
}

// fireEachReminder is the pass's isolation property, split out so a test can
// inject a failing fire for one id and observe that the ids after it still
// run. Through the public entry point that is not reachable: making a real
// reminder fail mid-transaction requires corrupting a row the database
// refuses to store corrupt. Same split, and the same reason, as
// dueReminderCandidates / fireOneReminder.
func fireEachReminder(ids []string, nowTS string, fire func(id, nowTS string) (*models.Reminder, error)) ([]*models.Reminder, error) {
	var fired []*models.Reminder
	var errs []error
	for _, id := range ids {
		r, err := fire(id, nowTS)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if r != nil {
			fired = append(fired, r)
		}
	}
	return fired, errors.Join(errs...)
}

// fireOneReminder is the arbiter plus the emission, in one transaction.
// Returns (nil, nil) when another pass won the row or its item is gone.
func (s *Store) fireOneReminder(id, nowTS string) (*models.Reminder, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("fire reminder: %w", err)
	}
	defer tx.Rollback()

	// THE INSTANT IS REVALIDATED HERE, not just the fire mark (codex round 3).
	// A re-arm can move this reminder into the future between the candidate
	// scan and this UPDATE — it clears fired_at, so a predicate that checked
	// only `fired_at IS NULL` still matched, and the pass fired a reminder the
	// user had just deferred and emitted its event. The re-arm cannot undo
	// that: it can clear the mark, but the event is already on the outbox.
	//
	// Same nowTS the candidate scan used, deliberately: the arbiter and the
	// scan must agree about when this pass is, or a reminder could pass one
	// and fail the other for no reason but clock drift within the pass.
	res, err := tx.Exec(s.q(`
		UPDATE item_reminders SET fired_at = ?, updated_at = ?
		WHERE id = ? AND fired_at IS NULL AND remind_at <= ?
	`), nowTS, now(), id, nowTS)
	if err != nil {
		return nil, fmt.Errorf("fire reminder %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("fire reminder %s: %w", id, err)
	}
	if n == 0 {
		// Another instance's tick won this row. Not an error, and emitting
		// nothing is the correct outcome: the winner emits.
		return nil, nil
	}

	row := tx.QueryRow(s.q(`SELECT `+reminderColumns+` FROM item_reminders WHERE id = ?`), id)
	r, err := scanReminder(row)
	if err != nil {
		return nil, fmt.Errorf("reload fired reminder %s: %w", id, err)
	}

	// READ THE ITEM ON THE TRANSACTION, never on s.db. A pool read inside a
	// transaction that holds a write lock deadlocks on a single-connection
	// pool, which is exactly how this store is configured under SQLite.
	item, err := s.GetItemQ(tx, r.ItemID)
	if err != nil {
		return nil, fmt.Errorf("load item for reminder %s: %w", id, err)
	}
	if item == nil {
		// The item was soft-deleted between the candidate scan and now.
		// Roll the fire back rather than emitting an event about an item a
		// consumer cannot fetch: the deferred Rollback does it, and the
		// reminder stays armed. A hard delete cascades the row away instead.
		return nil, nil
	}

	if err := s.emitReminderEventTx(tx, r, item); err != nil {
		return nil, fmt.Errorf("emit reminder event %s: %w", id, err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("fire reminder %s: %w", id, err)
	}
	return r, nil
}

// reminderEventPayload is the PayloadReminder shape: the reminder that fired,
// and the item it is about.
//
// The item is a scrubbed snapshot, matching every other item-carrying payload
// — a reminder event travels the same webhook wire as item.created and must
// not be the one door that ships PII the others strip.
type reminderEventPayload struct {
	Reminder *models.Reminder `json:"reminder"`
	Item     *models.Item     `json:"item"`
}

// emitReminderEventTx writes one item.reminder_due event on the caller's
// transaction.
func (s *Store) emitReminderEventTx(tx *sql.Tx, r *models.Reminder, item *models.Item) error {
	if r == nil || item == nil {
		return fmt.Errorf("outbox: %s has no reminder or item snapshot", kernelevents.ItemReminderDue)
	}
	payload, err := marshalEventPayload(reminderEventPayload{Reminder: r, Item: scrubItemPII(item)})
	if err != nil {
		return err
	}
	return writeOutboxTx(tx, s, OutboxEvent{
		WorkspaceID:   r.WorkspaceID,
		EventType:     kernelevents.ItemReminderDue,
		SubjectID:     r.ID,
		Payload:       payload,
		PayloadFamily: kernelevents.PayloadReminder,
	})
}

// NormalizeInstant renders a parsed time as the RFC3339 second it must not
// fire before, in UTC.
//
// SECONDS ARE THE STORED RESOLUTION: the column is compared as a string
// against a whole-second clock, and the tick runs on a 30s interval, so
// sub-second precision is not a thing this system can honour. The question is
// only which way to resolve it, and truncating was wrong (codex round 2):
// `09:00:00.900Z` truncated to `09:00:00Z` fires 900ms BEFORE the moment the
// caller named, and it does so silently, having rewritten their value on the
// way in.
//
// Rounding UP costs at most a second of lateness and makes the guarantee
// stateable: a reminder never fires before the instant it was set for. Late is
// a reminder; early is a wrong answer.
//
// Whole seconds are unchanged, so the ordinary case round-trips exactly.
func NormalizeInstant(t time.Time) string {
	u := t.UTC()
	if trunc := u.Truncate(time.Second); !trunc.Equal(u) {
		u = trunc.Add(time.Second)
	}
	return u.Format(time.RFC3339)
}
