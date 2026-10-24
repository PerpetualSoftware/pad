package store

import (
	"database/sql"
	"fmt"
)

// TASK-4051: test-only fault seam for a committed UPDATE's row count.
var rowsAffected = func(r sql.Result) (int64, error) { return r.RowsAffected() }

// The dispatcher's settle primitives beyond push_dispatch.go's claim-guarded
// ones (TASK-3939 part B).

// RecordSentError is RecordNotificationsSent's error. The record runs one
// UPDATE per chunk with no transaction, so a failure can follow chunks that
// already committed: Landed is how many leading ids those chunks covered,
// which a caller must treat as recorded (TASK-4032 codex r3 M1).
type RecordSentError struct {
	Landed int
	err    error
}

func (e *RecordSentError) Error() string { return e.err.Error() }
func (e *RecordSentError) Unwrap() error { return e.err }

// RecordNotificationsSent records ids as pushed, BY ID AND NOT BY CLAIM: this
// is the one deliberate exception to "settle only under the claim" (r4-L4).
// A delivery is a fact. A claimer whose Send outlived its lease has still
// delivered, so its late 'sent' must land on a row another instance has since
// reclaimed (that instance's own settle is then a no-op) and on a row
// ExpireStaleNotifications took after the lease lapsed; a token guard would
// lose the delivered row or record it twice. Only pending and expired rows
// move: a skipped row stays skipped. Skip and release stay token-guarded.
// An error is always a *RecordSentError.
func (s *Store) RecordNotificationsSent(ids []string) (int64, error) {
	var total int64
	for start := 0; start < len(ids); start += notificationIDChunk {
		end := min(start+notificationIDChunk, len(ids))
		ph, args := idPlaceholders(ids[start:end])
		res, err := s.db.Exec(s.q(`UPDATE user_notifications
			SET push_state = 'sent', sent_at = ?, claim_token = '', claimed_at = NULL
			WHERE push_state IN ('pending', 'expired') AND id IN (`+ph+`)`), append([]any{now()}, args...)...)
		if err != nil {
			return total, &RecordSentError{Landed: start, err: fmt.Errorf("record notifications sent: %w", err)}
		}
		n, err := rowsAffected(res)
		if err != nil {
			// The UPDATE committed; only its count is lost.
			return total, &RecordSentError{Landed: end, err: fmt.Errorf("record notifications sent: %w", err)}
		}
		total += n
	}
	return total, nil
}

// UnclaimNotifications hands claimed rows back WITHOUT counting an attempt:
// for rows that were never tried (a shutdown, a deadline, a gate that could
// not be evaluated). token is Claim.Token; same guard as the other settles.
func (s *Store) UnclaimNotifications(token string, ids []string) (int64, error) {
	return s.settleNotifications(token, ids, `claim_token = '', claimed_at = NULL`)
}

// HeldNotifications returns the subset of ids still pending under token: the
// dispatcher's check just before a Send that a row was not reclaimed,
// settled or deleted while it worked.
func (s *Store) HeldNotifications(token string, ids []string) ([]string, error) {
	if token == "" {
		return nil, errEmptyClaimToken
	}
	var held []string
	for start := 0; start < len(ids); start += notificationIDChunk {
		end := min(start+notificationIDChunk, len(ids))
		ph, args := idPlaceholders(ids[start:end])
		rows, err := s.db.Query(s.q(`SELECT id FROM user_notifications
			WHERE claim_token = ? AND push_state = 'pending' AND id IN (`+ph+`)`), append([]any{token}, args...)...)
		if err != nil {
			return nil, fmt.Errorf("held notifications: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, fmt.Errorf("held notifications: %w", err)
			}
			held = append(held, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("held notifications: %w", err)
		}
	}
	return held, nil
}
