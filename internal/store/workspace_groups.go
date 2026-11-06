package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
)

const MaxWorkspaceGroups = 20
const MaxWorkspaceGroupMembers = 100

var (
	ErrWorkspaceGroupLimit            = errors.New("workspace group limit reached")
	ErrWorkspaceGroupNameTaken        = errors.New("workspace group name is already taken")
	ErrWorkspaceGroupMemberLimit      = errors.New("workspace group member limit reached")
	ErrWorkspaceGroupUnknownWorkspace = errors.New("workspace group member is not available")
)

// workspaceGroupsQ reads owner-scoped rows in one statement. Admission is
// deliberately applied by the caller on every response, not stored here.
func (s *Store) workspaceGroupsQ(q Queryer, userID string, id *string) ([]models.WorkspaceGroup, error) {
	where := `g.user_id = ?`
	args := []any{userID}
	if id != nil {
		where = `g.id = ? AND g.user_id = ?`
		args = []any{*id, userID}
	}
	rows, err := q.Query(s.q(`SELECT g.id, g.name, g.created_at, g.updated_at, m.workspace_id
 FROM user_workspace_groups g
 LEFT JOIN user_workspace_group_members m ON m.group_id = g.id
 WHERE `+where+`
 ORDER BY g.id, m.workspace_id`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]models.WorkspaceGroup, 0)
	for rows.Next() {
		var g models.WorkspaceGroup
		var member sql.NullString
		if err := rows.Scan(&g.ID, &g.Name, &g.CreatedAt, &g.UpdatedAt, &member); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].ID != g.ID {
			g.WorkspaceIDs = []string{}
			out = append(out, g)
		}
		if member.Valid {
			out[len(out)-1].WorkspaceIDs = append(out[len(out)-1].WorkspaceIDs, member.String)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Ordered here, not in SQL: lower() folds ASCII only on SQLite and by locale
	// on Postgres, so the same account would list in two orders.
	sort.SliceStable(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if a != b {
			return a < b
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (s *Store) ListWorkspaceGroups(ctx context.Context, userID string) ([]models.WorkspaceGroup, error) {
	return s.workspaceGroupsQ(s.QContext(ctx), userID, nil)
}
func (s *Store) workspaceGroupQ(q Queryer, userID, id string) (*models.WorkspaceGroup, error) {
	groups, err := s.workspaceGroupsQ(q, userID, &id)
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, sql.ErrNoRows
	}
	return &groups[0], nil
}
func (s *Store) GetWorkspaceGroup(ctx context.Context, userID, id string) (*models.WorkspaceGroup, error) {
	return s.workspaceGroupQ(s.QContext(ctx), userID, id)
}

// beginWorkspaceGroupWrite serializes writes for one user before touching
// group rows. SQLite uses BEGIN IMMEDIATE; Postgres takes the strongest
// required user lock first, so cap and name checks cannot race.
func (s *Store) beginWorkspaceGroupWrite(ctx context.Context, userID string) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	query := `SELECT id FROM users WHERE id = ?`
	if s.dialect.Driver() == DriverPostgres {
		query += ` FOR NO KEY UPDATE`
	}
	var id string
	if err := tx.QueryRowContext(ctx, s.q(query), userID).Scan(&id); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func (s *Store) workspaceGroupNameAvailable(ctx context.Context, tx *sql.Tx, userID, id, name string) error {
	// EqualFold also handles Unicode names on SQLite, whose lower() folds ASCII.
	rows, err := tx.QueryContext(ctx, s.q(`SELECT id, name FROM user_workspace_groups WHERE user_id = ?`), userID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var otherID, otherName string
		if err := rows.Scan(&otherID, &otherName); err != nil {
			return err
		}
		if otherID != id && strings.EqualFold(otherName, name) {
			return ErrWorkspaceGroupNameTaken
		}
	}
	return rows.Err()
}

// replaceWorkspaceGroupMembers makes ids the group's members. keep names stored
// members the caller cannot see right now: they are left in place, so an edit
// made while access to a workspace was lost does not erase it for good.
func (s *Store) replaceWorkspaceGroupMembers(ctx context.Context, tx *sql.Tx, userID, id string, ids, keep []string) error {
	unique := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, wsID := range ids {
		if !seen[wsID] {
			seen[wsID] = true
			unique = append(unique, wsID)
		}
	}
	kept := make([]string, 0, len(keep))
	for _, wsID := range keep {
		if !seen[wsID] {
			seen[wsID] = true
			kept = append(kept, wsID)
		}
	}
	// The cap counts only the ids the caller sent. A kept member the caller
	// cannot see must not take a slot: it would make a 100-id edit refuse at
	// 99 and so confirm that a hidden member exists.
	if len(unique) > MaxWorkspaceGroupMembers {
		return ErrWorkspaceGroupMemberLimit
	}
	// Lock live workspaces in id order before member writes. This also
	// prevents a concurrent delete from racing the foreign-key inserts.
	sort.Strings(unique)
	for _, wsID := range unique {
		query := `SELECT id FROM workspaces WHERE id = ? AND deleted_at IS NULL`
		if s.dialect.Driver() == DriverPostgres {
			query += ` FOR SHARE`
		}
		var found string
		if err := tx.QueryRowContext(ctx, s.q(query), wsID).Scan(&found); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrWorkspaceGroupUnknownWorkspace
			}
			return err
		}
	}
	// The owner predicate remains explicit even though the transaction has
	// already resolved the group under that owner.
	deleteSQL := `DELETE FROM user_workspace_group_members
 WHERE group_id IN (SELECT id FROM user_workspace_groups WHERE id = ? AND user_id = ?)`
	deleteArgs := []any{id, userID}
	if len(kept) > 0 {
		deleteSQL += ` AND workspace_id NOT IN (` + placeholders(len(kept)) + `)`
		for _, wsID := range kept {
			deleteArgs = append(deleteArgs, wsID)
		}
	}
	if _, err := tx.ExecContext(ctx, s.q(deleteSQL), deleteArgs...); err != nil {
		return err
	}
	for _, wsID := range unique {
		result, err := tx.ExecContext(ctx, s.q(`INSERT INTO user_workspace_group_members (group_id, workspace_id)
   SELECT g.id, w.id FROM user_workspace_groups g CROSS JOIN workspaces w
   WHERE g.id = ? AND g.user_id = ? AND w.id = ? AND w.deleted_at IS NULL`), id, userID, wsID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrWorkspaceGroupUnknownWorkspace
		}
	}
	return nil
}

func (s *Store) CreateWorkspaceGroup(ctx context.Context, userID, name string, ids []string) (*models.WorkspaceGroup, error) {
	tx, err := s.beginWorkspaceGroupWrite(ctx, userID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM user_workspace_groups WHERE user_id = ?`), userID).Scan(&count); err != nil {
		return nil, err
	}
	if count >= MaxWorkspaceGroups {
		return nil, ErrWorkspaceGroupLimit
	}
	if err := s.workspaceGroupNameAvailable(ctx, tx, userID, "", name); err != nil {
		return nil, err
	}
	id, stamp := newID(), nowNano()
	if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO user_workspace_groups (id,user_id,name,created_at,updated_at) VALUES (?,?,?,?,?)`), id, userID, name, stamp, stamp); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrWorkspaceGroupNameTaken
		}
		return nil, err
	}
	if err := s.replaceWorkspaceGroupMembers(ctx, tx, userID, id, ids, nil); err != nil {
		return nil, err
	}
	g, err := s.workspaceGroupQ(WithContext(ctx, tx), userID, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return g, nil
}

