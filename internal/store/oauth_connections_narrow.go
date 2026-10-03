package store

import (
	"fmt"
)

// NarrowCurrentOnlyResult reports what NarrowCurrentOnlyWildcardConnections
// changed.
type NarrowCurrentOnlyResult struct {
	// Connections is how many connections were narrowed.
	Connections int
	// WorkspacesAdded is how many allow-list rows the narrowing wrote.
	WorkspacesAdded int
	// Emptied counts narrowed connections whose user was a member of no
	// live workspace, so their list is empty and they now reach nothing.
	Emptied int
}

// NarrowCurrentOnlyWildcardConnections gives every connection stored with
// all_current_workspaces on and include_future_workspaces off what that
// combination promised (BUG-3338).
//
// The Connected Apps checkbox let a user turn "future" off under the
// wildcard, but the wildcard is live and the flag was never read, so those
// connections went on reaching workspaces joined later. Each one becomes a
// specific list of the live workspaces the wildcard reaches for its user
// NOW (membership or a guest grant: snapshotCurrentWorkspacesSQL, the same
// set the limit action takes), with added_by='user', keeping any rows
// already staged,
// and the wildcard off: the same result as the user pressing "Limit to my
// current workspaces".
//
// Narrowing-only: it writes allow-list rows only for workspaces the
// wildcard already reached, and only ever clears the wildcard. A user with
// no memberships ends up with an empty list, which the gate reads as
// reaching nothing; that is still narrower than a live wildcard.
//
// Idempotent: a narrowed connection no longer matches, and since BUG-3338
// no door stores the split combination, so a re-run finds nothing. It runs
// at startup next to BackfillOAuthConnections, before the HTTP server
// accepts requests, in one transaction.
func (s *Store) NarrowCurrentOnlyWildcardConnections() (NarrowCurrentOnlyResult, error) {
	var res NarrowCurrentOnlyResult
	tx, err := s.db.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	selectSQL := `
        SELECT request_id, user_id FROM oauth_connections
         WHERE all_current_workspaces = ? AND include_future_workspaces = ?`
	if s.dialect.Driver() == DriverPostgres {
		selectSQL += ` FOR UPDATE`
	}
	rows, err := tx.Query(s.q(selectSQL), s.dialect.BoolToInt(true), s.dialect.BoolToInt(false))
	if err != nil {
		return res, fmt.Errorf("narrow connections: select: %w", err)
	}
	type conn struct{ requestID, userID string }
	var conns []conn
	for rows.Next() {
		var c conn
		if err := rows.Scan(&c.requestID, &c.userID); err != nil {
			rows.Close()
			return res, fmt.Errorf("narrow connections: scan: %w", err)
		}
		conns = append(conns, c)
	}
	// Next can stop on an iteration error and close the rows itself, after
	// which Close reports nothing: read Err, or a partial scan would commit
	// as if complete.
	if err := rows.Err(); err != nil {
		rows.Close()
		return res, fmt.Errorf("narrow connections: rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return res, fmt.Errorf("narrow connections: rows: %w", err)
	}
	if len(conns) == 0 {
		return res, nil
	}

	for _, c := range conns {
		r, err := tx.Exec(s.q(s.snapshotCurrentWorkspacesSQL()), c.requestID, AddedByUser, c.userID, c.userID, c.userID)
		if err != nil {
			return res, fmt.Errorf("narrow connections: insert %s: %w", c.requestID, err)
		}
		added, err := r.RowsAffected()
		if err != nil {
			return res, fmt.Errorf("narrow connections: rows affected %s: %w", c.requestID, err)
		}
		r, err = tx.Exec(s.q(`
            UPDATE oauth_connections
               SET all_current_workspaces = ?,
                   updated_at = `+s.dialect.NowRFC3339()+`
             WHERE request_id = ? AND all_current_workspaces = ? AND include_future_workspaces = ?`),
			s.dialect.BoolToInt(false), c.requestID, s.dialect.BoolToInt(true), s.dialect.BoolToInt(false))
		if err != nil {
			return res, fmt.Errorf("narrow connections: update %s: %w", c.requestID, err)
		}
		if n, err := r.RowsAffected(); err != nil {
			return res, fmt.Errorf("narrow connections: rows affected %s: %w", c.requestID, err)
		} else if n != 1 {
			return res, fmt.Errorf("narrow connections: %s changed under the transaction", c.requestID)
		}
		var total int
		if err := tx.QueryRow(s.q(`SELECT COUNT(*) FROM oauth_connection_workspaces WHERE request_id = ?`), c.requestID).Scan(&total); err != nil {
			return res, fmt.Errorf("narrow connections: count %s: %w", c.requestID, err)
		}
		res.Connections++
		res.WorkspacesAdded += int(added)
		if total == 0 {
			res.Emptied++
		}
	}
	if err := tx.Commit(); err != nil {
		return NarrowCurrentOnlyResult{}, err
	}
	return res, nil
}
