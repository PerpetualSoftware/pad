package store

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
)

// Field-value reads that answer the same on both dialects (BUG-3221).
//
// JSONExtractText is json_extract on SQLite, which yields the NATIVE type
// (true is INTEGER 1, 1e3 is REAL 1000.0, an array is compact JSON text), and
// ->> on Postgres, which yields jsonb's TEXT form ('true', '1000', an array
// with spaces). Compared to a caller's string, sorted, grouped or displayed,
// the same row therefore answered differently by dialect. The helpers here
// compare and render a field value by its JSON TYPE instead:
//
//   - string: itself.
//   - boolean: 'true' / 'false', never '1' / '0'.
//   - number: equal to an argument that parses as a JSON number, NUMERICALLY
//     (1000 = 1000.0 = 1e3); rendered with integer-valued numbers as integer
//     digits.
//   - array, object, null, missing: never equal to a scalar argument, and
//     rendered as NULL. No membership: that would be a multi_select feature,
//     not a dialect fix.
//
// Residual, pinned by TestFieldValueDialectResidual: a NON-integer number is
// rendered with each dialect's own number text, so one whose SQLite REAL form
// uses an exponent or needs more than 15 significant digits (1.5e-7) renders
// differently. Equality is numeric and does not depend on the rendering.

// jsonNumberRe is the JSON number grammar. An argument outside it never
// equals a stored number, so SQLite's CAST('abc' AS REAL) = 0 cannot fire.
var jsonNumberRe = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

// numericArg returns the SQLite comparison value for a numeric argument: an
// int64 when the number is integer-valued and in range, so a stored INTEGER is
// compared exactly, else a float64. ok is false for a non-number.
func numericArg(arg string) (any, bool) {
	if !jsonNumberRe.MatchString(arg) {
		return nil, false
	}
	if n, err := strconv.ParseInt(arg, 10, 64); err == nil {
		return n, true
	}
	f, err := strconv.ParseFloat(arg, 64)
	if err != nil || math.IsInf(f, 0) {
		return nil, false
	}
	if f == math.Trunc(f) && f >= -9.2e18 && f <= 9.2e18 {
		return int64(f), true
	}
	return f, true
}

// ---------- SQLite ----------

func (d *sqliteDialect) jsonType(column, key string) string {
	return fmt.Sprintf("json_type(%s, '$.%s')", column, key)
}

func (d *sqliteDialect) JSONFieldText(column, key string) string {
	v := d.JSONExtractText(column, key)
	return fmt.Sprintf("(CASE %s"+
		" WHEN 'text' THEN %s"+
		" WHEN 'true' THEN 'true' WHEN 'false' THEN 'false'"+
		" WHEN 'integer' THEN CAST(%s AS TEXT)"+
		" WHEN 'real' THEN CASE WHEN %s = CAST(%s AS INTEGER) THEN CAST(CAST(%s AS INTEGER) AS TEXT) ELSE CAST(%s AS TEXT) END"+
		" END)", d.jsonType(column, key), v, v, v, v, v, v)
}

func (d *sqliteDialect) JSONFieldEquals(column, key, arg string) (string, []any) {
	t := d.jsonType(column, key)
	v := d.JSONExtractText(column, key)
	clauses := fmt.Sprintf("(%s = 'text' AND %s = ?)", t, v)
	args := []any{arg}
	switch arg {
	case "true", "false":
		clauses += fmt.Sprintf(" OR %s = '%s'", t, arg)
	}
	if n, ok := numericArg(arg); ok {
		clauses += fmt.Sprintf(" OR (%s IN ('integer','real') AND %s = ?)", t, v)
		args = append(args, n)
	}
	return "(" + clauses + ")", args
}

func (d *sqliteDialect) JSONFieldOrder(column, key, dir string) string {
	t := d.jsonType(column, key)
	rank := fmt.Sprintf("(CASE COALESCE(%s, 'null')"+
		" WHEN 'integer' THEN 0 WHEN 'real' THEN 0"+
		" WHEN 'text' THEN 1 WHEN 'true' THEN 2 WHEN 'false' THEN 2"+
		" WHEN 'null' THEN 4 ELSE 3 END)", t)
	num := fmt.Sprintf("(CASE WHEN %s IN ('integer','real') THEN %s END)", t, d.JSONExtractText(column, key))
	return fmt.Sprintf("%s ASC, %s %s, %s %s", rank, num, dir, d.JSONFieldText(column, key), dir)
}

// ---------- PostgreSQL ----------

func (d *postgresDialect) JSONFieldText(column, key string) string {
	return fmt.Sprintf("(CASE jsonb_typeof(%s->'%s')"+
		" WHEN 'string' THEN %s->>'%s'"+
		" WHEN 'boolean' THEN %s->>'%s'"+
		" WHEN 'number' THEN trim_scale((%s->'%s')::numeric)::text"+
		" END)", column, key, column, key, column, key, column, key)
}

func (d *postgresDialect) JSONFieldEquals(column, key, arg string) (string, []any) {
	t := fmt.Sprintf("jsonb_typeof(%s->'%s')", column, key)
	v := d.JSONExtractText(column, key)
	clauses := fmt.Sprintf("(%s = 'string' AND %s = ?)", t, v)
	args := []any{arg}
	switch arg {
	case "true", "false":
		clauses += fmt.Sprintf(" OR (%s = 'boolean' AND %s = '%s')", t, v, arg)
	}
	if _, ok := numericArg(arg); ok {
		// The argument goes over as text and is cast by Postgres, so the
		// comparison is exact numeric, not float.
		clauses += fmt.Sprintf(" OR (%s = 'number' AND (%s->'%s')::numeric = CAST(? AS numeric))", t, column, key)
		args = append(args, arg)
	}
	return "(" + clauses + ")", args
}

func (d *postgresDialect) JSONFieldOrder(column, key, dir string) string {
	t := fmt.Sprintf("jsonb_typeof(%s->'%s')", column, key)
	rank := fmt.Sprintf("(CASE COALESCE(%s, 'null')"+
		" WHEN 'number' THEN 0 WHEN 'string' THEN 1 WHEN 'boolean' THEN 2"+
		" WHEN 'null' THEN 4 ELSE 3 END)", t)
	num := fmt.Sprintf("(CASE WHEN %s = 'number' THEN (%s->'%s')::numeric END)", t, column, key)
	return fmt.Sprintf("%s ASC, %s %s, %s %s", rank, num, dir, d.JSONFieldText(column, key), dir)
}
