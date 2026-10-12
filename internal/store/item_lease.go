package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Item execution lease (#1221): an atomic claim/checkout so two pollers
// that both read "unclaimed" cannot both proceed. The conditional UPDATE
// is the arbiter — the same protocol the event-outbox claim (TASK-2714)
// and orphan GC (BUG-2415) established: the predicate on the UPDATE
// decides the winner, so a race produces one winner and one typed
// refusal, never two winners.
//
// Expiry is the reaper: an expired lease is treated as absent by the
// claim predicate and by every read, so a crashed holder strands nothing
// and no sweep job exists. Timestamps are RFC3339 whole-second UTC TEXT,
// the items-table convention, so string comparison in SQL and time
// comparison in Go agree.

// LeaseHeldError reports a claim or release refused because another
// holder's lease is live. It names the holder and expiry so the caller
// can decide to wait, skip, or escalate rather than guessing.
type LeaseHeldError struct {
	Holder string
	// Worker is the live lease's SHORT worker id ("wk_3f2a"), or "" for a
	// name-only lease (TASK-3613).
	Worker     string
	AcquiredAt time.Time
	ExpiresAt  time.Time
}

func (e *LeaseHeldError) Error() string {
	if e.Worker != "" {
		return fmt.Sprintf("item is leased to %q (%s) until %s", e.Holder, e.Worker, e.ExpiresAt.Format(time.RFC3339))
	}
	return fmt.Sprintf("item is leased to %q until %s", e.Holder, e.ExpiresAt.Format(time.RFC3339))
}

// ClaimItemLease atomically acquires (or, for the live holder, refreshes)
// the execution lease on an item. It succeeds iff no live lease exists or
// the caller already holds it; a refresh moves the expiry forward but
// keeps the original acquired_at — extending is not re-acquiring. A
// negative ttl writes an already-expired lease (used by tests to model a
// crashed holder; the handler bounds ttl before it gets here).
//
// On contention it returns *LeaseHeldError naming the live holder.
// userID is the authenticated claimer (BUG-3341). A lease is "ours" when its
// label matches AND it was claimed by this user, or by nobody recorded (a
// lease from before migration 104, honoured on its label alone until it
// expires). A claim stamps userID, so refreshing a legacy lease binds it.
// Empty userID stores NULL: the label-only behaviour, for callers with no
// signed-in user.
//
// workerID (TASK-3613) is the caller's opaque session identity, or "" for a
// name-only caller; leaseOurs says what it matches. A claim stamps it, so a
// worker refreshing a name-only lease of its own label binds that lease.
func (s *Store) ClaimItemLease(itemID, holder, workerID, userID string, ttl time.Duration) (*models.ItemLease, error) {
	ours, oursArgs, err := leaseOurs(holder, workerID, userID)
	if err != nil {
		return nil, err
	}
	nowStr := now()
	expiresStr := time.Now().UTC().Add(ttl).Format(time.RFC3339)
	uid := leaseUserArg(userID)

	// The predicate is the arbiter: unclaimed, expired, or already ours.
	// All SET expressions read the PRE-update row (standard SQL, both
	// dialects), so the CASE sees the old holder/expiry.
	args := append([]any{}, oursArgs...)
	args = append(args, nowStr, nowStr, holder, leaseWorkerArg(workerID), uid, expiresStr, itemID, nowStr)
	args = append(args, oursArgs...)
	res, err := s.db.Exec(s.q(`
		UPDATE items SET
			lease_acquired_at = CASE
				WHEN `+ours+` AND lease_expires_at > ? THEN lease_acquired_at
				ELSE ?
			END,
			lease_holder = ?,
			lease_worker_id = ?,
			lease_user_id = ?,
			lease_expires_at = ?
		WHERE id = ?
		  AND (lease_holder IS NULL OR lease_expires_at IS NULL OR lease_expires_at <= ? OR `+ours+`)
	`), args...)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 1 {
		return s.readItemLeaseRow(itemID)
	}

	// Lost the race (or the item id doesn't exist). Read the live lease
	// for the refusal; if it expired or was released between our UPDATE
	// and this read, the caller's retry will win — say so.
	lease, err := s.GetItemLease(itemID)
	if err != nil {
		return nil, err
	}
	if lease == nil {
		return nil, fmt.Errorf("claim on item %s did not apply and no live lease exists — item missing or lease released mid-claim; retry", itemID)
	}
	return nil, &LeaseHeldError{Holder: lease.Holder, Worker: lease.Worker, AcquiredAt: lease.AcquiredAt, ExpiresAt: lease.ExpiresAt}
}

// ReleaseItemLease clears the caller's lease on an item. Idempotent:
// releasing an absent or expired lease is a no-op (released=false), never
// an error — cleanup code must not special-case "did I still hold this".
// Releasing another holder's LIVE lease is refused with *LeaseHeldError.
func (s *Store) ReleaseItemLease(itemID, holder, workerID, userID string) (bool, error) {
	// Only the lease's own holder may release it, by leaseOurs (BUG-3341,
	// TASK-3613).
	ours, oursArgs, err := leaseOurs(holder, workerID, userID)
	if err != nil {
		return false, err
	}
	res, err := s.db.Exec(s.q(`
		UPDATE items SET lease_holder = NULL, lease_worker_id = NULL, lease_user_id = NULL, lease_acquired_at = NULL, lease_expires_at = NULL
		WHERE id = ? AND lease_holder IS NOT NULL AND `+ours+`
	`), append([]any{itemID}, oursArgs...)...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 1 {
		return true, nil
	}

	lease, err := s.GetItemLease(itemID)
	if err != nil {
		return false, err
	}
	// A live lease that is not ours: another label, or our label claimed by
	// another user. Either way the caller does not hold it.
	if lease != nil {
		return false, &LeaseHeldError{Holder: lease.Holder, Worker: lease.Worker, AcquiredAt: lease.AcquiredAt, ExpiresAt: lease.ExpiresAt}
	}
	return false, nil
}

// GetItemLease returns the item's live lease, or nil — absent and expired
// are indistinguishable on purpose (expiry is absence on every read path).
func (s *Store) GetItemLease(itemID string) (*models.ItemLease, error) {
	lease, err := s.readItemLeaseRow(itemID)
	if err != nil {
		return nil, err
	}
	if lease == nil || !lease.ExpiresAt.After(time.Now().UTC()) {
		return nil, nil
	}
	return lease, nil
}

// ListItemLeases returns every LIVE lease in a workspace, keyed by item
// id. Expired leases are filtered in SQL so the map carries only what a
// list rendering should decorate.
func (s *Store) ListItemLeases(workspaceID string) (map[string]models.ItemLease, error) {
	rows, err := s.db.Query(s.q(`
		SELECT id, lease_holder, lease_worker_id, lease_acquired_at, lease_expires_at
		FROM items
		WHERE workspace_id = ? AND lease_holder IS NOT NULL AND lease_expires_at > ?
	`), workspaceID, now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	leases := make(map[string]models.ItemLease)
	for rows.Next() {
		var id, holder, acquiredAt, expiresAt string
		var workerID sql.NullString
		if err := rows.Scan(&id, &holder, &workerID, &acquiredAt, &expiresAt); err != nil {
			return nil, err
		}
		leases[id] = models.ItemLease{
			Holder:     holder,
			Worker:     models.ShortWorkerID(workerID.String),
			AcquiredAt: parseTime(acquiredAt),
			ExpiresAt:  parseTime(expiresAt),
		}
	}
	return leases, rows.Err()
}

// ListForeignItemLeaseIDs returns the ids of the items in a workspace whose
// LIVE lease a claim by (holder, workerID, userID) would be refused, i.e.
// every live lease that is not "ours" in ClaimItemLease's sense (TASK-1809).
// It is NOT leaseOurs, the claim's own predicate, so `pad project next` /
// `ready` hide exactly the items a claim would lose. leaseOurs is two-valued
// (every column is COALESCEd), which is what makes the negation safe.
func (s *Store) ListForeignItemLeaseIDs(workspaceID, holder, workerID, userID string) (map[string]struct{}, error) {
	ours, oursArgs, err := leaseOurs(holder, workerID, userID)
	if err != nil {
		return nil, err
	}
	query := `
		SELECT id FROM items
		WHERE workspace_id = ? AND lease_holder IS NOT NULL AND lease_expires_at > ?
		  AND NOT ` + ours
	args := append([]any{workspaceID, now()}, oursArgs...)
	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = struct{}{}
	}
	return ids, rows.Err()
}

// readItemLeaseRow reads the raw lease columns without the liveness
// filter — the claim path needs the row it just wrote even when a test
// wrote it pre-expired. Returns nil when the item has no lease columns
// set (or no such item exists).
func (s *Store) readItemLeaseRow(itemID string) (*models.ItemLease, error) {
	var holder, workerID, acquiredAt, expiresAt *string
	err := s.db.QueryRow(s.q(`
		SELECT lease_holder, lease_worker_id, lease_acquired_at, lease_expires_at FROM items WHERE id = ?
	`), itemID).Scan(&holder, &workerID, &acquiredAt, &expiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if holder == nil || expiresAt == nil {
		return nil, nil
	}
	lease := &models.ItemLease{Holder: *holder, ExpiresAt: parseTime(*expiresAt)}
	if workerID != nil {
		lease.Worker = models.ShortWorkerID(*workerID)
	}
	if acquiredAt != nil {
		lease.AcquiredAt = parseTime(*acquiredAt)
	}
	return lease, nil
}

// leaseUserArg is the SQL argument for a lease's user: NULL when there is no
// signed-in user, so the row keeps the label-only behaviour.
func leaseUserArg(userID string) any {
	if userID == "" {
		return nil
	}
	return userID
}

// ErrWorkerIDWithoutUser refuses a worker id with no signed-in user: a worker
// id is matched only together with the account (TASK-3613, lead ruling), so
// without one it would be an unscoped credential.
var ErrWorkerIDWithoutUser = errors.New("a lease worker id needs a signed-in user")

// ErrInvalidWorkerID refuses a malformed worker id before any SQL sees it,
// which is also what keeps lease_worker_id free of NULs (nulcolumns.go).
var ErrInvalidWorkerID = errors.New(`a lease worker id is "wk_" followed by 32 lowercase hex characters`)

// leaseOurs is THE predicate for "this live lease is the caller's", shared by
// claim, release and ListForeignItemLeaseIDs so they cannot drift. Every
// column is COALESCEd to ” and every argument is non-NULL, so the fragment
// is never NULL and its negation is exact.
//
//   - A lease with a worker id is ours iff the worker id AND the user match;
//     its label plays no part.
//   - A lease without one (a name-only claim, or one from before migration
//     142) is ours on its label, with the user matching or unrecorded
//     (BUG-3341), whether or not the caller has a worker id: that is how a
//     worker adopts its own name-only lease.
//   - So a name-only caller never matches a worker-bound lease, even under
//     the same label: the collision TASK-3613 closes.
func leaseOurs(holder, workerID, userID string) (string, []any, error) {
	if workerID != "" {
		if !models.ValidWorkerID(workerID) {
			return "", nil, ErrInvalidWorkerID
		}
		if userID == "" {
			return "", nil, ErrWorkerIDWithoutUser
		}
	}
	frag := `((COALESCE(lease_worker_id, '') <> '' AND COALESCE(lease_worker_id, '') = ? AND COALESCE(lease_user_id, '') = ?)
		OR (COALESCE(lease_worker_id, '') = '' AND COALESCE(lease_holder, '') = ? AND (COALESCE(lease_user_id, '') = '' OR COALESCE(lease_user_id, '') = ?)))`
	return frag, []any{workerID, userID, holder, userID}, nil
}

// leaseWorkerArg stores "" as NULL, so a name-only lease reads as one.
func leaseWorkerArg(workerID string) any {
	if workerID == "" {
		return nil
	}
	return workerID
}
