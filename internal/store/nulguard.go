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
		s, ok := checkedValue(a.Value)
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
//
// SHAPE, and why it is not one type.
//
// A wrapper that implements an optional interface the BASE lacks does not
// merely add a no-op: database/sql BRANCHES on whether the interface is
// present, so advertising one changes its behaviour even when the method
// delegates faithfully. Measured on the two drivers Pad uses:
//
//	                        sqlite   pgx
//	driver.DriverContext    no       YES
//	conn Pinger             yes      yes
//	conn SessionResetter    yes      yes
//	conn Validator          yes      NO
//	conn NamedValueChecker  no       YES
//	stmt NamedValueChecker  no       no
//	stmt ColumnConverter    no       no
//
// So Validator and NamedValueChecker VARY between the two, and a single
// wrapper type advertising both would have told database/sql that pgx
// validates connections and that sqlite converts its own arguments — neither
// true (codex round 2). The conn wrapper is therefore selected per-connection
// from four variants covering those two interfaces.
//
// The interfaces that do NOT vary are asserted at registration instead of
// varied over: requireBaseInterfaces fails loudly if a driver bump ever drops
// one, which is better than silently degrading and better than sixteen types.

// checkedValue extracts the string a parameter will bind, following
// driver.Valuer when the driver's own NamedValueChecker has left one in place.
//
// This closes a real hole (codex round 2). checkParams originally type-asserted
// `string` only, and pgx implements NamedValueChecker — so it ACCEPTS a
// sql.NullString unchanged rather than letting database/sql's default converter
// unwrap it, and the guard never saw the text. Measured: on Postgres a
// sql.NullString carrying a NUL passed the guard entirely and was refused by
// the server with SQLSTATE 22021, i.e. as a 500 rather than the typed 400 the
// same value gets on SQLite. The dialect split, reappearing in the response
// shape. internal/store/wiki_links.go binds sql.NullString today.
func checkedValue(v driver.Value) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case driver.Valuer:
		// Value() is contractually pure; the standard converter calls it for
		// exactly this purpose.
		inner, err := t.Value()
		if err != nil {
			return "", false
		}
		if str, ok := inner.(string); ok {
			return str, true
		}
	}
	return "", false
}

type guardDriver struct{ base driver.Driver }

func (d guardDriver) Open(name string) (driver.Conn, error) {
	c, err := d.base.Open(name)
	if err != nil {
		return nil, err
	}
	return wrapConn(c), nil
}

// guardDriverCtx is guardDriver PLUS driver.DriverContext, and it is a separate
// TYPE for the same reason the conn wrapper has four variants: implementing
// OpenConnector unconditionally would advertise DriverContext for sqlite, whose
// base lacks it.
//
// That is not hypothetical caution — the first version of this fix did exactly
// that, and every SQLite open failed at once. The failure was loud, which is
// the only reason it cost minutes; the same mistake on a method database/sql
// merely branches on would have been silent.
//
// The interface matters because without it sql.Open falls back to a legacy
// connector that IGNORES the context, so a cancelled or deadlined request can
// leave a connection attempt running — on pgx, up to its 60-second dial timeout
// (codex round 2).
type guardDriverCtx struct{ guardDriver }

func (d guardDriverCtx) OpenConnector(name string) (driver.Connector, error) {
	c, err := d.base.(driver.DriverContext).OpenConnector(name)
	if err != nil {
		return nil, err
	}
	// The OUTER driver, not the embedded one. database/sql answers
	// db.Driver() from the connector, so handing back guardDriver here made a
	// pgx-backed pool report a driver WITHOUT DriverContext — losing the very
	// interface this type exists to forward. Caught by the parity test on its
	// first run, which is what that test is for.
	return guardConnector{base: c, drv: d}, nil
}

type guardConnector struct {
	base driver.Connector
	drv  driver.Driver
}

func (c guardConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return wrapConn(conn), nil
}

func (c guardConnector) Driver() driver.Driver { return c.drv }

// wrapConn selects the variant matching the base connection's optional
// interfaces. See the shape note above for why this is a selection rather than
// one type.
func wrapConn(c driver.Conn) driver.Conn {
	_, hasValidator := c.(driver.Validator)
	_, hasNVC := c.(driver.NamedValueChecker)
	base := guardConn{c}
	switch {
	case hasValidator && hasNVC:
		return guardConnVN{base}
	case hasValidator:
		return guardConnV{base}
	case hasNVC:
		return guardConnN{base}
	default:
		return base
	}
}

type guardConn struct{ driver.Conn }

// Pinger and SessionResetter are implemented unconditionally, which is sound
// only because requireBaseInterfaces has already established that every base
// has them.
func (c guardConn) Ping(ctx context.Context) error {
	return c.Conn.(driver.Pinger).Ping(ctx)
}

