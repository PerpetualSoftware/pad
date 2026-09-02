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
// refuse the value. Casting it there is not an opinion, it is the oracle.
//
// It is exact about THE VALUE. It is not a perfect model of the MIGRATION, and
// the difference is measured rather than hand-waved: one write path normalises
// before writing, so a value the cast refuses can still import through that
// column. See the KNOWN OVER-REFUSAL note on CheckJSONBAcceptable.

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

// ErrDestinationCheckUnavailable is the sentinel for a check that did not
// COMPLETE — a dropped connection, a timeout, anything that is not the
// database's verdict on the value.
//
// It exists so a caller can fail CLOSED. An unverified suspect treated as a
// pass is the preflight promising a migration it did not check, which is the
// defect the suspect class was added to correct, arriving by a different route
// (codex round 5).
var ErrDestinationCheckUnavailable = errors.New("store: could not ask the destination")

// CheckJSONBAcceptable asks THIS store's database whether a value is a jsonb
// document it will accept, without writing anything.
//
// `SELECT $1::jsonb` is side-effect-free: no table is touched, no transaction
// state changes, and the cast is exactly the one an INSERT into a jsonb column
// performs. That makes it a faithful oracle rather than a model of one.
//
// The verdict is narrow ON PURPOSE. Only the two NUL SQLSTATEs — 22P05 for an
// escape decoding to NUL inside jsonb, 22021 for a NUL in text — wrap
// ErrNULDestinationRefused. Another cast failure that the SERVER answered comes
// back as a plain error: the destination will reject the row too, but for a
// reason outside this preflight's remit, and a NUL preflight that silently grew
// into a general one would refuse migrations that have nothing to do with this
// bug. Anything else — no SQLSTATE, or an OPERATIONAL one such as a cancelled
// query or a terminated connection — wraps ErrDestinationCheckUnavailable, and
// the caller refuses on those. See classifyDestinationError for why the test is
// "is this class 22" rather than "did the server answer".
//
// KNOWN OVER-REFUSAL, measured rather than reasoned about (codex round 5).
// This casts the value AS STORED, and one write path normalises before writing:
// CreateWorkspace runs models.NormalizeWorkspaceSettings, a map round-trip, so
// a shadowed-duplicate in workspaces.settings collapses to its surviving member
// and imports cleanly. Measured against a real server by importing the same
// value into three columns:
//
//	workspaces.settings  -> import SUCCEEDS, stored as {"a": "clean"}
//	items.fields         -> import FAILS, SQLSTATE 22P05
//	collections.schema   -> import FAILS, SQLSTATE 22P05
//
// So for that one column the preflight refuses a migration that would have gone
// through. Left as-is deliberately: the row is a violation of the invariant
// wherever it sits — Layer B refuses that value on every write today, and it
// exists only because it predates enforcement — so surviving the migration is
// an accident of one column's normaliser rather than a property worth
// preserving, and `pad db repair-nul` clears it in one command. Deriving
// "would this column's writer normalise it" is a per-column enumeration, which
// is the shape this cluster keeps proving unmaintainable. Flagged to the lead
// rather than decided here; the REFUSAL WORDING no longer claims PostgreSQL
// would reject the row, only that the value carries a NUL jsonb refuses.
//
// SQLSTATE by string extraction rather than a pgconn type assertion, following
// isDeadlockError in this package: internal/store keeps both drivers behind
// database/sql, and pgx puts the code verbatim in the error text
// ("... (SQLSTATE 22P05)"). TestDestinationOracleClassifiesRealPostgresErrors
// pins the extraction against a real server rather than assuming the wording.
func (s *Store) CheckJSONBAcceptable(value string) error {
	if s.dialect.Driver() != DriverPostgres {
		return fmt.Errorf("the jsonb cast oracle needs a PostgreSQL destination")
	}
	var out []byte
	err := s.db.QueryRow(`SELECT $1::jsonb`, value).Scan(&out)
	if err == nil {
		return nil
	}
	switch classifyDestinationError(err) {
	case destinationRefusedNUL:
		return fmt.Errorf("%w: %v", ErrNULDestinationRefused, err)
	case destinationRejectedValue:
		return err
	default:
		return fmt.Errorf("%w: %v", ErrDestinationCheckUnavailable, err)
	}
}

// destinationVerdict is what a failed cast tells us.
type destinationVerdict int

