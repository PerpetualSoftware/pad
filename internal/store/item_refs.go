package store

import (
	"database/sql"
	"fmt"
)

// itemRefMatchesSQL is the shared identity rule for item i, its current
// collection c, and the requested spelling r.target_ref (BUG-4119). Numbers
// are workspace-unique, but that alone never establishes prefix ownership.
// Former collections deliberately include deleted rows; prefix reuse must
// not invalidate a moved item's old ref. The CURRENT former prefix is used.
const itemRefMatchesSQL = `(
	LOWER(r.target_ref) = LOWER(c.prefix || '-' || CAST(i.item_number AS TEXT))
	OR EXISTS (
		SELECT 1 FROM item_collection_moves m
		JOIN collections old ON old.id = m.from_collection_id
		WHERE m.workspace_id = i.workspace_id AND m.item_id = i.id
		  AND old.workspace_id = i.workspace_id
		  AND LOWER(r.target_ref) = LOWER(old.prefix || '-' || CAST(i.item_number AS TEXT))
	)
)`

// itemRefIDQ resolves exact and proven moved refs in one statement, using the
// caller's executor so transaction-local moves and scope are respected. The
// caller retains its liveness policy: backlinks may name archived targets;
// ordinary resolution, relations and wiki links require live items.
func (s *Store) itemRefIDQ(q Queryer, workspaceID, prefix string, number int, scope linkScope, includeDeleted bool) (string, error) {
	restrict, scopeArgs := scope.clause("i")
	args := []any{fmt.Sprintf("%s-%d", prefix, number), workspaceID, number, includeDeleted}
	args = append(args, scopeArgs...)
	var id string
	err := q.QueryRow(s.q(`
		SELECT i.id FROM items i
		JOIN collections c ON c.id = i.collection_id
		CROSS JOIN (SELECT ? AS target_ref) r
		WHERE i.workspace_id = ? AND i.item_number = ?
		  AND (? OR i.deleted_at IS NULL)
		  AND `+itemRefMatchesSQL+restrict), args...).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve item ref: %w", err)
	}
	return id, nil
}
