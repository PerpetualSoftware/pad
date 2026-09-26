package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
)

// The multi_select half of an option rename (BUG-3224). The scalar pass in
// applyFieldMigrationsTx matches rows whose value EQUALS an old option, and an
// array never equals a scalar, so a multi_select value kept the old option
// after every rename: an orphan the schema no longer lists and the editor
// cannot show as selected.

// arrayRow is one array value found holding an old option, read before any
// row of the rename is written.
type arrayRow struct{ id, raw string }

// arrayRowsHoldingTx returns every live row of the collection whose field is
// an ARRAY holding at least one of olds as a string element, each once.
func (s *Store) arrayRowsHoldingTx(tx *sql.Tx, collectionID, field string, olds []string) ([]arrayRow, error) {
	if !validFieldKey.MatchString(field) {
		return nil, fmt.Errorf("rename option: unsupported field key %q", field)
	}
	var selectSQL string
	if s.dialect.Driver() == DriverPostgres {
		selectSQL = fmt.Sprintf(`
			SELECT id, (fields->'%[1]s')::text FROM items
			WHERE collection_id = ? AND deleted_at IS NULL
			  AND jsonb_typeof(fields->'%[1]s') = 'array'
			  AND fields->'%[1]s' @> jsonb_build_array(CAST(? AS text))`, field)
	} else {
		selectSQL = fmt.Sprintf(`
			SELECT id, json_extract(fields, '$.%[1]s') FROM items
			WHERE collection_id = ? AND deleted_at IS NULL
			  AND json_type(fields, '$.%[1]s') = 'array'
			  AND EXISTS (SELECT 1 FROM json_each(fields, '$.%[1]s') e WHERE e.type = 'text' AND e.value = ?)`, field)
	}
	seen := map[string]bool{}
	var out []arrayRow
	for _, oldVal := range olds {
		rows, err := tx.Query(s.q(selectSQL), collectionID, oldVal)
		if err != nil {
			return nil, fmt.Errorf("rename option %s (%s) list arrays: %w", field, oldVal, err)
		}
		for rows.Next() {
			var r arrayRow
			if err := rows.Scan(&r.id, &r.raw); err != nil {
				rows.Close()
				return nil, fmt.Errorf("rename option %s scan: %w", field, err)
			}
			if !seen[r.id] {
				seen[r.id] = true
				out = append(out, r)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("rename option %s rows: %w", field, err)
		}
		rows.Close()
	}
	return out, nil
}

// writeRenamedArraysTx rewrites each row's array under the whole rename map
// at once, bumping seq and updated_at like the scalar pass. It returns the ids
// it wrote and the old values it renamed somewhere, sorted.
func (s *Store) writeRenamedArraysTx(tx *sql.Tx, workspaceID, field string, rows []arrayRow, renames map[string]string, ts string) ([]string, []string, error) {
	if len(rows) == 0 {
		return nil, nil, nil
	}
	setSQL := fmt.Sprintf(`json_set(fields, '$.%s', json(?))`, field)
	if s.dialect.Driver() == DriverPostgres {
		setSQL = fmt.Sprintf(`jsonb_set(COALESCE(fields, '{}')::jsonb, '{%s}', CAST(? AS jsonb))`, field)
	}
	stmt := s.q(fmt.Sprintf(`
		UPDATE items
		SET fields = %s,
		    updated_at = ?,
		    seq = `+nextWorkspaceSeqSubquery+`
		WHERE id = ?`, setSQL))
	var touched []string
	renamedSet := map[string]bool{}
	for _, r := range rows {
		next, renamed, err := renameArrayElements(r.raw, renames)
		if err != nil {
			return nil, nil, fmt.Errorf("rename option %s item %s: %w", field, r.id, err)
		}
		if len(renamed) == 0 {
			continue
		}
		res, err := tx.Exec(stmt, next, ts, workspaceID, r.id)
		if err != nil {
			return nil, nil, fmt.Errorf("rename option %s row %s: %w", field, r.id, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			touched = append(touched, r.id)
			for _, o := range renamed {
				renamedSet[o] = true
			}
		}
	}
	olds := make([]string, 0, len(renamedSet))
	for o := range renamedSet {
		olds = append(olds, o)
	}
	sort.Strings(olds)
	return touched, olds, nil
}

// renameArrayElements maps each string element of a JSON array through
// renames ONCE (a→b, b→c turns ["a","b"] into ["b","c"]). A mapped element is
// dropped when its new value is already in the result, whether as an element
// the map does not touch or as an earlier mapped one; a duplicate the array
// already held among untouched elements is left alone. Other elements keep
// their value, numbers their literal (json.Number). renamed lists the old
// values that occurred; empty means the array is unchanged.
func renameArrayElements(raw string, renames map[string]string) (string, []string, error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.UseNumber()
	var elems []any
	if err := dec.Decode(&elems); err != nil {
		return "", nil, err
	}
	present := map[string]bool{}
	for _, e := range elems {
		if str, ok := e.(string); ok {
			// An identity entry (b→b) is not a rename: b stays, so it is
			// present for the dedupe exactly as an unmapped element is.
			if newVal, mapped := renames[str]; !mapped || newVal == str {
				present[str] = true
			}
		}
	}
	out := make([]any, 0, len(elems))
	renamedSet := map[string]bool{}
	for _, e := range elems {
		if str, ok := e.(string); ok {
			if newVal, mapped := renames[str]; mapped && newVal != str {
				renamedSet[str] = true
				if present[newVal] {
					continue
				}
				present[newVal] = true
				out = append(out, newVal)
				continue
			}
		}
		out = append(out, e)
	}
	if len(renamedSet) == 0 {
		return raw, nil, nil
	}
	// SetEscapeHTML(false): Marshal would rewrite <, > and & in the elements
	// this rename did not touch.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return "", nil, err
	}
	renamed := make([]string, 0, len(renamedSet))
	for o := range renamedSet {
		renamed = append(renamed, o)
	}
	sort.Strings(renamed)
	return string(bytes.TrimRight(buf.Bytes(), "\n")), renamed, nil
}