const (
	// destinationUnavailable: the server did not render a verdict about the
	// value. No SQLSTATE at all, or an OPERATIONAL one.
	destinationUnavailable destinationVerdict = iota
	// destinationRejectedValue: the server judged the value and refused it, for
	// a reason that is not a NUL.
	destinationRejectedValue
	// destinationRefusedNUL: the server judged the value and refused it for a
	// NUL.
	destinationRefusedNUL
)

// classifyDestinationError decides which of the three a cast failure is.
//
// THE TEST IS INVERTED FROM THE OBVIOUS ONE, and that inversion is the fix
// (codex round 6). The first version asked "did the server answer at all",
// treating every SQLSTATE-bearing error as a verdict about the value — but
// 57014 (query cancelled), 57P01 (terminated by administrator), the 08 class
// (connection exception) and the 53 class (out of resources) all carry
// SQLSTATEs and say nothing about the value. Classified as verdicts, they let
// the preflight proceed with an unverified suspect, which is the fail-open this
// whole three-way split exists to close, one level deeper.
//
// So only CLASS 22 — data exception, PostgreSQL's class for "this value is
// wrong" — counts as a verdict. `SELECT $1::jsonb` produces 22P02 for
// malformed JSON and 22P05 / 22021 for the NUL cases; everything else means the
// question was not answered, and the caller refuses rather than guessing.
//
// Erring toward "unavailable" is the safe direction: its cost is a refused
// migration an operator re-runs, against a half-finished one they have to
// unpick.
func classifyDestinationError(err error) destinationVerdict {
	code := sqlStateOf(err)
	switch code {
	case "22P05", "22021":
		return destinationRefusedNUL
	case "":
		return destinationUnavailable
	}
	if strings.HasPrefix(code, "22") {
		return destinationRejectedValue
	}
	return destinationUnavailable
}

// sqlStateOf extracts the five-character error code pgx renders into a
// server-side error's message, or "" when there is none.
//
// String extraction rather than a pgconn type assertion, following
// isDeadlockError in this package: internal/store keeps both drivers behind
// database/sql. The RENDERING is pinned by tests that provoke real errors from
// a real server rather than by assuming the wording.
func sqlStateOf(err error) string {
	if err == nil {
		return ""
	}
	// ONE string for both the search and the slice. The first version indexed
	// into strings.ToUpper(msg) and then sliced the ORIGINAL, which is only
	// safe while every byte before the marker is ASCII: Unicode case mapping
	// changes byte LENGTH for some runes, so a localised server message —
	// lc_messages is a per-server setting — shifts the offset and the five
	// bytes taken are the wrong five (codex round 7).
	const marker = "SQLSTATE "
	msg := strings.ToUpper(err.Error())
	i := strings.Index(msg, marker)
	if i < 0 {
		return ""
	}
	rest := msg[i+len(marker):]
	if len(rest) < 5 {
		return ""
	}
	return rest[:5]
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

// MigratedTables names the tables `pad db migrate-to-pg` actually copies.
//
// The migration is application-level: it walks workspaces and runs
// ExportWorkspace / ImportWorkspace on each. That reads six tables and no
// others — the command's own help says users, platform settings and auth data
// are NOT migrated — so a NUL in users.name, platform_settings.value,
// sessions.user_agent or any oauth table cannot break it.
//
// The preflight uses this to decide what to REFUSE on, not what to REPORT
// (codex round 9). Scanning everything is right: `pad db scan-nul` is about the
// database, and an operator should hear about every affected row. Blocking a
// migration over a row it will never touch is not — it demands the operator
// rewrite content that has nothing to do with the copy they asked for.
//
// TestMigratedTablesCoversTheExport pins this against models.WorkspaceExport's
// own shape, so a new export section fails here rather than silently making the
// preflight miss a table.
//
// KNOWN RESIDUAL, stated because it is the same class of over-refusal one size
// smaller: the export also skips SOFT-DELETED collections and items
// (`deleted_at IS NULL`), and this filter is per-table, not per-row. A NUL in a
// soft-deleted item still blocks a migration that would not have carried it.
// Narrowing that needs a per-row deleted_at check at every candidate, which is
// more machinery than the remaining over-refusal costs — the operator's way out
// is the same single repair command either way.
func MigratedTables() map[string]bool {
	return map[string]bool{
		"workspaces":    true,
		"collections":   true,
		"items":         true,
		"comments":      true,
		"item_links":    true,
		"item_versions": true,
	}
}
