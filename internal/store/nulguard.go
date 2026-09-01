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
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

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
	// Ordinal is the 1-based position of the offending parameter, or 0 when
	// the refusal came from the DATABASE (a Layer B trigger), which identifies
	// the column instead.
	Ordinal int
	// Reason is the rule that refused it, phrased for a caller.
	Reason string
}

func (e *InvalidTextParameterError) Error() string {
	if e.Ordinal < 1 {
		// A refusal from the DATABASE knows the column, not the parameter
		// position. Printing "parameter 0" for it would be a claim about
		// something nobody measured.
		return fmt.Sprintf("%s: %s", ErrInvalidTextParameter.Error(), e.Reason)
	}
	return fmt.Sprintf("%s: parameter %d: %s", ErrInvalidTextParameter.Error(), e.Ordinal, e.Reason)
}

func (e *InvalidTextParameterError) Unwrap() error { return ErrInvalidTextParameter }

// CLASSING NOTE for normalizeAndCheck below.
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
// BINARY is exempt, and that is measured rather than assumed. The invariant is
// about text and JSON columns; in this store []byte binds BINARY, and
// item_yjs_updates.update_data — the only BLOB/BYTEA column in either schema —
// legitimately contains NUL bytes. The first version of this guard checked
// []byte and refused every Yjs op-log append; the existing collab suite caught
// it immediately. TestBinaryColumnCensus fails when a new binary column
// appears, which is when that exemption must be re-examined.
// normalizeAndCheck resolves each parameter to the value that will actually be
// bound, checks it, and WRITES THE RESOLVED VALUE BACK so the driver binds the
// same thing that was inspected.
//
// Three defects in the first version, all found by codex round 3, and all of
// them the same underlying error — inspecting one value and forwarding another:
//
//   - Value() was called for the check and the ORIGINAL Valuer was forwarded,
//     so pgx called Value() a second time. A stateful valuer could return clean
//     text to the guard and NUL-bearing text to the database.
//   - A typed-nil valuer, (*sql.NullString)(nil), was called directly and
//     panicked. database/sql special-cases that as SQL NULL.
//   - Only an exact `string` was recognised. pgx implements NamedValueChecker
//     and accepts *string, named string types and json.RawMessage unconverted,
//     so each of those carried text the guard never saw.
//
// Resolution is therefore by REFLECTION on the kind rather than by a list of
// types: anything whose kind is String is text, and anything that is a byte
// slice is binary and exempt (see the classing note in checkParams).
func normalizeAndCheck(args []driver.NamedValue) error {
	for i := range args {
		resolved, err := resolveValue(args[i].Value)
		if err != nil {
			return err
		}
		args[i].Value = resolved

		s, isText, cerr := classify(resolved)
		if cerr != nil {
			return cerr
		}
		if !isText {
			continue
		}
		if textguard.ContainsNUL(s) {
			return &InvalidTextParameterError{
				Ordinal: args[i].Ordinal,
				Reason:  "value contains a NUL byte, which no text or JSON column can store",
			}
		}
		if textguard.DocumentDecodesNULAnyShape(s) {
			return &InvalidTextParameterError{
				Ordinal: args[i].Ordinal,
				Reason:  "value is a JSON document containing an escape that decodes to a NUL",
			}
		}
	}
	return nil
}

// maxValuerDepth bounds the unwrap loop. database/sql's own converter resolves
// once, but a driver with its own NamedValueChecker may recurse — so the guard
// resolves to a FIXED POINT rather than once, or an outer valuer returning an
// inner one would hand the guard a clean value and the driver a NUL-bearing
// one (codex round 4). The bound exists because a cyclic valuer would hang;
// four is far past anything real.
const maxValuerDepth = 4

var valuerType = reflect.TypeOf((*driver.Valuer)(nil)).Elem()

// resolveValue unwraps driver.Valuer to a fixed point, mirroring database/sql's
// converter at each step — including its typed-nil rule.
//
// That rule is COPIED, not approximated: database/sql treats a nil pointer as
// SQL NULL only when the POINTER'S ELEMENT TYPE implements Valuer, and calls a
// pointer-receiver-only valuer even when nil. An earlier version here nil'd
// every nil pointer valuer, which is broader than the library (codex round 4).
func resolveValue(v driver.Value) (driver.Value, error) {
	for depth := 0; depth < maxValuerDepth; depth++ {
		valuer, ok := v.(driver.Valuer)
		if !ok {
			return v, nil
		}
		rv := reflect.ValueOf(valuer)
		if rv.Kind() == reflect.Ptr && rv.IsNil() && rv.Type().Elem().Implements(valuerType) {
			return nil, nil
		}
		out, err := valuer.Value()
		if err != nil {
			return nil, err
		}
		v = out
	}
	return nil, errors.New("store: parameter valuer did not resolve within the depth bound")
}

