package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/textguard"
)

// The counter and the repair for the NUL invariant's LEGACY population
// (DOC-2823 S3, closing BUG-2810).
//
// Layers A and B stop the value being WRITTEN. Neither of them makes a row that
// already carries one go away, and such a row is not merely untidy: its
// workspace exports fine and re-imports 400, and `pad db migrate-to-pg` fails
// partway through the copy against PostgreSQL's jsonb parser. This file is the
// read-only census of that population and the explicit operator repair.
//
// THREE PROPERTIES, and each of them was a decision:
//
// ONE PREDICATE. The decision about a value is textguard.ParameterRefused with
// isJSON taken from nulColumns — the same core the HTTP gate, Layer A and Layer
// B share, with Layer B's column-derived classing. The SQL below narrows, it
// never decides. A fourth implementation of "decodes to a NUL" is the thing
// this whole cluster exists because of.
//
// THE COUNT IS COMPUTED IN GO, NOT IN SQL. Measured on the read path in this
// worktree: a row planted with `bad<NUL>name` reads back into a Go string with
// all 8 bytes and the NUL intact, while `length(name)` in the same database
// answers 3. TASK-2824 found that C-truncation in SQLite's string functions and
// concluded no DB-side REPAIR could be trusted; the same measurement on the
// read path says no DB-side COUNT can be either.
//
// SQLITE ONLY, and not by omission. PostgreSQL cannot hold either defect —
// SQLSTATE 22021 for a raw NUL in text, 22P05 for the escape reaching jsonb —
// which is why Layer B was ruled not to apply there, and the four-way
// differential test pins that native refusal. A scan of a Postgres database
// would be a full table scan of every protected column to prove a theorem.
// ScanNUL says so and returns an empty report rather than pretending it looked.

// NULViolation is one stored value that violates the invariant.
type NULViolation struct {
	Table  string
	Column string
	// Key addresses the row: the declared primary-key columns and their
	// values, or `rowid` for the one table that declares no primary key.
	Key map[string]string
	// WorkspaceID is the owning workspace, or "" for the 16 protected tables
	// that carry no workspace_id column — see the census in nulscan_test.go.
	WorkspaceID string
	// RawNUL and EscapedNUL record WHICH defect the value carries. They are
	// not exclusive: a JSON blob can hold both, and the repair has a separate
	// pass for each.
	RawNUL     bool
	EscapedNUL bool
	// KeyIncomplete marks a row one of whose key columns is NULL, so Key does
	// not address it. SQLite permits NULL in a declared PRIMARY KEY that is
	// neither INTEGER PRIMARY KEY nor NOT NULL, which no other engine does.
	// The scan still REPORTS such a row — it is real and it is broken — and
	// the repair skips it rather than issuing a WHERE that matches nothing.
	KeyIncomplete bool
}

// String renders a violation the way the CLI reports it.
func (v NULViolation) String() string {
	parts := make([]string, 0, len(v.Key))
	for _, k := range sortedKeys(v.Key) {
		parts = append(parts, k+"="+v.Key[k])
	}
	kind := "raw NUL"
	switch {
	case v.RawNUL && v.EscapedNUL:
		kind = "raw NUL + escaped NUL"
	case v.EscapedNUL:
		kind = "escaped NUL"
	}
	out := fmt.Sprintf("%s.%s [%s] (%s)", v.Table, v.Column, strings.Join(parts, ", "), kind)
	if v.WorkspaceID != "" {
		out += " workspace=" + v.WorkspaceID
	}
	return out
}

// NULScanReport is what the counter returns.
type NULScanReport struct {
	// Applicable is false on PostgreSQL, where the state cannot exist. Reason
	// says why nothing was scanned, so a zero report is never mistaken for a
	// scan that found nothing.
	Applicable bool
	Reason     string

	// Violations is every offending value, in table/column order. The
	// population this exists for is legacy rows on a single self-hosted
	// database; it is not bounded, because an operator deciding whether to
	// repair needs the whole list and a truncated one would understate it.
	Violations []NULViolation

	// ColumnsScanned is how many of the protected columns actually exist in
	// this database's schema, and ColumnsAbsent lists any that do not.
	// A database at an older migration is not an error, but a scan that
	// silently skipped columns is not a census.
	ColumnsScanned int
	ColumnsAbsent  []string
}

// Total is the number of offending values.
func (r *NULScanReport) Total() int { return len(r.Violations) }

// ByWorkspace groups the violation count by workspace id, with "" collecting
// the tables that have no workspace column.
func (r *NULScanReport) ByWorkspace() map[string]int {
	out := map[string]int{}
	for _, v := range r.Violations {
		out[v.WorkspaceID]++
	}
	return out
}

// ByColumn groups the violation count by "table.column".
func (r *NULScanReport) ByColumn() map[string]int {
	out := map[string]int{}
	for _, v := range r.Violations {
		out[v.Table+"."+v.Column]++
	}
	return out
}

