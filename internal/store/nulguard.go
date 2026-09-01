package store

// The NUL invariant's Go-side enforcement (DOC-2823 Layer A, closing BUG-2814
// and the current-binary half of BUG-2813).
//
// WHY THIS IS A DRIVER WRAPPER AND NOT A SEAM IN THIS PACKAGE.
//
// The design's first shape was a validating wrapper the store's write calls
// would route through. Two measurements retired it:
//
//   - There is no existing write seam to wrap. `Queryer` (store.go) is
//     Query + QueryRow — its own doc says "the read-only subset" — and no
//     store write passes through it.
//   - Writes reach the driver by FOUR receivers: db.Exec (139 sites),
//     tx.Exec (99), stmt.Exec (2, prepared in agent_roles.go), and a passed
//     executor. A wrapper at the *sql.DB / *sql.Tx level cannot see the
//     prepared ones at all, and sees nothing of SQL built by Sprintf.
//
// The deeper reason is the one this whole cluster teaches. A seam every write
// site must be EDITED to route through is an enumeration wearing a seam's
// costume: nothing stops the next site taking the raw handle, and the compiler
// cannot tell you it did. TASK-2825 already watched two instruments each miss
// write sites the other caught, and dynamically-composed SQL is invisible to
// any source-level instrument by construction.
//
// A driver wrapper has no such property to maintain. It never looks at SQL
// text — only at bound parameters — so Sprintf-built statements are covered
// without being understood, and a new call site is covered before it is
// written. Completeness stops being anyone's job.
//
// WHAT IT COVERS. Both ExecContext and QueryContext, on the conn AND on
// prepared statements. The Query path is not defensive symmetry: three writes
// ride it today — UPDATE ... RETURNING in password_resets.go and
// email_verification.go, and INSERT ... RETURNING in yjs_updates.go, which
// binds a []byte payload on the Postgres path.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"

	"github.com/PerpetualSoftware/pad/internal/textguard"
)

// ErrInvalidTextParameter is the sentinel behind InvalidTextParameterError.
var ErrInvalidTextParameter = errors.New("store: invalid text parameter")

// InvalidTextParameterError is the refusal a caller receives when a bound
// parameter violates the NUL invariant.
//
// It carries no parameter VALUE, deliberately: the value is user content and
// this error travels into logs and, through the handlers, into responses. The
// ordinal is enough to locate it while debugging, and the reason names the
// rule rather than quoting the offender.
type InvalidTextParameterError struct {
	// Ordinal is the 1-based position of the offending parameter.
	Ordinal int
	// Reason is the rule that refused it, phrased for a caller.
	Reason string
}

func (e *InvalidTextParameterError) Error() string {
	return fmt.Sprintf("%s: parameter %d: %s", ErrInvalidTextParameter.Error(), e.Ordinal, e.Reason)
}

func (e *InvalidTextParameterError) Unwrap() error { return ErrInvalidTextParameter }

