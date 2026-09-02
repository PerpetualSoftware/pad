package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/textguard"
)

// The suspect class's two operations (DOC-2823 S3, the day-54 lead ruling on
// PR #1233): ask the DESTINATION whether a suspect is really fatal, and repair
// the one shape that is.
//
// The ruling's shape, and why it is not a fourth predicate: our layers cannot
// tell a harmless doubled-backslash literal from a NUL hidden behind a literal
// duplicate key, because both look identical to a map-model decode — that is
// textguard.KnownGaps and DOC-2823 forbids closing it in one layer. But the
// migration has something no layer has: the actual PostgreSQL that is about to
// refuse the value. Casting it there is not an opinion, it is the oracle, and
// it is exact in both directions.

// ReadNULTargetValue reads back the value at a suspect's address.
//
// The scan report deliberately carries no user content, so anything that needs
// the bytes — the destination cast, the repair — fetches them by address. Few
// suspects and one row each, so the extra read is not worth avoiding.
func (s *Store) ReadNULTargetValue(table, column string, key map[string]string) (string, error) {
	if len(key) == 0 {
		return "", fmt.Errorf("no address for %s.%s", table, column)
	}
	where, args := nulKeyPredicate(key)
	var value string
	err := s.db.QueryRow(
		fmt.Sprintf(`SELECT %s FROM %s WHERE %s`, quoteIdent(column), quoteIdent(table), where),
		args...,
	).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("row %s.%s no longer exists", table, column)
	}
	if err != nil {
		return "", fmt.Errorf("read %s.%s: %w", table, column, err)
	}
	return value, nil
}

// ErrNULDestinationRefused is the sentinel behind a destination cast that
// failed for a NUL reason.
var ErrNULDestinationRefused = errors.New("store: destination refused the value")

// CheckJSONBAcceptable asks THIS store's database whether a value is a jsonb
// document it will accept, without writing anything.
//
// `SELECT $1::jsonb` is side-effect-free: no table is touched, no transaction
// state changes, and the cast is exactly the one an INSERT into a jsonb column
// performs. That makes it a faithful oracle rather than a model of one.
//
// The verdict is narrow ON PURPOSE. Only the two NUL SQLSTATEs — 22P05 for an
// escape decoding to NUL inside jsonb, 22021 for a NUL in text — wrap
// ErrNULDestinationRefused. Any OTHER cast failure comes back as a plain error:
// it means the destination will reject the row too, but for a reason outside
// this preflight's remit, and a NUL preflight that silently grew into a general
// one would refuse migrations that have nothing to do with this bug. The caller
// reports those instead of refusing on them.
//
// SQLSTATE by string match rather than a pgconn type assertion, following
// isDeadlockError in this package: internal/store keeps both drivers behind
// database/sql, and pgx puts the code verbatim in the error text
// ("... (SQLSTATE 22P05)"). TestDestinationOracleClassifiesRealPostgresErrors
// pins the match against a real server rather than assuming the wording.
func (s *Store) CheckJSONBAcceptable(value string) error {
	if s.dialect.Driver() != DriverPostgres {
		return fmt.Errorf("the jsonb cast oracle needs a PostgreSQL destination")
	}
	var out []byte
	err := s.db.QueryRow(`SELECT $1::jsonb`, value).Scan(&out)
	if err == nil {
		return nil
	}
	if isNULSQLState(err) {
		return fmt.Errorf("%w: %v", ErrNULDestinationRefused, err)
	}
	return err
}

// isNULSQLState reports whether err carries one of PostgreSQL's two NUL codes.
func isNULSQLState(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToUpper(err.Error())
	return strings.Contains(msg, "22P05") || strings.Contains(msg, "22021")
}

// RepairSuspectValue rewrites a suspect's NUL escapes with U+FFFD, using the
// TOKEN-level scanner rather than the predicate-gated repair.
//
// MEASURED, because the ruling asked and because the answer decides whether the
// preflight's remedy works: `textguard.Repair` leaves the shadowed-duplicate
// shape completely untouched. Its scanner is gated on
// DocumentDecodesNULAnyShape, which is a map-model question and answers false
// for exactly this value, so the scanner never runs. Probed on the branch:
//
//	Repair(v, true)        -> unchanged
//	the scanner directly   -> the shadowed escape rewritten, replaced=1
//
// Without this, `pad db migrate-to-pg` would refuse a suspect and print a
// repair command that does nothing to it — a remedy that is a contract claim
// nobody ran (PATTE-135). So the repair reaches the suspect class through the
// scanner, which is sound here for the same reason it is sound everywhere: it
// rewrites only escapes a JSON parser would decode, and consumes escapes in
// order, so a doubled-backslash literal is left exactly as it was.
//
// This does NOT widen what any layer REFUSES. KnownGaps is untouched; what
// changed is what an operator's explicit repair is allowed to FIX.
func (s *Store) RepairSuspectValue(sus NULSuspect) (repaired bool, err error) {
	if sus.KeyIncomplete || len(sus.Key) == 0 {
		return false, fmt.Errorf("no address for %s.%s", sus.Table, sus.Column)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	where, args := nulKeyPredicate(sus.Key)
	qc := quoteIdent(sus.Column)
	qt := quoteIdent(sus.Table)

	var value string
	err = tx.QueryRow(fmt.Sprintf(`SELECT %s FROM %s WHERE %s`, qc, qt, where), args...).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s.%s: %w", sus.Table, sus.Column, err)
	}

	next, n := textguard.RepairJSONEscapes(value)
	if n == 0 || next == value {
		// A harmless literal — the common case. Nothing to do, and saying so is
		// not a failure.
		return false, nil
	}

	res, err := tx.Exec(fmt.Sprintf(`UPDATE %s SET %s = ? WHERE %s`, qt, qc, where),
		append([]any{next}, args...)...)
	if err != nil {
		return false, fmt.Errorf("update %s.%s: %w", sus.Table, sus.Column, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected for %s.%s: %w", sus.Table, sus.Column, err)
	}
	if rows != 1 {
		return false, fmt.Errorf("repairing %s.%s matched %d rows, want exactly 1", sus.Table, sus.Column, rows)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}

// nulKeyPredicate renders a WHERE clause addressing one row from its key map.
func nulKeyPredicate(key map[string]string) (string, []any) {
	keys := sortedKeys(key)
	clauses := make([]string, 0, len(keys))
	args := make([]any, 0, len(keys))
	for _, k := range keys {
		clauses = append(clauses, quoteIdent(k)+" = ?")
		args = append(args, key[k])
	}
	return strings.Join(clauses, " AND "), args
}