// classify sorts a resolved parameter into text, exempt, or REFUSED.
//
// An ALLOW-LIST, and that is the point (codex round 4). Two rounds were spent
// widening a text detector shape by shape — exact string, then *string and
// named string types, then json.RawMessage — and the next round named five
// more (json.Marshaler, fmt.Stringer, pgx's TextValuer, defined []byte aliases,
// JSON-marshallable structs). Enumerating what CAN carry text is a losing game
// against a driver as permissive as pgx.
//
// So this enumerates what the store actually BINDS and refuses anything else.
// The vocabulary is small and stable: strings, binary blobs, numbers, booleans,
// times, NULL. A parameter outside it is a programming error, and refusing it
// loudly beats binding it unchecked — which is what every earlier version did
// by default.
func classify(v driver.Value) (text string, isText bool, err error) {
	if v == nil {
		return "", false, nil
	}
	if raw, ok := v.(json.RawMessage); ok {
		return string(raw), true, nil
	}

	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return "", false, nil
		}
		rv = rv.Elem()
	}

	switch rv.Kind() {
	case reflect.String:
		return rv.String(), true, nil

	case reflect.Slice:
		// Byte slices are BINARY and exempt — see the classing note above.
		// json.RawMessage is handled by name before reaching here, because a
		// byte slice that means JSON is the one exception.
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return "", false, nil
		}

	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "", false, nil

	case reflect.Struct:
		// time.Time is the only struct this store binds.
		if _, ok := rv.Interface().(time.Time); ok {
			return "", false, nil
		}
	}

	return "", false, fmt.Errorf("store: parameter of type %T is outside the shapes this store binds; "+
		"the NUL write guard refuses rather than passing an unclassifiable value through unchecked. If "+
		"this type is legitimate, add it to classify() and say whether it is text or binary", v)
}

// nulTriggerMarker is the string every Layer B trigger puts in its RAISE(ABORT)
// message, and the hook the driver error is classified by.
//
// A marker rather than a pattern-match on SQLite's phrasing, because that
// phrasing is the driver's to change and this is ours. It is greppable in both
// directions: a reader who sees it in a log finds the migration, and a reader
// of the migration finds this.
const nulTriggerMarker = "pad_nul_invariant"

// classifyTriggerRefusal converts a Layer B trigger abort into the SAME typed
// error Layer A produces, so a caller cannot tell which layer refused.
//
// That indistinguishability is the requirement, not a nicety. Ruling 2 admitted
// the second ring of caller-influenced columns — user agents, IP addresses,
// attachment filenames — on the condition that a header-derived value hitting a
// trigger must not surface as a 500 or a broken login. It reaches the handler
// as a 400 naming the rule because it becomes InvalidTextParameterError here.
//
// It also matters for the ordinary case: on a CURRENT binary Layer A answers
// first and the trigger never fires, so the only way to reach this code is a
// path Layer A does not cover — which is exactly when a caller most needs a
// comprehensible answer rather than an internal error.
func classifyTriggerRefusal(err error) error {
	if err == nil || !strings.Contains(err.Error(), nulTriggerMarker) {
		return err
	}
	// The message carries "pad_nul_invariant: <table>.<column> must not ...".
	// The COLUMN is safe to surface — it names the rule's subject — while the
	// value is not, and is never included.
	reason := "value contains a NUL byte, which no text or JSON column can store"
	if i := strings.Index(err.Error(), nulTriggerMarker+": "); i >= 0 {
		rest := err.Error()[i+len(nulTriggerMarker)+2:]
		if j := strings.Index(rest, " must not"); j > 0 {
			reason = "column " + rest[:j] + " must not contain a NUL byte, in a value or in a JSON escape that decodes to one"
		}
	}
	// NO ORDINAL. A trigger names the COLUMN, not the parameter position, and
	// the position is genuinely unknown here — the error comes back from the
	// database after the statement ran, not from inspecting an argument list.
	// The first version used Ordinal: 0, which violated the field's documented
	// 1-based contract and rendered as "parameter 0" (codex round 1).
	return &InvalidTextParameterError{Reason: reason}
}

type guardDriver struct{ base driver.Driver }