// ScanNUL counts and locates every stored value violating the NUL invariant.
//
// Read-only: it issues SELECTs and nothing else, so it is safe to run against a
// live server and safe to run repeatedly. `pad db migrate-to-pg` calls it as a
// preflight for exactly that reason.
//
// ONE RESIDUAL, and it is inherited rather than introduced. The decision is
// textguard's predicate, which shares the HTTP gate's map-model blind spots
// until BUG-2812's token-walk replaces the decode — today that is a JSON
// document with LITERAL duplicate keys, where the decode keeps the last one and
// a NUL in a shadowed value is never seen (textguard.KnownGaps). PostgreSQL
// refuses such a value, so a database carrying one passes the migrate-to-pg
// preflight and then fails during the copy: for that one shape the preflight
// does not deliver what it promises. Closing it HERE is explicitly what
// DOC-2823 forbids — Layer A "must NOT quietly fix either gap on its own",
// because layers disagreeing about one value is the defect this whole cluster
// is made of. TestScanNULInheritsTheRecordedKnownGaps pins the miss and fails
// when it stops being one.
//
// COST, stated because an operator should not be surprised by it: one
// unindexed scan per protected column — 131 of them today (24 JSON-classed,
// 107 text), measured from NULProtectedColumns rather than counted by hand.
// There is no index that would help, since the predicate is a substring search
// over the value. That is cheap next to the migration it guards, which reads
// every row of every table anyway.
func (s *Store) ScanNUL() (*NULScanReport, error) {
	if s.dialect.Driver() != DriverSQLite {
		return &NULScanReport{
			Applicable: false,
			Reason: "PostgreSQL refuses these values natively (SQLSTATE 22021 for a NUL in text, " +
				"22P05 for the escape reaching jsonb), so no stored row can carry one",
		}, nil
	}

	live, err := s.liveColumnTypes()
	if err != nil {
		return nil, err
	}

	report := &NULScanReport{Applicable: true}
	cols := NULProtectedColumns()
	sort.Slice(cols, func(i, j int) bool {
		if cols[i].Table != cols[j].Table {
			return cols[i].Table < cols[j].Table
		}
		return cols[i].Column < cols[j].Column
	})

	addressing := map[string]tableAddressing{}
	for _, c := range cols {
		if !live[c.Table+"."+c.Column] {
			report.ColumnsAbsent = append(report.ColumnsAbsent, c.Table+"."+c.Column)
			continue
		}
		report.ColumnsScanned++

		addr, ok := addressing[c.Table]
		if !ok {
			addr, err = s.addressingFor(c.Table)
			if err != nil {
				return nil, err
			}
			addressing[c.Table] = addr
		}

		found, err := s.scanColumn(c, addr)
		if err != nil {
			return nil, err
		}
		report.Violations = append(report.Violations, found...)
	}
	sort.Strings(report.ColumnsAbsent)
	return report, nil
}

// tableAddressing is how a row in one table is named and attributed.
type tableAddressing struct {
	// KeyColumns are the declared primary-key columns, or {"rowid"} for a
	// table that declares none. item_wiki_links is the only such table today
	// (measured); rowid is a correct address for it because it is not
	// declared WITHOUT ROWID.
	KeyColumns []string
	// HasWorkspace says whether the table carries a workspace_id.
	HasWorkspace bool
	// KeyIsProtected marks a table whose PRIMARY KEY is itself a protected
	// column. Repairing such a row rewrites its identity — and can collide
	// with an existing row — so the repair refuses it and says so.
	// email_optouts(email) is the only one today.
	KeyIsProtected bool
}

// addressingFor reads a table's primary key and workspace column from the live
// schema rather than from a list, because a hand-kept table→key mapping is the
// enumeration this cluster keeps proving unmaintainable.
func (s *Store) addressingFor(table string) (tableAddressing, error) {
	rows, err := s.db.Query(`SELECT name, pk FROM pragma_table_info(?)`, table)
	if err != nil {
		return tableAddressing{}, fmt.Errorf("table_info %s: %w", table, err)
	}
	defer rows.Close()

	var addr tableAddressing
	type pkCol struct {
		name string
		pos  int
	}
	var pks []pkCol
	for rows.Next() {
		var name string
		var pos int
		if err := rows.Scan(&name, &pos); err != nil {
			return tableAddressing{}, err
		}
		if name == "workspace_id" {
			addr.HasWorkspace = true
		}
		if pos > 0 {
			pks = append(pks, pkCol{name, pos})
		}
	}
	if err := rows.Err(); err != nil {
		return tableAddressing{}, err
	}

	// pragma_table_info's `pk` is the 1-based position WITHIN the key, so a
	// composite key must be ordered by it rather than by column order.
	sort.Slice(pks, func(i, j int) bool { return pks[i].pos < pks[j].pos })
	for _, p := range pks {
		addr.KeyColumns = append(addr.KeyColumns, p.name)
	}
	if len(addr.KeyColumns) == 0 {
		addr.KeyColumns = []string{"rowid"}
	}

	protected := map[string]bool{}
	for _, c := range NULProtectedColumns() {
		if c.Table == table {
			protected[c.Column] = true
		}
	}
	for _, k := range addr.KeyColumns {
		if protected[k] {
			addr.KeyIsProtected = true
		}
	}
	return addr, nil
}

