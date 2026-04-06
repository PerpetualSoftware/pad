package store

import (
	"fmt"
	"strings"
)

// DriverType identifies the database backend.
type DriverType string

const (
	DriverSQLite   DriverType = "sqlite"
	DriverPostgres DriverType = "postgres"
)

// Dialect encapsulates SQL syntax differences between database backends.
// The Store calls dialect methods to generate backend-specific SQL fragments.
type Dialect interface {
	// Driver returns the driver type.
	Driver() DriverType

	// Placeholder returns the nth parameter placeholder (1-indexed).
	// SQLite: "?", PostgreSQL: "$1", "$2", etc.
	Placeholder(n int) string

	// Rebind converts a query with "?" placeholders to the dialect's format.
	// For SQLite this is a no-op. For PostgreSQL, "?" becomes "$1", "$2", etc.
	Rebind(query string) string

	// JSONExtractText returns SQL to extract a text value from a JSON column.
	// SQLite: json_extract(col, '$.key')
	// PostgreSQL: col->>'key'
	JSONExtractText(column, key string) string

	// JSONExtractPath returns SQL to extract a value at a dotted path from a JSON column.
	// SQLite: json_extract(col, '$.path.to.key')
	// PostgreSQL: col #>> '{path,to,key}'
	JSONExtractPath(column, path string) string

	// JSONSet returns SQL to set a value at a path in a JSON column.
	// SQLite: json_set(col, '$.key', ?)
	// PostgreSQL: jsonb_set(col::jsonb, '{key}', ?::jsonb)
	// Returns the SQL fragment and any extra placeholders used.
	JSONSet(column, key string) string

	// JSONRemove returns SQL to remove a key from a JSON column.
	// SQLite: json_remove(col, '$.key')
	// PostgreSQL: col::jsonb - 'key'
	JSONRemove(column, key string) string

	// Now returns the SQL expression for the current UTC timestamp.
	// SQLite: datetime('now')
	// PostgreSQL: NOW() AT TIME ZONE 'UTC'
	Now() string

	// NowRFC3339 returns the SQL expression for current UTC time in RFC3339 format.
	// SQLite: strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
	// PostgreSQL: TO_CHAR(NOW() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
	NowRFC3339() string

	// GroupConcat returns SQL for string aggregation with a separator.
	// SQLite: GROUP_CONCAT(DISTINCT expr)
	// PostgreSQL: STRING_AGG(DISTINCT expr, ',')
	GroupConcat(expr string, distinct bool) string

	// BoolToInt converts a Go bool to a query parameter value.
	// SQLite: 0/1 (integers)
	// PostgreSQL: true/false (native booleans)
	BoolToInt(b bool) interface{}

	// ILike returns the case-insensitive LIKE operator.
	// SQLite: LIKE (case-insensitive by default)
	// PostgreSQL: ILIKE
	ILike() string

	// Concat returns SQL to concatenate string expressions.
	// SQLite: expr1 || expr2
	// PostgreSQL: expr1 || expr2 (same, but useful as abstraction point)
	Concat(exprs ...string) string

	// FTSMatch returns the full-text search WHERE clause fragment.
	// SQLite: "table MATCH ?"
	// PostgreSQL: "table.tsvector_col @@ plainto_tsquery('english', ?)"
	FTSMatch(table, column string) string

	// FTSSnippet returns SQL for highlighted search result snippets.
	// SQLite: snippet(fts_table, col_idx, '<mark>', '</mark>', '...', 32)
	// PostgreSQL: ts_headline('english', col, plainto_tsquery('english', ?))
	FTSSnippet(ftsTable string, colIndex int, sourceColumn string) string

	// FTSRank returns the column/expression for full-text relevance ranking.
	// SQLite: rank (built-in FTS5 column)
	// PostgreSQL: ts_rank(tsvector_col, plainto_tsquery('english', ?))
	FTSRank(table, column string) string

	// JSONArrayContains returns SQL + the arg to check if a JSON array column
	// contains a given text value.
	// SQLite: "column LIKE ?" with arg `%"value"%`
	// PostgreSQL: "column::jsonb @> ?::jsonb" with arg `["value"]`
	JSONArrayContains(column, value string) (string, interface{})
}

// ---------- SQLite dialect ----------

type sqliteDialect struct{}

func (d *sqliteDialect) Driver() DriverType { return DriverSQLite }

func (d *sqliteDialect) Placeholder(_ int) string { return "?" }

func (d *sqliteDialect) Rebind(query string) string { return query }

func (d *sqliteDialect) JSONExtractText(column, key string) string {
	return fmt.Sprintf("json_extract(%s, '$.%s')", column, key)
}

func (d *sqliteDialect) JSONExtractPath(column, path string) string {
	return fmt.Sprintf("json_extract(%s, '$.%s')", column, path)
}

func (d *sqliteDialect) JSONSet(column, key string) string {
	return fmt.Sprintf("json_set(%s, '$.%s', ?)", column, key)
}