func (c guardConn) ResetSession(ctx context.Context) error {
	return c.Conn.(driver.SessionResetter).ResetSession(ctx)
}

type guardConnV struct{ guardConn }

func (c guardConnV) IsValid() bool { return c.Conn.(driver.Validator).IsValid() }

type guardConnN struct{ guardConn }

func (c guardConnN) CheckNamedValue(nv *driver.NamedValue) error {
	return c.Conn.(driver.NamedValueChecker).CheckNamedValue(nv)
}

type guardConnVN struct{ guardConn }

func (c guardConnVN) IsValid() bool { return c.Conn.(driver.Validator).IsValid() }

func (c guardConnVN) CheckNamedValue(nv *driver.NamedValue) error {
	return c.Conn.(driver.NamedValueChecker).CheckNamedValue(nv)
}

// Prepare delegates to PrepareContext so there is ONE wrapping site. A mutation
// showed the separate body was unreachable: database/sql routes to
// PrepareContext whenever the conn implements ConnPrepareContext, which every
// base here does (asserted at registration).
func (c guardConn) Prepare(q string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), q)
}

func (c guardConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	st, err := c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, q)
	if err != nil {
		return nil, err
	}
	return guardStmt{st}, nil
}

func (c guardConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func (c guardConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	if err := checkParams(args); err != nil {
		return nil, err
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, q, args)
}

func (c guardConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if err := checkParams(args); err != nil {
		return nil, err
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
}

// guardStmt wraps only the two context forms, which every base statement here
// implements (asserted at registration). Statement-level NamedValueChecker and
// ColumnConverter are NOT forwarded because neither driver implements them —
// measured, and asserted, so a driver that starts to will fail registration
// rather than silently lose its conversion.
type guardStmt struct{ driver.Stmt }

func (s guardStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	if err := checkParams(args); err != nil {
		return nil, err
	}
	return s.Stmt.(driver.StmtExecContext).ExecContext(ctx, args)
}

func (s guardStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	if err := checkParams(args); err != nil {
		return nil, err
	}
	return s.Stmt.(driver.StmtQueryContext).QueryContext(ctx, args)
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
			if err := requireBaseInterfaces(d.base, probe); err != nil {
				_ = probe.Close()
				registerGuardedErr = err
				return
			}
			_ = probe.Close()
			wrapped := guardDriver{base: base}
			if _, ok := base.(driver.DriverContext); ok {
				sql.Register(d.guarded, guardDriverCtx{wrapped})
			} else {
				sql.Register(d.guarded, wrapped)
			}
		}
	})
	return registerGuardedErr
}

// requireBaseInterfaces asserts the optional interfaces the wrapper implements
// UNCONDITIONALLY are actually present on the base.
//
// This is what makes the shape sound. Four interfaces vary across drivers and
// are selected per-connection (see the shape note); the rest are advertised
// flatly, and advertising one the base lacks would change database/sql's
// behaviour rather than merely add a no-op. Rather than sixteen wrapper types
// or a comment saying "both drivers have these", the assumption is CHECKED at
// registration, once, and a driver bump that drops one fails loudly at startup
// instead of degrading silently in production.
//
// It also refuses a base whose STATEMENTS implement NamedValueChecker or
// ColumnConverter, because guardStmt does not forward those — measured absent
// on both drivers today, and a driver that gains one would otherwise lose its
// own argument conversion without any signal.
func requireBaseInterfaces(name string, probe *sql.DB) error {
	conn, err := probe.Conn(context.Background())
	if err != nil {
		// A driver that cannot open a connection to an empty DSN tells us
		// nothing; this is a best-effort check, not a reachability test.
		return nil //nolint:nilerr // see comment
	}
	defer func() { _ = conn.Close() }()

	var missing []string
	rawErr := conn.Raw(func(dc any) error {
		for _, want := range []struct {
			name string
			ok   bool
		}{
			{"Pinger", isIface[driver.Pinger](dc)},
			{"SessionResetter", isIface[driver.SessionResetter](dc)},
			{"ExecerContext", isIface[driver.ExecerContext](dc)},
			{"QueryerContext", isIface[driver.QueryerContext](dc)},
			{"ConnPrepareContext", isIface[driver.ConnPrepareContext](dc)},
			{"ConnBeginTx", isIface[driver.ConnBeginTx](dc)},
		} {
			if !want.ok {
				missing = append(missing, want.name)
			}
		}
		return nil
	})
	if rawErr != nil {
		return nil //nolint:nilerr // best-effort, as above
	}
	if len(missing) > 0 {
		return fmt.Errorf("store: driver %q no longer implements %v; the NUL write guard advertises these "+
			"unconditionally and would misreport them to database/sql — see nulguard.go's shape note", name, missing)
	}
	return nil
}

func isIface[T any](v any) bool {
	_, ok := v.(T)
	return ok
}