func (d guardDriver) Open(name string) (driver.Conn, error) {
	c, err := d.base.Open(name)
	if err != nil {
		return nil, err
	}
	if err := assertConnInterfaces(c); err != nil {
		_ = c.Close()
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
	if err := assertConnInterfaces(conn); err != nil {
		_ = conn.Close()
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
	if err := assertStmtInterfaces(st); err != nil {
		_ = st.Close()
		return nil, err
	}
	return guardStmt{st}, nil
}

func (c guardConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func (c guardConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	if err := normalizeAndCheck(args); err != nil {
		return nil, err
	}
	res, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, q, args)
	return res, classifyTriggerRefusal(err)
}

func (c guardConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if err := normalizeAndCheck(args); err != nil {
		return nil, err
	}
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
	return rows, classifyTriggerRefusal(err)
}

// guardStmt wraps only the two context forms, which every base statement here
// implements (asserted at registration). Statement-level NamedValueChecker and
// ColumnConverter are NOT forwarded because neither driver implements them —
// measured, and asserted, so a driver that starts to will fail registration
// rather than silently lose its conversion.
type guardStmt struct{ driver.Stmt }

func (s guardStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	if err := normalizeAndCheck(args); err != nil {
		return nil, err
	}
	res, err := s.Stmt.(driver.StmtExecContext).ExecContext(ctx, args)
	return res, classifyTriggerRefusal(err)
}

func (s guardStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	if err := normalizeAndCheck(args); err != nil {
		return nil, err
	}
	rows, err := s.Stmt.(driver.StmtQueryContext).QueryContext(ctx, args)
	return rows, classifyTriggerRefusal(err)
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
			// sql.Open does NOT connect, so this costs nothing and touches no
			// network. The interface assertions happen at connect time instead
			// — see assertConnInterfaces.
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

// assertConnInterfaces checks, on a REAL connection the caller already asked
// for, that the base implements everything the wrapper advertises flatly.
//
// It runs at CONNECT time, not at registration, and that is the fix for a
// genuinely dangerous first version (codex round 3): registration opened a
// probe *sql.DB for BOTH drivers and called Conn() on each, so creating a
// SQLite store attempted a live PostgreSQL connection using whatever the
// environment's default host, port and credentials happened to be — network
// access, and a wrong-server contact, as a side effect of opening a local file.
// Worse, the probe swallowed its own connection error, so the guarantees were
// skipped in exactly the case where the check could not run.
//
// Checking here costs one type-assert set per connection and answers about the
// connection actually in hand.
func assertConnInterfaces(c driver.Conn) error {
	var missing []string
	for _, want := range []struct {
		name string
		ok   bool
	}{
		{"Pinger", isIface[driver.Pinger](c)},
		{"SessionResetter", isIface[driver.SessionResetter](c)},
		{"ExecerContext", isIface[driver.ExecerContext](c)},
		{"QueryerContext", isIface[driver.QueryerContext](c)},
		{"ConnPrepareContext", isIface[driver.ConnPrepareContext](c)},
		{"ConnBeginTx", isIface[driver.ConnBeginTx](c)},
	} {
		if !want.ok {
			missing = append(missing, want.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("store: the database driver does not implement %v; the NUL write guard "+
			"advertises these unconditionally and would misreport them to database/sql — see "+
			"nulguard.go's shape note", missing)
	}
	return nil
}

// assertStmtInterfaces is the statement half, and it exists because the claim
// was previously made in a COMMENT and nowhere else (codex round 3): the code
// said statement-level NamedValueChecker and ColumnConverter were "measured
// absent on both drivers and asserted", and nothing asserted it. guardStmt does
// not forward either, so a driver that gained one would silently lose its own
// argument conversion.
//
// Checked on the first statement each connection prepares, for the same reason
// as the conn half: it is the object actually in hand.
func assertStmtInterfaces(st driver.Stmt) error {
	if !isIface[driver.StmtExecContext](st) || !isIface[driver.StmtQueryContext](st) {
		return errors.New("store: driver statements do not implement the context interfaces; the NUL " +
			"write guard advertises them unconditionally")
	}
	if isIface[driver.NamedValueChecker](st) {
		return errors.New("store: driver statements now implement NamedValueChecker, which the NUL write " +
			"guard does not forward — forwarding it needs a guardStmt variant, as the conn wrapper has")
	}
	//nolint:staticcheck // ColumnConverter is deprecated; a driver still using it would silently lose it
	if isIface[driver.ColumnConverter](st) {
		return errors.New("store: driver statements now implement ColumnConverter, which the NUL write " +
			"guard does not forward")
	}
	return nil
}

func isIface[T any](v any) bool {
	_, ok := v.(T)
	return ok
}
