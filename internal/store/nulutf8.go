package store

import (
	"database/sql"
	"fmt"
	"strings"
	"unicode/utf8"
)

// scanColumnUTF8 finds every value in one protected column that is not valid
// UTF-8 (BUG-3222).
//
// PostgreSQL refuses such a value in text or jsonb (SQLSTATE 22021 under a
// UTF8 database), and SQLite stores it without complaint, so a legacy row of
// this class breaks `pad db migrate-to-pg` partway through the copy, leaving
// the destination half-populated, which is the failure the NUL preflight
// exists to prevent. No HTTP door stores one today (measured on BUG-3220's
// trail: JSON bodies decode through Go strings, multipart filenames are
// substituted), so this is a guard for rows an earlier release or a direct
// database edit left behind.
//
// NO SQL PRE-FILTER, unlike scanColumn: SQLite has no UTF-8 validity test, so
// every non-NULL value is read as a BLOB and checked in Go. That is one full
// read of each protected column, the same order of work as the copy it
// guards, which reads every row anyway.
//
// CAST AS BLOB, not a plain read: the bytes are what is judged, and a BLOB is
// returned exactly as stored.
func (s *Store) scanColumnUTF8(c nulColumn, addr tableAddressing) ([]NULViolation, error) {
	sel := make([]string, 0, len(addr.KeyColumns)+3)
	sel = append(sel, "rowid")
	for _, k := range addr.KeyColumns {
		sel = append(sel, quoteIdent(k))
	}
	if addr.HasWorkspace {
		sel = append(sel, `"workspace_id"`)
	}
	qc := quoteIdent(c.Column)
	sel = append(sel, "CAST("+qc+" AS BLOB)")

	q := fmt.Sprintf(`SELECT %s FROM %s WHERE %s IS NOT NULL`, strings.Join(sel, ", "), quoteIdent(c.Table), qc)
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("scan %s.%s for invalid UTF-8: %w", c.Table, c.Column, err)
	}
	defer rows.Close()

	var out []NULViolation
	for rows.Next() {
		// Nullable key and workspace columns, for the reason scanColumn
		// gives: SQLite permits NULL in some PRIMARY KEY columns.
		dest := make([]any, 0, len(sel))
		var rowid int64
		dest = append(dest, &rowid)
		keyVals := make([]sql.NullString, len(addr.KeyColumns))
		for i := range keyVals {
			dest = append(dest, &keyVals[i])
		}
		var wsID sql.NullString
		if addr.HasWorkspace {
			dest = append(dest, &wsID)
		}
		var value []byte
		dest = append(dest, &value)
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("scan %s.%s row for invalid UTF-8: %w", c.Table, c.Column, err)
		}
		if utf8.Valid(value) {
			continue
		}
		v := NULViolation{
			Table:       c.Table,
			Column:      c.Column,
			Key:         map[string]string{},
			WorkspaceID: wsID.String,
			InvalidUTF8: true,
			rowid:       rowid,
		}
		for i, k := range addr.KeyColumns {
			if !keyVals[i].Valid {
				v.KeyIncomplete = true
				continue
			}
			v.Key[k] = keyVals[i].String
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// mergeUTF8Violations folds one column's invalid-UTF-8 findings into its NUL
// findings, so a value carrying both defects is ONE violation with both kinds
// rather than two rows the census would count twice. Matched on the rowid
// both scans read, not on Key, which cannot identify a KeyIncomplete row
// (codex r1). Order: the NUL findings in their order, then the UTF-8-only
// ones in theirs.
func mergeUTF8Violations(nul, invalid []NULViolation) []NULViolation {
	if len(invalid) == 0 {
		return nul
	}
	index := make(map[int64]int, len(nul))
	for i, v := range nul {
		index[v.rowid] = i
	}
	out := append([]NULViolation{}, nul...)
	for _, v := range invalid {
		if i, ok := index[v.rowid]; ok {
			out[i].InvalidUTF8 = true
			continue
		}
		out = append(out, v)
	}
	return out
}
