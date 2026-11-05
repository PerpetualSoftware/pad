package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// maxTxTimeoutOverride caps a per-transaction override: it matches the
// 30-minute cap the sweeps already use, and stops a caller computing a huge or
// garbled duration from silently switching the bound off.
const maxTxTimeoutOverride = 30 * time.Minute

// setTxTimeouts raises (or lowers) statement_timeout and lock_timeout for the
// rest of this transaction only (SET LOCAL), for the few long paths that
// legitimately outlast the runtime pool's bounds (workspace purge, import,
// attachment remap, the effort backfill read). Call it as the FIRST statement
// after Begin. A no-op on SQLite.
//
// There is no blanket zero: a value <= 0 is refused, because PostgreSQL reads 0
// as "no timeout at all". A value above maxTxTimeoutOverride is clamped.
func (s *Store) setTxTimeouts(tx *sql.Tx, stmt, lock time.Duration) error {
	return s.setTxTimeoutsCtx(context.Background(), tx, stmt, lock)
}

// setTxIdleInTxTimeout raises idle_in_transaction_session_timeout for the rest
// of this transaction only (SET LOCAL), for a transaction that does Go work
// between statements and so can sit idle on the server for longer than the
// runtime pool's bound (the workspace import, which builds and inserts a whole
// bundle in one transaction). Call it right after Begin. A no-op on SQLite.
// A value <= 0 is refused (0 would switch the bound off); above
// maxTxTimeoutOverride it is clamped.
func (s *Store) setTxIdleInTxTimeout(tx *sql.Tx, d time.Duration) error {
	if s.dialect.Driver() != DriverPostgres {
		return nil
	}
	if d <= 0 {
		return fmt.Errorf("set tx idle_in_transaction_session_timeout: %s must be positive", d)
	}
	if d > maxTxTimeoutOverride {
		d = maxTxTimeoutOverride
	}
	if _, err := tx.Exec(fmt.Sprintf("SET LOCAL idle_in_transaction_session_timeout = %d", statementTimeoutMS(d))); err != nil {
		return fmt.Errorf("set tx idle_in_transaction_session_timeout: %w", err)
	}
	return nil
}

// setTxTimeoutsCtx is setTxTimeouts with a context for the SET statements.
func (s *Store) setTxTimeoutsCtx(ctx context.Context, tx *sql.Tx, stmt, lock time.Duration) error {
	if s.dialect.Driver() != DriverPostgres {
		return nil
	}
	if stmt <= 0 || lock <= 0 {
		return fmt.Errorf("set tx timeouts: statement %s and lock %s must both be positive", stmt, lock)
	}
	if stmt > maxTxTimeoutOverride {
		stmt = maxTxTimeoutOverride
	}
	if lock > maxTxTimeoutOverride {
		lock = maxTxTimeoutOverride
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", statementTimeoutMS(stmt))); err != nil {
		return fmt.Errorf("set tx statement_timeout: %w", err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL lock_timeout = %d", statementTimeoutMS(lock))); err != nil {
		return fmt.Errorf("set tx lock_timeout: %w", err)
	}
	return nil
}
