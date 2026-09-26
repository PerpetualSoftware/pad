package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
)

// renameArrayOptionTx is the multi_select half of an option rename
// (BUG-3224). The scalar pass in applyFieldMigrationsTx matches rows whose
// value EQUALS the old option, and an array never equals a scalar, so a
// multi_select value kept the old option after every rename: an orphan the
// schema no longer lists and the editor cannot show as selected.
//
// It rewrites every string ELEMENT equal to oldVal to newVal, in place, and
// drops the renamed element when newVal is already in the array, so the
// rename cannot mint a duplicate a multi_select write would refuse. Other
// elements keep their bytes, numbers included (json.Number). It returns the
// ids it rewrote; the caller folds them into its one bulk event.
func (s *Store) renameArrayOptionTx(tx *sql.Tx, collectionID, workspaceID, field, oldVal, newVal, ts string) ([]string, error) {
	if !validFieldKey.MatchString(field) {
		return nil, fmt.Errorf("rename option: unsupported field key %q", field)
	}
	var selectSQL, updateSQL string
	if s.dialect.Driver() == DriverPostgres {
		selectSQL = fmt.Sprintf(`
			SELECT id, (fields->'%[1]s')::text FROM items
			WHERE collection_id = ? AND deleted_at IS NULL
			  AND jsonb_typeof(fields->'%[1]s') = 'array'
			  AND fields->'%[1]s' @> jsonb_build_array(CAST(? AS text))`, field)
		updateSQL = fmt.Sprintf(`jsonb_set(COALESCE(fields, '{}')::jsonb, '{%s}', CAST(? AS jsonb))`, field)
	} else {
		selectSQL = fmt.Sprintf(`
			SELECT id, json_extract(fields, '$.%[1]s') FROM items
			WHERE collection_id = ? AND deleted_at IS NULL
			  AND json_type(fields, '$.%[1]s') = 'array'
			  AND EXISTS (SELECT 1 FROM json_each(fields, '$.%[1]s') e WHERE e.type = 'text' AND e.value = ?)`, field)
		updateSQL = fmt.Sprintf(`json_set(fields, '$.%s', json(?))`, field)
	}

	rows, err := tx.Query(s.q(selectSQL), collectionID, oldVal)
	if err != nil {
		return nil, fmt.Errorf("rename option %s (%s → %s) list arrays: %w", field, oldVal, newVal, err)
	}
	type pending struct{ id, value string }
	var todo []pending
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return nil, fmt.Errorf("rename option %s scan: %w", field, err)
		}
		next, changed, err := renameArrayElements(raw, oldVal, newVal)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("rename option %s item %s: %w", field, id, err)
		}
		if changed {
			todo = append(todo, pending{id, next})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("rename option %s rows: %w", field, err)
	}
	rows.Close()

	stmt := s.q(fmt.Sprintf(`
		UPDATE items
		SET fields = %s,
		    updated_at = ?,
		    seq = `+nextWorkspaceSeqSubquery+`
		WHERE id = ?`, updateSQL))
	var touched []string
	for _, p := range todo {
		res, err := tx.Exec(stmt, p.value, ts, workspaceID, p.id)
		if err != nil {
			return nil, fmt.Errorf("rename option %s (%s → %s) row %s: %w", field, oldVal, newVal, p.id, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			touched = append(touched, p.id)
		}
	}
	return touched, nil
}

// renameArrayElements rewrites the string elements of a JSON array equal to
// oldVal to newVal, in place. A renamed element is dropped instead when newVal
// is already in the array (or an earlier element was renamed to it); a
// duplicate the array already held is left alone. changed is false when
// nothing was renamed.
func renameArrayElements(raw, oldVal, newVal string) (string, bool, error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.UseNumber()
	var elems []any
	if err := dec.Decode(&elems); err != nil {
		return "", false, err
	}
	haveNew := false
	for _, e := range elems {
		if str, ok := e.(string); ok && str == newVal {
			haveNew = true
		}
	}
	// Only a RENAMED element is ever dropped: a duplicate the array already
	// held is not this rename's to fix.
	out := make([]any, 0, len(elems))
	changed := false
	for _, e := range elems {
		if str, ok := e.(string); ok && str == oldVal {
			changed = true
			if haveNew {
				continue
			}
			e, haveNew = newVal, true
		}
		out = append(out, e)
	}
	if !changed {
		return raw, false, nil
	}
	// SetEscapeHTML(false): Marshal would rewrite <, > and & in the elements
	// this rename did not touch.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return "", false, err
	}
	return string(bytes.TrimRight(buf.Bytes(), "\n")), true, nil
}