// checkParams applies the shared predicate to every string/[]byte argument.
//
// CLASSING, and why it is what it is. The wrapper sees positional parameters,
// not columns, so it cannot consult the 86-column classification directly. It
// classes a parameter as JSON when the VALUE ITSELF is a complete JSON
// document — which is sound for this invariant in a way that a column-derived
// classing would not be cheaper than:
//
//   - Every value bound to a JSON-classed column IS a JSON document; the store
//     marshals it before binding. So no JSON column's value escapes the
//     document check.
//
//   - A user-TEXT column whose value happens to parse as JSON gets the
//     document check too. This is a REAL over-refusal, and the first version of
//     this comment justified it wrongly: it claimed such a value "is, on any
//     column, a value Postgres would refuse the moment anything parsed it".
//     That is false. Nothing parses a TEXT column, and Postgres stores the
//     escape there as six ordinary characters quite happily (codex round 1,
//     finding 3).
//
//     So the honest statement of the trade: a user pasting a JSON document
//     that contains a live NUL escape into an item's CONTENT is refused,
//     though it would store fine. That is not hypothetical in a documentation
//     tool — writing about this very bug produces such a value.
//
//     It is accepted for now because the alternative is worse in the direction
//     that matters. Deriving JSON-ness from the CALL SITE's knowledge puts the
//     classification back at the sites this design exists to stop depending on,
//     and the failure mode of over-refusal is a loud 400 naming the rule, while
//     the failure mode of under-refusal is a NUL at rest that only the next
//     dialect migration discovers. Flagged to the lead as a measured trade
//     rather than buried here; if it is ever paid down, the mechanism is S2's
//     column-attached triggers, which DO have the classing this layer lacks.
//
// The alternative — deriving JSON-ness from the call site's knowledge — puts
// the classification back at the call sites this design exists to stop
// depending on.
func checkParams(args []driver.NamedValue) error {
	for _, a := range args {
		// STRING ONLY, and []byte is deliberately exempt.
		//
		// The invariant is about TEXT and JSON columns. In this store, Go's
		// `string` is how text and JSON are bound and `[]byte` is how BINARY
		// is bound — and binary legitimately contains NUL bytes, so checking
		// []byte refuses valid writes. The existing collab suite caught this
		// immediately: item_yjs_updates.update_data is raw Yjs binary, and the
		// first version of this guard refused every op-log append.
		//
		// The rule is measured, not assumed:
		//
		//   - item_yjs_updates.update_data (BLOB on SQLite, BYTEA on Postgres)
		//     is the ONLY binary column in either schema.
		//   - No json.Marshal result is bound without a string() conversion,
		//     so no JSON column receives a []byte parameter.
		//
		// The second of those was established by a source-level sweep, and
		// this unit's own history is a catalogue of source-level sweeps missing
		// things. So the claim is pinned BEHAVIOURALLY rather than trusted:
		// TestBinaryColumnCensus fails the moment a new BLOB/BYTEA column
		// enters either schema, which is when someone must re-examine whether
		// []byte is still binary-only here.
		s, ok := a.Value.(string)
		if !ok {
			continue
		}
		if textguard.ContainsNUL(s) {
			return &InvalidTextParameterError{
				Ordinal: a.Ordinal,
				Reason:  "value contains a NUL byte, which no text or JSON column can store",
			}
		}
		if textguard.DocumentDecodesNULAnyShape(s) {
			return &InvalidTextParameterError{
				Ordinal: a.Ordinal,
				Reason:  "value is a JSON document containing an escape that decodes to a NUL",
			}
		}
	}
	return nil
}

// ---- the wrapper ----

type guardDriver struct{ base driver.Driver }

func (d guardDriver) Open(name string) (driver.Conn, error) {
	c, err := d.base.Open(name)
	if err != nil {
		return nil, err
	}
	return guardConn{c}, nil
}

// guardConn forwards the OPTIONAL connection interfaces as well as the
// required ones.
//
// Embedding driver.Conn alone silently HID them, which is a production
// regression rather than a cosmetic gap: measured, the raw modernc conn
// implements Pinger, SessionResetter and Validator, and the wrapped one
// implemented none. database/sql degrades quietly for each — Ping succeeds
// without pinging anything, pooled connections stop being reset between uses,
// and a connection the driver knows is dead stays in the pool.
//
// Each method below delegates when the base supports it and otherwise does
// exactly what database/sql does for a conn that lacks the interface, so
// wrapping a driver without one is equivalent to not wrapping it. Always
// implementing them is safe for that reason.
type guardConn struct{ driver.Conn }

func (c guardConn) Ping(ctx context.Context) error {
	if p, ok := c.Conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	// database/sql treats a non-Pinger connection as reachable by virtue of
	// existing; matching that keeps the wrapper transparent.
	return nil
}

