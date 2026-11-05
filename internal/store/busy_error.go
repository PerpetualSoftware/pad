package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"net"

	"github.com/jackc/pgx/v5/pgconn"
)

// Two classes of "the database was busy or interrupted the work" (TASK-4278),
// kept apart because they promise different things to the caller.
//
// IsDBBusy: the SERVER said so, with a SQLSTATE that means the statement or
// transaction was rolled back:
//
//	57014 query_canceled (statement_timeout), 55P03 lock_not_available
//	(lock_timeout), 25P03 idle_in_transaction_session_timeout, 57P01
//	admin_shutdown (session terminated), 40P01 deadlock_detected, 40001
//	serialization_failure.
//
// A failed write of this class changed nothing, and retrying it is safe.
// (An error that a store function has itself proved happened BEFORE any COMMIT
// was sent, see markRolledBack, joins this class.)
//
// IsDBOutcomeUnknown: the CLIENT side gave up or lost the connection (a
// context deadline, a dead or closed pooled connection, a network failure or
// a driver-level timeout). The server may or may not have committed: the
// signal can arrive while COMMIT is in flight, or after it succeeded, so a
// write cannot claim it saved nothing. Retrying is the caller's call after
// checking.
//
// context.Canceled is in neither class: it means the client went away, and
// there is nobody to tell.

// IsDBBusy reports a server-signalled, definitive-rollback outcome (see above).
func IsDBBusy(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "57014", "55P03", "25P03", "57P01", "40P01", "40001":
			return true
		}
		return false
	}
	var rb *rolledBackError
	return errors.As(err, &rb)
}

// IsDBOutcomeUnknown reports a client-side interruption whose effect on the
// server is unknown (see above). It is false for anything the server answered
// (any *pgconn.PgError, which is a definitive answer either way) and for an
// error a store function marked as proven rolled back.
//
// Only typed checks are used, never message text:
//   - context.DeadlineExceeded: the caller's clock ran out, possibly mid-COMMIT;
//   - driver.ErrBadConn: database/sql's "this connection is dead";
//   - net.ErrClosed and *net.OpError: the socket closed or failed mid-exchange;
//   - pgconn.ErrConnClosed: pgx's typed "conn closed" (it used to be matched by
//     substring);
//   - pgconn.Timeout: pgx's own deadline/timeout wrapper around the above.
//
// Why *net.OpError and pgconn.Timeout are safe to read as "the database
// connection failed": pgx returns a mid-query socket failure as the bare
// *net.OpError (normalizeTimeoutError passes a non-timeout error through), so
// no pgx-specific type can tell it from any other network error. What makes
// the match sound is that the store performs NO other network I/O: it holds
// the database handle only (no blob store, HTTP client or dialer), which
// TestStoreHasNoNonDatabaseNetworkIO pins by import. The one residue is
// outside the store: server.writeInternalError is a generic funnel, so a
// handler that passes it a network error from a blob backend that dials out
// (none in this repo; the filesystem backend raises none) would read it as a
// database interruption.
func IsDBOutcomeUnknown(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return false
	}
	var rb *rolledBackError
	if errors.As(err, &rb) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, driver.ErrBadConn) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, pgconn.ErrConnClosed) ||
		pgconn.Timeout(err) {
		return true
	}
	var opErr *net.OpError
	return errors.As(err, &opErr)
}

// IsDBRetryable is either class: the request failed for a reason a later
// attempt can get past. Callers that must word the two differently use the two
// predicates.
func IsDBRetryable(err error) bool { return IsDBBusy(err) || IsDBOutcomeUnknown(err) }

// rolledBackError marks an error a store function has PROVEN happened before
// any COMMIT was sent, so the transaction rolled back whatever the error's
// own class says (a deadline that expired between statements).
type rolledBackError struct{ err error }

func (e *rolledBackError) Error() string { return e.err.Error() }
func (e *rolledBackError) Unwrap() error { return e.err }

// markRolledBack marks err (nil stays nil) as a definitive rollback.
func markRolledBack(err error) error {
	if err == nil {
		return nil
	}
	return &rolledBackError{err: err}
}

// commitStepError marks the error of the COMMIT call itself, the one step
// whose outcome an interruption leaves unknown.
type commitStepError struct{ err error }

func (e *commitStepError) Error() string { return e.err.Error() }
func (e *commitStepError) Unwrap() error { return e.err }