func (d *sqliteDialect) JSONRemove(column, key string) string {
	return fmt.Sprintf("json_remove(%s, '$.%s')", column, key)
}

func (d *sqliteDialect) Now() string {
	return "datetime('now')"
}

func (d *sqliteDialect) NowRFC3339() string {
	return "strftime('%Y-%m-%dT%H:%M:%SZ', 'now')"
}

func (d *sqliteDialect) GroupConcat(expr string, distinct bool) string {
	if distinct {
		return fmt.Sprintf("GROUP_CONCAT(DISTINCT %s)", expr)
	}
	return fmt.Sprintf("GROUP_CONCAT(%s)", expr)
}

func (d *sqliteDialect) BoolToInt(b bool) interface{} {
	if b {
		return 1
	}
	return 0
}

func (d *sqliteDialect) ILike() string { return "LIKE" }

func (d *sqliteDialect) Concat(exprs ...string) string {
	return strings.Join(exprs, " || ")
}

func (d *sqliteDialect) FTSMatch(table, _ string) string {
	return fmt.Sprintf("%s MATCH ?", table)
}

func (d *sqliteDialect) FTSSnippet(ftsTable string, colIndex int, _ string) string {
	return fmt.Sprintf("snippet(%s, %d, '<mark>', '</mark>', '...', 32)", ftsTable, colIndex)
}

func (d *sqliteDialect) FTSRank(_, _ string) string {
	return "rank"
}

func (d *sqliteDialect) JSONArrayContains(column, value string) (string, interface{}) {
	return column + " LIKE ?", "%\"" + value + "\"%"
}

// ---------- PostgreSQL dialect ----------

type postgresDialect struct{}

func (d *postgresDialect) Driver() DriverType { return DriverPostgres }

func (d *postgresDialect) Placeholder(n int) string {
	return fmt.Sprintf("$%d", n)
}

func (d *postgresDialect) Rebind(query string) string {
	return rebindQuery(query)
}

func (d *postgresDialect) JSONExtractText(column, key string) string {
	return fmt.Sprintf("%s->>'%s'", column, key)
}

func (d *postgresDialect) JSONExtractPath(column, path string) string {
	parts := strings.Split(path, ".")
	return fmt.Sprintf("%s #>> '{%s}'", column, strings.Join(parts, ","))
}

func (d *postgresDialect) JSONSet(column, key string) string {
	return fmt.Sprintf("jsonb_set(COALESCE(%s, '{}')::jsonb, '{%s}', to_jsonb(?::text))", column, key)
}

func (d *postgresDialect) JSONRemove(column, key string) string {
	return fmt.Sprintf("(%s::jsonb - '%s')", column, key)
}

func (d *postgresDialect) Now() string {
	return "(NOW() AT TIME ZONE 'UTC')"
}

func (d *postgresDialect) NowRFC3339() string {
	return "TO_CHAR(NOW() AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"')"
}

func (d *postgresDialect) GroupConcat(expr string, distinct bool) string {
	if distinct {
		return fmt.Sprintf("STRING_AGG(DISTINCT %s, ',')", expr)
	}
	return fmt.Sprintf("STRING_AGG(%s, ',')", expr)
}

func (d *postgresDialect) BoolToInt(b bool) interface{} {
	return b
}

func (d *postgresDialect) ILike() string { return "ILIKE" }

func (d *postgresDialect) Concat(exprs ...string) string {
	return strings.Join(exprs, " || ")
}

func (d *postgresDialect) FTSMatch(table, column string) string {
	return fmt.Sprintf("%s.%s @@ plainto_tsquery('english', ?)", table, column)
}

func (d *postgresDialect) FTSSnippet(_ string, _ int, sourceColumn string) string {
	return fmt.Sprintf("ts_headline('english', %s, plainto_tsquery('english', ?), 'StartSel=<mark>,StopSel=</mark>,MaxFragments=1,MaxWords=32')", sourceColumn)
}

func (d *postgresDialect) FTSRank(table, column string) string {
	return fmt.Sprintf("ts_rank(%s.%s, plainto_tsquery('english', ?))", table, column)
}

func (d *postgresDialect) JSONArrayContains(column, value string) (string, interface{}) {
	return column + "::jsonb @> ?::jsonb", `["` + value + `"]`
}

// ---------- Helper ----------

// rebindQuery converts "?" placeholders to PostgreSQL's "$1", "$2", etc.
// Respects string literals (single quotes) and does not modify "?" inside them.
func rebindQuery(query string) string {
	var buf strings.Builder
	buf.Grow(len(query) + 16)
	n := 0
	inString := false
	for i := 0; i < len(query); i++ {
		ch := query[i]
		if ch == '\'' {
			inString = !inString
			buf.WriteByte(ch)
		} else if ch == '?' && !inString {
			n++
			fmt.Fprintf(&buf, "$%d", n)
		} else {
			buf.WriteByte(ch)
		}
	}
	return buf.String()
}