func (c guardConn) ResetSession(ctx context.Context) error {
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c guardConn) IsValid() bool {
	if v, ok := c.Conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

// CheckNamedValue forwards the driver's own argument conversion when it has
// one. Without this the wrapper would fall back to database/sql's default
// converter and quietly narrow the argument types a driver accepts — pgx in
// particular converts a great deal more than the default does.
func (c guardConn) CheckNamedValue(nv *driver.NamedValue) error {
	if ck, ok := c.Conn.(driver.NamedValueChecker); ok {
		return ck.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

// Prepare exists because driver.Conn requires it, and it DELEGATES rather than
// wrapping separately.
//
// A mutation is why: giving it its own wrapping body left a second place to get
// the wrapping right, and dropping the wrap there survived the whole suite —
// because database/sql routes to PrepareContext whenever the conn implements
// driver.ConnPrepareContext, which both modernc and pgx do. So the branch was
// unreachable for every driver Pad uses, and a test for it would have been a
// test of nothing. One implementation is better than an untestable second.
func (c guardConn) Prepare(q string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), q)
}

func (c guardConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	if p, ok := c.Conn.(driver.ConnPrepareContext); ok {
		s, err := p.PrepareContext(ctx, q)
		if err != nil {
			return nil, err
		}
		return guardStmt{s}, nil
	}
	s, err := c.Conn.Prepare(q)
	if err != nil {
		return nil, err
	}
	return guardStmt{s}, nil
}

func (c guardConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if b, ok := c.Conn.(driver.ConnBeginTx); ok {
		return b.BeginTx(ctx, opts)
	}
	// A base without ConnBeginTx cannot honour isolation or read-only, and
	// SILENTLY starting a plain transaction would give the caller weaker
	// guarantees than it asked for. database/sql itself refuses in this
	// situation; matching that keeps the wrapper transparent rather than
	// permissive.
	if opts.Isolation != driver.IsolationLevel(sql.LevelDefault) || opts.ReadOnly {
		return nil, errors.New("store: driver does not support transaction options")
	}
	return c.Conn.Begin() //nolint:staticcheck // fallback for a driver without ConnBeginTx
}

// ExecContext and QueryContext return driver.ErrSkip when the base conn does
// not implement the optional interface, which makes database/sql fall back to
// Prepare + the statement path — where guardStmt applies the same check. The
// value never reaches the database unvalidated on either route.
func (c guardConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	if err := checkParams(args); err != nil {
		return nil, err
	}
	if e, ok := c.Conn.(driver.ExecerContext); ok {
		return e.ExecContext(ctx, q, args)
	}
	return nil, driver.ErrSkip
}

func (c guardConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if err := checkParams(args); err != nil {
		return nil, err
	}
	if qc, ok := c.Conn.(driver.QueryerContext); ok {
		return qc.QueryContext(ctx, q, args)
	}
	return nil, driver.ErrSkip
}

type guardStmt struct{ driver.Stmt }

// The statement methods FALL BACK to the positional form rather than returning
// driver.ErrSkip.
//
// The distinction matters and the first version had it wrong. At the CONN
// level, ErrSkip is the documented signal and database/sql falls back to
// prepare-and-execute. At the STATEMENT level it does not: because this
// wrapper always implements StmtExecContext, database/sql calls it and
// propagates whatever comes back, so an ErrSkip would surface as a query
// FAILURE against any base statement lacking the context form rather than
// degrading to the older interface.
func (s guardStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	if err := checkParams(args); err != nil {
		return nil, err
	}
	if e, ok := s.Stmt.(driver.StmtExecContext); ok {
		return e.ExecContext(ctx, args)
	}
	vals, err := positionalArgs(args)
	if err != nil {
		return nil, err
	}
	return s.Stmt.Exec(vals) //nolint:staticcheck // the documented fallback for a pre-context driver
}

func (s guardStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	if err := checkParams(args); err != nil {
		return nil, err
	}
	if q, ok := s.Stmt.(driver.StmtQueryContext); ok {
		return q.QueryContext(ctx, args)
	}
	vals, err := positionalArgs(args)
	if err != nil {
		return nil, err
	}
	return s.Stmt.Query(vals) //nolint:staticcheck // the documented fallback for a pre-context driver
}

// positionalArgs converts named values back to the positional form the
// pre-context statement interfaces take, refusing a NAMED argument rather than
// dropping its name — silently binding a named parameter by position would
// bind the wrong value.
func positionalArgs(args []driver.NamedValue) ([]driver.Value, error) {
	vals := make([]driver.Value, len(args))
	for _, a := range args {
		if a.Name != "" {
			return nil, errors.New("store: driver does not support named parameters")
		}
		if a.Ordinal < 1 || a.Ordinal > len(vals) {
			return nil, errors.New("store: parameter ordinal out of range")
		}
		vals[a.Ordinal-1] = a.Value
	}
	return vals, nil
}

// ---- registration ----

const (
	guardedSQLiteDriver   = "pad-sqlite-nulguard"
	guardedPostgresDriver = "pad-pgx-nulguard"
)

var registerGuarded sync.Once
var registerGuardedErr error

// registerGuardedDrivers registers the wrapping driver names once per process.
//
// The base driver is taken from a throwaway sql.DB rather than from a driver
// value the imported packages export, because neither modernc.org/sqlite nor
// pgx/stdlib exports one under a stable name. sql.Open does not dial, so the
// throwaway costs nothing.
func registerGuardedDrivers() error {
	registerGuarded.Do(func() {
		for _, d := range []struct{ base, guarded string }{
			{"sqlite", guardedSQLiteDriver},
			{"pgx", guardedPostgresDriver},
		} {
			probe, err := sql.Open(d.base, "")
			if err != nil {
				registerGuardedErr = fmt.Errorf("resolve %s driver: %w", d.base, err)
				return
			}
			base := probe.Driver()
			_ = probe.Close()
			sql.Register(d.guarded, guardDriver{base: base})
		}
	})
	return registerGuardedErr
}
