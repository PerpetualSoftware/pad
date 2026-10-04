package store

import (
	"fmt"
	"time"
)

// The OAuth expiry sweep (BUG-3301). Nothing else deletes an OAuth row by
// age: rotation flips a chain's previous access and refresh rows to
// inactive and inserts a fresh pair, so every refresh leaves two rows
// behind for good (measured on BUG-3301's trail: +1 access and +1 refresh
// row per refresh, 48 rows a day for one client at the default 1 h access
// lifetime). The caller (the server's token reaper) decides the cutoffs
// from the OAuth server's lifespans; this file only deletes below them.

// OAuthSweepCutoffs is, per table, the requested_at before which a row is
// deleted. A zero time skips that table.
type OAuthSweepCutoffs struct {
	AccessTokens       time.Time
	RefreshTokens      time.Time
	AuthorizationCodes time.Time
	PKCERequests       time.Time
}

// OAuthSweepResult is how many rows each table lost. Capped reports that a
// table used its whole batch cap, so rows MAY remain for the next call; it
// is not a promise that they do (the last full batch may have been the
// end).
type OAuthSweepResult struct {
	AccessTokens       int64
	RefreshTokens      int64
	AuthorizationCodes int64
	PKCERequests       int64
	// AppTokenBindings is install-token bindings whose grant has no row left
	// in any of the four tables (TASK-3399): the binding table has no
	// foreign key to them, so without this an expired chain's binding
	// stayed forever, for service and delegated grants alike.
	AppTokenBindings int64
	Capped           bool
}

// Total is the rows deleted across all four tables.
func (r OAuthSweepResult) Total() int64 {
	return r.AccessTokens + r.RefreshTokens + r.AuthorizationCodes + r.PKCERequests + r.AppTokenBindings
}

// SweepExpiredOAuthRows deletes rows whose requested_at is before the
// table's cutoff, batchSize rows per statement and at most maxBatches
// statements per table per call.
//
// Each batch is its own autocommit DELETE naming at most batchSize rows
// by primary key, so it holds SQLite's writer lock (or Postgres's row
// locks) for one short statement, and a concurrent /oauth/token write
// runs between batches rather than waiting on one long delete. pause
// separates the batches. The cap bounds one call; rows left over go on
// the next tick.
//
// requested_at is TEXT written as UTC RFC3339 at second precision on
// both dialects (insertOAuthRequestRow), so the string comparison is a
// time comparison, and three of the four tables index it. PKCE has only
// a request_id index; its rows exist only for authorizations whose code
// was never exchanged (a successful exchange deletes it), so the scan is
// over a small table.
func (s *Store) SweepExpiredOAuthRows(cut OAuthSweepCutoffs, batchSize, maxBatches int, pause time.Duration) (OAuthSweepResult, error) {
	var res OAuthSweepResult
	if batchSize <= 0 || maxBatches <= 0 {
		return res, fmt.Errorf("sweep oauth: batch size and cap must be positive")
	}
	for _, t := range []struct {
		table  string
		cutoff time.Time
		n      *int64
	}{
		{"oauth_access_tokens", cut.AccessTokens, &res.AccessTokens},
		{"oauth_refresh_tokens", cut.RefreshTokens, &res.RefreshTokens},
		{"oauth_authorization_codes", cut.AuthorizationCodes, &res.AuthorizationCodes},
		{"oauth_pkce_requests", cut.PKCERequests, &res.PKCERequests},
	} {
		if t.cutoff.IsZero() {
			continue
		}
		before := t.cutoff.UTC().Format(time.RFC3339)
		// The table name is one of four literals above, never input.
		stmt := s.q(`DELETE FROM ` + t.table + ` WHERE signature IN (
			SELECT signature FROM ` + t.table + ` WHERE requested_at < ? LIMIT ?
		)`)
		batches := 0
		for {
			r, err := s.db.Exec(stmt, before, batchSize)
			if err != nil {
				return res, fmt.Errorf("sweep %s: %w", t.table, err)
			}
			n, err := r.RowsAffected()
			if err != nil {
				return res, fmt.Errorf("sweep %s: rows affected: %w", t.table, err)
			}
			*t.n += n
			batches++
			if n < int64(batchSize) {
				break
			}
			if batches >= maxBatches {
				res.Capped = true
				break
			}
			// Leave the writer lock free for a moment, so a write that
			// is backing off in SQLite's busy handler gets its turn
			// rather than losing every race to the next batch.
			time.Sleep(pause)
		}
	}
	// Bindings orphaned by the deletes above, or by any earlier one. A
	// binding is written in the same transaction as its grant's first row,
	// so a committed binding without a row is never one mid-issuance.
	orphans := s.q(`DELETE FROM app_token_bindings WHERE request_id IN (
		SELECT b.request_id FROM app_token_bindings b
		WHERE NOT EXISTS (SELECT 1 FROM oauth_access_tokens t WHERE t.request_id = b.request_id)
		  AND NOT EXISTS (SELECT 1 FROM oauth_refresh_tokens t WHERE t.request_id = b.request_id)
		  AND NOT EXISTS (SELECT 1 FROM oauth_authorization_codes t WHERE t.request_id = b.request_id)
		  AND NOT EXISTS (SELECT 1 FROM oauth_pkce_requests t WHERE t.request_id = b.request_id)
		LIMIT ?
	)`)
	for batches := 0; ; batches++ {
		if batches >= maxBatches {
			res.Capped = true
			break
		}
		r, err := s.db.Exec(orphans, batchSize)
		if err != nil {
			return res, fmt.Errorf("sweep app_token_bindings: %w", err)
		}
		n, err := r.RowsAffected()
		if err != nil {
			return res, fmt.Errorf("sweep app_token_bindings: rows affected: %w", err)
		}
		res.AppTokenBindings += n
		if n < int64(batchSize) {
			break
		}
		time.Sleep(pause)
	}
	return res, nil
}