// scanColumn narrows in SQL and decides in Go.
//
// The WHERE clause is a PRE-FILTER on the two byte patterns a violating value
// must contain, and it is the same pre-filter textguard applies internally
// before it pays for a decode. It can only ever return a superset:
//
//   - `instr(col, char(0))` finds a raw NUL. Measured (TASK-2824, and again on
//     the read path here): instr searches the whole stored value, unlike
//     length(), which stops at the NUL.
//   - `instr(col, '\u00')` finds the four characters every NUL escape starts
//     with. Only JSON-classed columns get it: in a text column those six
//     characters are six characters, and refusing them there is the false
//     positive that made BUG-2803's parity pre-filter unsound.
//
// Whether the match MEANS anything is then textguard's call, on the value read
// into Go — which is what keeps `{"a":"x\\u0000y"}` out of the report.
func (s *Store) scanColumn(c nulColumn, addr tableAddressing) ([]NULViolation, error) {
	qt := quoteIdent(c.Table)
	qc := quoteIdent(c.Column)

	sel := make([]string, 0, len(addr.KeyColumns)+2)
	for _, k := range addr.KeyColumns {
		sel = append(sel, quoteIdent(k))
	}
	if addr.HasWorkspace {
		sel = append(sel, `"workspace_id"`)
	}
	sel = append(sel, qc)

	where := fmt.Sprintf(`%s IS NOT NULL AND (instr(%s, char(0)) > 0`, qc, qc)
	args := []any{}
	if c.Class == classJSON {
		where += fmt.Sprintf(` OR instr(%s, ?) > 0`, qc)
		args = append(args, nulEscapePrefix)
	}
	where += `)`

	q := fmt.Sprintf(`SELECT %s FROM %s WHERE %s`, strings.Join(sel, ", "), qt, where)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("scan %s.%s: %w", c.Table, c.Column, err)
	}
	defer rows.Close()

	var out []NULViolation
	for rows.Next() {
		// EVERY column is scanned as a NULLABLE string, and that is not
		// defensive padding — a plain *string fails with "converting NULL to
		// string is unsupported" on the first row it meets, and the whole scan
		// (and therefore the repair, and the migrate-to-pg preflight) fails
		// with it. Two of the three can be NULL on an ordinary database:
		// workspace_id is nullable on several protected tables, and SQLite —
		// unlike every other engine — permits NULL in a PRIMARY KEY column
		// that is not INTEGER PRIMARY KEY or explicitly NOT NULL. Only the
		// value column is guaranteed non-NULL, by the query's own WHERE.
		dest := make([]any, 0, len(sel))
		keyVals := make([]sql.NullString, len(addr.KeyColumns))
		for i := range keyVals {
			dest = append(dest, &keyVals[i])
		}
		var wsID sql.NullString
		if addr.HasWorkspace {
			dest = append(dest, &wsID)
		}
		var value string
		dest = append(dest, &value)

		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("scan %s.%s row: %w", c.Table, c.Column, err)
		}

		isJSON := c.Class == classJSON
		if !textguard.ParameterRefused(value, isJSON) {
			// The pre-filter matched and the predicate did not. This is the
			// doubled-backslash case and it is EXPECTED, not an anomaly — the
			// pre-filter is allowed to be a superset and is worthless if it is
			// not.
			continue
		}

		v := NULViolation{
			Table:       c.Table,
			Column:      c.Column,
			Key:         map[string]string{},
			WorkspaceID: wsID.String,
			RawNUL:      textguard.ContainsNUL(value),
		}
		v.EscapedNUL = isJSON && textguard.DocumentDecodesNULAnyShape(value)
		for i, k := range addr.KeyColumns {
			if !keyVals[i].Valid {
				// A NULL key column cannot address the row for an UPDATE
				// (`WHERE k = NULL` matches nothing), so the repair must not
				// be handed one. Reported, with the address it could build, so
				// the operator sees the row exists rather than having it
				// vanish from a census.
				v.KeyIncomplete = true
				continue
			}
			v.Key[k] = keyVals[i].String
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// nulEscapePrefix is the four characters every NUL escape begins with, built
// rather than typed: typing them produces the CHARACTER in a Go source file,
// which is the decay corpus.go records and which happened twice while writing
// this unit.
var nulEscapePrefix = textguard.EscNUL[:4]

// liveColumnTypes reports which table.column pairs actually exist, so a
// database at an older migration is scanned for what it has rather than
// erroring on a column the list knows about and the schema does not.
func (s *Store) liveColumnTypes() (map[string]bool, error) {
	rows, err := s.db.Query(`
		SELECT m.name, ti.name
		FROM sqlite_master m, pragma_table_info(m.name) ti
		WHERE m.type = 'table'
	`)
	if err != nil {
		return nil, fmt.Errorf("read live schema: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var table, col string
		if err := rows.Scan(&table, &col); err != nil {
			return nil, err
		}
		out[table+"."+col] = true
	}
	return out, rows.Err()
}

// quoteIdent quotes a SQL identifier. The names come from this package's own
// list rather than from user input, but the trigger restoration learned that
// quoting is worth having anyway when identifiers are interpolated at all.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
