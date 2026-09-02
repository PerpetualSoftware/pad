package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/textguard"
)

// The explicit operator repair for the legacy NUL population (DOC-2823 S3).
//
// NEVER A MIGRATION, on Dave's day-54 ruling: a migration that rewrites user
// content decides consent for the operator. This runs only when somebody types
// `pad db repair-nul`, and `pad db migrate-to-pg` refuses and prints that
// command rather than repairing on the operator's behalf.
//
// IN GO, NOT IN SQL, for the reason TASK-2824 measured and this unit measured
// again on the read path: SQLite's own string functions disagree about a
// NUL-bearing value (`length()` answers 3 for an 8-byte value), so no SQL-side
// transform can be trusted to leave the rest of the value intact. The rewrite
// is textguard.Repair, which is also what the four enforcement layers are
// measured against.

// RepairNULCommand is the exact command an operator runs to fix the rows this
// file repairs.
//
// It lives here, in the package that implements the repair, because three
// places quote it at an operator — the migrate-to-pg preflight, the import's
// strict refusal, and the CLI's own help — and a remedy naming a command that
// has been renamed is worse than no remedy at all.
const RepairNULCommand = "pad db repair-nul"

// NULRepairSkip is a violation the repair deliberately did not touch.
type NULRepairSkip struct {
	Violation NULViolation
	Reason    string
}

// NULRepairFailure is a violation the repair tried and could not complete.
type NULRepairFailure struct {
	Violation NULViolation
	Err       error
}

// NULRepairReport is what the repair returns. Every violation the scan found
// ends up in exactly one of the three buckets, which is what lets the CLI
// report a total that adds up.
type NULRepairReport struct {
	Scan     *NULScanReport
	Repaired []NULViolation
	Skipped  []NULRepairSkip
	Failed   []NULRepairFailure
}

// RepairNUL rewrites every offending stored value, replacing each NUL with
// U+FFFD, and reports what it changed.
//
// PER-ROW TRANSACTIONS, not one big one. The repair is idempotent — running it
// twice changes nothing the second time, pinned in textguard — so a partial run
// is a resumable state rather than a corrupt one, and that is worth more here
// than atomicity across an unbounded number of rows: a single transaction over
// a large database holds SQLite's write lock for the whole sweep, which on the
// one deployment shape this exists for (a self-hoster's live instance) is the
// difference between a repair and an outage.
//
// Each row IS read and written inside its own transaction, so the value cannot
// change between the read and the rewrite.
func (s *Store) RepairNUL() (*NULRepairReport, error) {
	scan, err := s.ScanNUL()
	if err != nil {
		return nil, err
	}
	report := &NULRepairReport{Scan: scan}
	if !scan.Applicable {
		return report, nil
	}

	for _, v := range scan.Violations {
		// THE PRIMARY KEY IS NOT REWRITTEN. Repairing a column that is part of
		// its own row's key changes the row's identity, and U+FFFD substitution
		// can land it on top of an existing row — for email_optouts(email), the
		// only such column today, that would silently merge two opt-out records
		// and could un-suppress mail to somebody. Refusing to guess is the
		// posture the cross-workspace copy takes when a destination field needs
		// a value; the operator is told which rows and why.
		//
		// It is also mechanically impossible through this handle: Layer A
		// inspects every bound parameter, so a WHERE clause carrying the
		// NUL-bearing key would be refused along with the write.
		if _, isKey := v.Key[v.Column]; isKey {
			report.Skipped = append(report.Skipped, NULRepairSkip{
				Violation: v,
				Reason: "the column is part of the row's primary key; repairing it would change the row's " +
					"identity and could collide with an existing row",
			})
			continue
		}

		repaired, err := s.repairOneNUL(v)
		switch {
		case err != nil:
			report.Failed = append(report.Failed, NULRepairFailure{Violation: v, Err: err})
		case repaired:
			report.Repaired = append(report.Repaired, v)
		default:
			// The value was clean by the time the transaction read it —
			// somebody else repaired it, or the row changed. Not a failure and
			// not a repair.
			report.Skipped = append(report.Skipped, NULRepairSkip{
				Violation: v,
				Reason:    "the value no longer violates the invariant; nothing to do",
			})
		}
	}
	return report, nil
}

// repairOneNUL reads, rewrites and writes one value inside one transaction.
// It reports whether it actually changed anything.
func (s *Store) repairOneNUL(v NULViolation) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	where, args := nulRowPredicate(v)
	qc := quoteIdent(v.Column)
	qt := quoteIdent(v.Table)

	var value string
	err = tx.QueryRow(fmt.Sprintf(`SELECT %s FROM %s WHERE %s`, qc, qt, where), args...).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s.%s: %w", v.Table, v.Column, err)
	}

	isJSON := nulColumnIsJSON(v.Table, v.Column)
	if !textguard.ParameterRefused(value, isJSON) {
		return false, nil
	}

	repaired := textguard.Repair(value, isJSON)
	if repaired == value {
		// textguard.Repair is required to change any refused value; a no-op
		// here would mean the predicate and the repair disagree, and looping
		// on it or reporting success would both be lies.
		return false, fmt.Errorf("repair produced no change for a refused value in %s.%s", v.Table, v.Column)
	}

	updateArgs := append([]any{repaired}, args...)
	res, err := tx.Exec(fmt.Sprintf(`UPDATE %s SET %s = ? WHERE %s`, qt, qc, where), updateArgs...)
	if err != nil {
		return false, fmt.Errorf("update %s.%s: %w", v.Table, v.Column, err)
	}

	// THE ROW COUNT IS CHECKED, and it is not defensive padding. The WHERE
	// clause is built from values the scan read back out of the database, and
	// one of them is a `rowid` for the single protected table that declares no
	// primary key — an INTEGER column addressed with the TEXT the scan scanned
	// it into. If any such binding ever stopped matching, this function would
	// commit an UPDATE that touched nothing and report the value as repaired.
	// A repair that reports success for a row it did not change is the one
	// failure mode an operator cannot detect from the output.
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected for %s.%s: %w", v.Table, v.Column, err)
	}
	if n != 1 {
		return false, fmt.Errorf("repairing %s.%s matched %d rows, want exactly 1 — the row address the "+
			"scan produced does not select it", v.Table, v.Column, n)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}

// nulRowPredicate renders the WHERE clause addressing one row, plus its args.
func nulRowPredicate(v NULViolation) (string, []any) {
	keys := sortedKeys(v.Key)
	clauses := make([]string, 0, len(keys))
	args := make([]any, 0, len(keys))
	for _, k := range keys {
		clauses = append(clauses, quoteIdent(k)+" = ?")
		args = append(args, v.Key[k])
	}
	return strings.Join(clauses, " AND "), args
}

// nulColumnIsJSON answers the classing question from the shared list, so the
// repair and the predicate cannot disagree about a column.
func nulColumnIsJSON(table, column string) bool {
	for _, c := range NULProtectedColumns() {
		if c.Table == table && c.Column == column {
			return c.Class == classJSON
		}
	}
	return false
}
