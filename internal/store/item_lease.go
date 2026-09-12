package store

import (
	"database/sql"
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
	Holder     string
	AcquiredAt time.Time
	ExpiresAt  time.Time
}

func (e *LeaseHeldError) Error() string {
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
func (s *Store) ClaimItemLease(itemID, holder string, ttl time.Duration) (*models.ItemLease, error) {
	nowStr := now()
	expiresStr := time.Now().UTC().Add(ttl).Format(time.RFC3339)

	// The predicate is the arbiter: unclaimed, expired, or already ours.
	// All SET expressions read the PRE-update row (standard SQL, both
	// dialects), so the CASE sees the old holder/expiry.
	res, err := s.db.Exec(s.q(`
		UPDATE items SET
			lease_acquired_at = CASE
				WHEN lease_holder = ? AND lease_expires_at > ? THEN lease_acquired_at
				ELSE ?
			END,
			lease_holder = ?,
			lease_expires_at = ?
		WHERE id = ?
		  AND (lease_holder IS NULL OR lease_expires_at IS NULL OR lease_expires_at <= ? OR lease_holder = ?)
	`), holder, nowStr, nowStr, holder, expiresStr, itemID, nowStr, holder)
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
	return nil, &LeaseHeldError{Holder: lease.Holder, AcquiredAt: lease.AcquiredAt, ExpiresAt: lease.ExpiresAt}
}

// ReleaseItemLease clears the caller's lease on an item. Idempotent:
// releasing an absent or expired lease is a no-op (released=false), never
// an error — cleanup code must not special-case "did I still hold this".
// Releasing another holder's LIVE lease is refused with *LeaseHeldError.
func (s *Store) ReleaseItemLease(itemID, holder string) (bool, error) {
	res, err := s.db.Exec(s.q(`
		UPDATE items SET lease_holder = NULL, lease_acquired_at = NULL, lease_expires_at = NULL
		WHERE id = ? AND lease_holder = ?
	`), itemID, holder)
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
	if lease != nil && lease.Holder != holder {
		return false, &LeaseHeldError{Holder: lease.Holder, AcquiredAt: lease.AcquiredAt, ExpiresAt: lease.ExpiresAt}
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
		SELECT id, lease_holder, lease_acquired_at, lease_expires_at
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
		if err := rows.Scan(&id, &holder, &acquiredAt, &expiresAt); err != nil {
			return nil, err
		}
		leases[id] = models.ItemLease{
			Holder:     holder,
			AcquiredAt: parseTime(acquiredAt),
			ExpiresAt:  parseTime(expiresAt),
		}
	}
	return leases, rows.Err()
}

// readItemLeaseRow reads the raw lease columns without the liveness
// filter — the claim path needs the row it just wrote even when a test
// wrote it pre-expired. Returns nil when the item has no lease columns
// set (or no such item exists).
func (s *Store) readItemLeaseRow(itemID string) (*models.ItemLease, error) {
	var holder, acquiredAt, expiresAt *string
	err := s.db.QueryRow(s.q(`
		SELECT lease_holder, lease_acquired_at, lease_expires_at FROM items WHERE id = ?
	`), itemID).Scan(&holder, &acquiredAt, &expiresAt)
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
	if acquiredAt != nil {
		lease.AcquiredAt = parseTime(*acquiredAt)
	}
	return lease, nil
}