// UpdateWorkspaceGroup renames the group and/or replaces its members. visible
// is the set of workspaces the caller can see; when ids replaces the list, the
// stored members outside visible stay. They are read here, inside the
// transaction, so a concurrent edit is never undone from a stale snapshot. A
// nil visible keeps nothing.
func (s *Store) UpdateWorkspaceGroup(ctx context.Context, userID, id string, name *string, ids *[]string, visible map[string]bool) (*models.WorkspaceGroup, error) {
	tx, err := s.beginWorkspaceGroupWrite(ctx, userID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	g, err := s.workspaceGroupQ(WithContext(ctx, tx), userID, id)
	if err != nil {
		return nil, err
	}
	if name != nil {
		if err := s.workspaceGroupNameAvailable(ctx, tx, userID, id, *name); err != nil {
			return nil, err
		}
		g.Name = *name
	}
	if name != nil || ids != nil {
		if _, err := tx.ExecContext(ctx, s.q(`UPDATE user_workspace_groups SET name = ?, updated_at = ? WHERE id = ? AND user_id = ?`), g.Name, nowNano(), id, userID); err != nil {
			if isUniqueViolation(err) {
				return nil, ErrWorkspaceGroupNameTaken
			}
			return nil, err
		}
	}
	if ids != nil {
		var keep []string
		if visible != nil {
			for _, wsID := range g.WorkspaceIDs {
				if !visible[wsID] {
					keep = append(keep, wsID)
				}
			}
		}
		if err := s.replaceWorkspaceGroupMembers(ctx, tx, userID, id, *ids, keep); err != nil {
			return nil, err
		}
	}
	g, err = s.workspaceGroupQ(WithContext(ctx, tx), userID, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return g, nil
}

func (s *Store) DeleteWorkspaceGroup(ctx context.Context, userID, id string) error {
	tx, err := s.beginWorkspaceGroupWrite(ctx, userID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, s.q(`DELETE FROM user_workspace_groups WHERE id = ? AND user_id = ?`), id, userID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}
