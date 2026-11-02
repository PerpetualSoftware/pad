package store

import (
	"database/sql"
	"fmt"
)

// itemRefCurrentMatchSQL proves current prefix ownership for item i and
// collection c. Workspace-unique numbers alone do not establish it (BUG-4119).
const itemRefCurrentMatchSQL = `LOWER(r.target_ref) = LOWER(c.prefix || '-' || CAST(i.item_number AS TEXT))`

// itemRefMatchesSQL also accepts durable move history. Former collections
// deliberately include deleted rows; prefix reuse must not invalidate a moved
// item's old ref. The CURRENT former prefix is used.
const itemRefMatchesSQL = `(` + itemRefCurrentMatchSQL + `
	OR EXISTS (
		SELECT 1 FROM item_collection_moves m
		JOIN collections old ON old.id = m.from_collection_id
		WHERE m.workspace_id = i.workspace_id AND m.item_id = i.id
		  AND old.workspace_id = i.workspace_id
		  AND LOWER(r.target_ref) = LOWER(old.prefix || '-' || CAST(i.item_number AS TEXT))
	)
)`

// itemRefLookupSQL builds the one shared SELECT for both ordinary Queryer
// callers and the app door's concrete *sql.Tx path. allowMoves=false omits
// move history entirely for lowercase identifiers and include-deleted lookup.
func itemRefLookupSQL(workspaceID, prefix string, number int, scope linkScope, includeDeleted, allowMoves bool) (string, []any) {
	restrict, scopeArgs := scope.clause("i")
	args := []any{fmt.Sprintf("%s-%d", prefix, number), workspaceID, number, includeDeleted}
	args = append(args, scopeArgs...)
	match := itemRefCurrentMatchSQL
	if allowMoves {
		match = itemRefMatchesSQL
	}
	return `
		SELECT i.id FROM items i
		JOIN collections c ON c.id = i.collection_id
		CROSS JOIN (SELECT ? AS target_ref) r
		WHERE i.workspace_id = ? AND i.item_number = ?
		  AND (? OR i.deleted_at IS NULL)
		  AND ` + match + restrict, args
}

// itemRefIDQ retains the caller's executor, scope and liveness policy.
// The app door executes itemRefLookupSQL through its concrete *sql.Tx instead.
func (s *Store) itemRefIDQ(q Queryer, workspaceID, prefix string, number int, scope linkScope, includeDeleted, allowMoves bool) (string, error) {
	query, args := itemRefLookupSQL(workspaceID, prefix, number, scope, includeDeleted, allowMoves)
	var id string
	err := q.QueryRow(s.q(query), args...).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve item ref: %w", err)
	}
	return id, nil
}
