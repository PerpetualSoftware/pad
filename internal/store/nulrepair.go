package store

import (
	"database/sql"
	"errors"
	"fmt"

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

	// The suspect class (see NULSuspect), kept in its own buckets so the
	// violation counts still match what `pad db scan-nul` promised.
	//
	// SuspectsClean is the common and boring outcome: the value carried a
	// literal the scanner had no reason to touch.
	SuspectsRepaired []NULSuspect
	SuspectsClean    []NULSuspect
	SuspectsSkipped  []NULSuspect
	SuspectsFailed   []NULSuspectFailure
}

// NULSuspectFailure is a suspect the repair tried and could not complete.
type NULSuspectFailure struct {
	Suspect NULSuspect
	Err     error
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
		if v.KeyIncomplete {
			report.Skipped = append(report.Skipped, NULRepairSkip{
				Violation: v,
				Reason: "one of the row's primary-key columns is NULL, so there is no WHERE clause that " +
					"selects exactly this row",
			})
			continue
		}

		if _, isKey := v.Key[v.Column]; isKey {
			report.Skipped = append(report.Skipped, NULRepairSkip{
				Violation: v,
				Reason: "the column is part of the row's primary key; repairing it would change the row's " +
					"identity and could collide with an existing row",
			})
			continue
		}

		// A NUL in a key column the list does NOT protect, on a row whose
		// violation is elsewhere. The address is then unusable for a different
		// reason than the case above: Layer A inspects every bound parameter,
		// including the ones in a WHERE clause, so the lookup is refused before
		// SQLite is asked to find the row.
		//
		// Detected here rather than left to the driver. Without this the row
		// lands in Failed carrying "invalid text parameter: parameter 2", which
		// says nothing an operator can act on — the same information, phrased as
		// a fault in the repair rather than as a property of the row (codex
		// round 3).
		if key, bad := nulBearingKey(v); bad {
			report.Skipped = append(report.Skipped, NULRepairSkip{
				Violation: v,
				Reason: "the row's " + key + " value itself contains a NUL, so no query can address this " +
					"row; repair or remove it by hand",
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

	// SUSPECTS, per the day-54 ruling. Most carry only a harmless literal and
	// come back unchanged; the one shape that matters — a NUL behind a literal
	// duplicate key — is fixed here and nowhere else, because the predicate
	// that gates the ordinary repair cannot see it.
	//
	// Reported separately from Repaired so the two counts stay honest: the
	// scan's violation count is what `pad db scan-nul` promised to change, and
	// folding suspects into it would make the dry run disagree with the run.
	for _, sus := range scan.Suspects {
		if sus.KeyIncomplete {
			report.SuspectsSkipped = append(report.SuspectsSkipped, sus)
			continue
		}
		changed, err := s.RepairSuspectValue(sus)
		switch {
		case err != nil:
			report.SuspectsFailed = append(report.SuspectsFailed, NULSuspectFailure{Suspect: sus, Err: err})
		case changed:
			report.SuspectsRepaired = append(report.SuspectsRepaired, sus)
		default:
			report.SuspectsClean = append(report.SuspectsClean, sus)
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
func nulRowPredicate(v NULViolation) (string, []any) { return nulKeyPredicate(v.Key) }

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

// nulBearingKey reports the first key column whose VALUE carries a NUL.
//
// Such a value cannot be bound: the store's write guard checks every parameter,
// not only the ones being written, so a WHERE clause carrying one is refused
// along with the statement it belongs to.
func nulBearingKey(v NULViolation) (string, bool) {
	for _, k := range sortedKeys(v.Key) {
		if textguard.ContainsNUL(v.Key[k]) {
			return k, true
		}
	}
	return "", false
}
