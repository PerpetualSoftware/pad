package store

import (
	"database/sql"
	"fmt"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The per-user open set of workspace tabs (PLAN-3002 U1 / TASK-3256). The
// design is on PLAN-3002's trail, §2 and §3.
//
// These methods store ROWS; they do not decide visibility. A row may name a
// workspace the user can no longer see (a loss path nobody wired, a grant
// that lapsed with its collection), so the caller must intersect what it reads
// with the user's visible set before serving it: the invariant "a tab never
// names a workspace the caller cannot open" is enforced on read, by
// VisibleWorkspaceTabs. The deletes on the known loss paths are hygiene.
//
// Every write locks the user's row first (lockUserTabsTx), so two writes for
// one user serialise. That is what keeps "at most one ephemeral tab per user"
// true under concurrent opens without a partial unique index, which the two
// dialects spell differently.
//
// THE REVISION (BUG-3285). Serialising the writes does not order their
// RESPONSES: two requests from one user are processed in some order, and the
// client cannot tell from the send order which that was. So every write also
// bumps users.workspace_tabs_revision under the same lock, and every list
// carries the revision it was read with, read in ONE statement with the rows
// (listWorkspaceTabsQ), so a list and its revision are one instant on both
// dialects. A write reads its list inside its own transaction, after the
// bump: the answer is the set as that write left it. A list with a higher
// revision was processed later; equal revisions hold the same rows.
//
// The loss paths that delete rows bump too, and bump BEFORE they delete, so
// on Postgres they take the users row before the tab rows, the order every
// tab write takes them in.

// WorkspaceTabRow is a stored user_workspace_tabs row.
type WorkspaceTabRow struct {
	WorkspaceID string
	Position    int
	Ephemeral   bool
	LastRoute   string
	CreatedAt   string
	UpdatedAt   string
}

// WorkspaceTabList is a user's stored tab rows in bar order, with the
// revision they were read at.
type WorkspaceTabList struct {
	Revision int64
	Rows     []WorkspaceTabRow
}

// ListWorkspaceTabs returns the user's stored tab rows in bar order and their
// revision. It does not filter by visibility; see the note above.
func (s *Store) ListWorkspaceTabs(userID string) (WorkspaceTabList, error) {
	return listWorkspaceTabsQ(s, s.db, userID)
}

// listWorkspaceTabsQ reads the revision and the rows in ONE statement, so
// they describe one instant even on Postgres READ COMMITTED, where two
// statements may see two different commits. A user with no tabs yields one
// row of NULL tab columns; an unknown user yields nothing and revision 0.
func listWorkspaceTabsQ(s *Store, q Queryer, userID string) (WorkspaceTabList, error) {
	rows, err := q.Query(s.q(`
		SELECT u.workspace_tabs_revision, t.workspace_id, t.position, t.ephemeral,
		       COALESCE(t.last_route, ''), t.created_at, t.updated_at
		FROM users u
		LEFT JOIN user_workspace_tabs t ON t.user_id = u.id
		WHERE u.id = ?
		ORDER BY t.position ASC, t.created_at ASC, t.workspace_id ASC
	`), userID)
	if err != nil {
		return WorkspaceTabList{}, fmt.Errorf("list workspace tabs: %w", err)
	}
	defer rows.Close()
	var out WorkspaceTabList
	for rows.Next() {
		var (
			wsID               sql.NullString
			position           sql.NullInt64
			ephemeral          sql.NullBool
			lastRoute          sql.NullString
			createdAt, updated sql.NullString
		)
		if err := rows.Scan(&out.Revision, &wsID, &position, &ephemeral, &lastRoute, &createdAt, &updated); err != nil {
			return WorkspaceTabList{}, fmt.Errorf("scan workspace tab: %w", err)
		}
		if !wsID.Valid {
			continue // the LEFT JOIN's no-tabs row
		}
		out.Rows = append(out.Rows, WorkspaceTabRow{
			WorkspaceID: wsID.String,
			Position:    int(position.Int64),
			Ephemeral:   ephemeral.Bool,
			LastRoute:   lastRoute.String,
			CreatedAt:   createdAt.String,
			UpdatedAt:   updated.String,
		})
	}
	if err := rows.Err(); err != nil {
		return WorkspaceTabList{}, fmt.Errorf("list workspace tabs: %w", err)
	}
	return out, nil
}

// VisibleWorkspaceTabs intersects stored rows with the caller's visible
// workspace set, in row order. visible must be exactly what the caller may
// list (GetUserWorkspaces, after any token allow-list filter); a row whose
// workspace is not in it is dropped, never served.
func VisibleWorkspaceTabs(rows []WorkspaceTabRow, visible []models.Workspace) []models.WorkspaceTab {
	byID := make(map[string]*models.Workspace, len(visible))
	for i := range visible {
		byID[visible[i].ID] = &visible[i]
	}
	out := make([]models.WorkspaceTab, 0, len(rows))
	for _, r := range rows {
		ws, ok := byID[r.WorkspaceID]
		if !ok {
			continue
		}
		out = append(out, models.WorkspaceTab{
			WorkspaceID:   r.WorkspaceID,
			Slug:          ws.Slug,
			Name:          ws.Name,
			OwnerUsername: ws.OwnerUsername,
			IsGuest:       ws.IsGuest,
			Position:      r.Position,
			Ephemeral:     r.Ephemeral,
			LastRoute:     r.LastRoute,
			CreatedAt:     parseTime(r.CreatedAt),
			UpdatedAt:     parseTime(r.UpdatedAt),
		})
	}
	return out
}

// lockUserTabsTx takes the per-user write lock every tab write holds. On
// Postgres it is the users-row lock enforceUserLimitTx takes (FOR NO KEY
// UPDATE, which does not stall the FK checks of inserts referencing the
// user). On SQLite the transaction is already BEGIN IMMEDIATE, which
// serialises every writer; the read still proves the user exists.
func (s *Store) lockUserTabsTx(tx *sql.Tx, userID string) error {
	lock := `SELECT id FROM users WHERE id = ?`
	if s.dialect.Driver() == DriverPostgres {
		lock += ` FOR NO KEY UPDATE`
	}
	var id string
	if err := tx.QueryRow(s.q(lock), userID).Scan(&id); err != nil {
		return fmt.Errorf("workspace tabs: lock user %s: %w", userID, err)
	}
	return nil
}

// commitUserTabsTx finishes a tab write that holds lockUserTabsTx: it bumps
// the user's revision, reads the list as this write leaves it, and commits.
// Every tab write ends here, including one that changed nothing, so its
// answer is ordered after every write processed before it.
func (s *Store) commitUserTabsTx(tx *sql.Tx, userID string) (WorkspaceTabList, error) {
	if _, err := tx.Exec(s.q(`
		UPDATE users SET workspace_tabs_revision = workspace_tabs_revision + 1 WHERE id = ?
	`), userID); err != nil {
		return WorkspaceTabList{}, fmt.Errorf("workspace tabs: bump revision: %w", err)
	}
	list, err := listWorkspaceTabsQ(s, tx, userID)
	if err != nil {
		return WorkspaceTabList{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkspaceTabList{}, fmt.Errorf("workspace tabs: commit: %w", err)
	}
	return list, nil
}

// OpenWorkspaceTab adds a workspace to the user's open set.
//
//   - Already open: a durable open pins an ephemeral tab (PLAN-3002 Q9); an
//     ephemeral open of a tab that is already there changes nothing, and never
//     demotes a durable tab.
//   - Not open, durable: appended at the end.
//   - Not open, ephemeral: it REPLACES the user's current ephemeral tab, in
//     that tab's position, so there is at most one. With no ephemeral tab it
//     is appended.
//
// The caller must already have checked that the user may see the workspace.
func (s *Store) OpenWorkspaceTab(userID, workspaceID string, ephemeral bool) (WorkspaceTabList, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return WorkspaceTabList{}, fmt.Errorf("open workspace tab: begin: %w", err)
	}
	defer tx.Rollback()
	if err := s.lockUserTabsTx(tx, userID); err != nil {
		return WorkspaceTabList{}, err
	}
	ts := now()

	var existingEphemeral bool
	err = tx.QueryRow(s.q(`
		SELECT ephemeral FROM user_workspace_tabs WHERE user_id = ? AND workspace_id = ?
	`), userID, workspaceID).Scan(&existingEphemeral)
	switch {
	case err == nil:
		if existingEphemeral && !ephemeral {
			if _, err := tx.Exec(s.q(`
				UPDATE user_workspace_tabs SET ephemeral = ?, updated_at = ?
				WHERE user_id = ? AND workspace_id = ?
			`), s.dialect.BoolToInt(false), ts, userID, workspaceID); err != nil {
				return WorkspaceTabList{}, fmt.Errorf("open workspace tab: pin: %w", err)
			}
		}
		return s.commitUserTabsTx(tx, userID)
	case err != sql.ErrNoRows:
		return WorkspaceTabList{}, fmt.Errorf("open workspace tab: read: %w", err)
	}

	position := -1
	if ephemeral {
		var replaced string
		var replacedPos int
		err := tx.QueryRow(s.q(`
			SELECT workspace_id, position FROM user_workspace_tabs
			WHERE user_id = ? AND ephemeral = ?
			ORDER BY position ASC
			LIMIT 1
		`), userID, s.dialect.BoolToInt(true)).Scan(&replaced, &replacedPos)
		if err != nil && err != sql.ErrNoRows {
			return WorkspaceTabList{}, fmt.Errorf("open workspace tab: read ephemeral: %w", err)
		}
		if err == nil {
			position = replacedPos
		}
		// Every ephemeral row goes, not just the one read: "at most one" holds
		// even for a set that somehow held two.
		if _, err := tx.Exec(s.q(`
			DELETE FROM user_workspace_tabs WHERE user_id = ? AND ephemeral = ?
		`), userID, s.dialect.BoolToInt(true)); err != nil {
			return WorkspaceTabList{}, fmt.Errorf("open workspace tab: replace ephemeral: %w", err)
		}
	}
	if position < 0 {
		if err := tx.QueryRow(s.q(`
			SELECT COALESCE(MAX(position) + 1, 0) FROM user_workspace_tabs WHERE user_id = ?
		`), userID).Scan(&position); err != nil {
			return WorkspaceTabList{}, fmt.Errorf("open workspace tab: next position: %w", err)
		}
	}
	if _, err := tx.Exec(s.q(`
		INSERT INTO user_workspace_tabs (user_id, workspace_id, position, ephemeral, last_route, created_at, updated_at)
		VALUES (?, ?, ?, ?, NULL, ?, ?)
	`), userID, workspaceID, position, s.dialect.BoolToInt(ephemeral), ts, ts); err != nil {
		return WorkspaceTabList{}, fmt.Errorf("open workspace tab: insert: %w", err)
	}
	return s.commitUserTabsTx(tx, userID)
}

// CloseWorkspaceTab removes a workspace from the user's open set. Closing a
// tab that is not open is not an error. It takes the per-user lock like every
// other tab write, so its revision orders it among them (BUG-3285).
func (s *Store) CloseWorkspaceTab(userID, workspaceID string) (WorkspaceTabList, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return WorkspaceTabList{}, fmt.Errorf("close workspace tab: begin: %w", err)
	}
	defer tx.Rollback()
	if err := s.lockUserTabsTx(tx, userID); err != nil {
		return WorkspaceTabList{}, err
	}
	if _, err := tx.Exec(s.q(`
		DELETE FROM user_workspace_tabs WHERE user_id = ? AND workspace_id = ?
	`), userID, workspaceID); err != nil {
		return WorkspaceTabList{}, fmt.Errorf("close workspace tab: %w", err)
	}
	return s.commitUserTabsTx(tx, userID)
}

// ReorderWorkspaceTabs rewrites the user's tab positions. The workspaces in
// order come first, in that order; ids that name no open tab are ignored.
// Open tabs the list leaves out keep their relative order after it, so a
// reorder that raced an open on another device does not drop that tab.
func (s *Store) ReorderWorkspaceTabs(userID string, order []string) (WorkspaceTabList, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return WorkspaceTabList{}, fmt.Errorf("reorder workspace tabs: begin: %w", err)
	}
	defer tx.Rollback()
	if err := s.lockUserTabsTx(tx, userID); err != nil {
		return WorkspaceTabList{}, err
	}
	list, err := listWorkspaceTabsQ(s, tx, userID)
	if err != nil {
		return WorkspaceTabList{}, err
	}
	current := list.Rows
	open := make(map[string]bool, len(current))
	for _, r := range current {
		open[r.WorkspaceID] = true
	}
	placed := make(map[string]bool, len(current))
	next := make([]string, 0, len(current))
	for _, id := range order {
		if open[id] && !placed[id] {
			placed[id] = true
			next = append(next, id)
		}
	}
	for _, r := range current {
		if !placed[r.WorkspaceID] {
			next = append(next, r.WorkspaceID)
		}
	}
	ts := now()
	for i, id := range next {
		if _, err := tx.Exec(s.q(`
			UPDATE user_workspace_tabs SET position = ?, updated_at = ?
			WHERE user_id = ? AND workspace_id = ?
		`), i, ts, userID, id); err != nil {
			return WorkspaceTabList{}, fmt.Errorf("reorder workspace tabs: %w", err)
		}
	}
	return s.commitUserTabsTx(tx, userID)
}

// WorkspaceTabUpdate is a PATCH to one open tab. Nil / false members are left
// alone.
type WorkspaceTabUpdate struct {
	// Pin makes an ephemeral tab durable. There is no unpin.
	Pin bool
	// LastRoute replaces the stored route; an empty string clears it. The
	// caller validates it (the store does not know the workspace's prefix).
	LastRoute *string
}

// UpdateWorkspaceTab applies u to the user's tab for workspaceID. It returns
// sql.ErrNoRows when that workspace is not open.
func (s *Store) UpdateWorkspaceTab(userID, workspaceID string, u WorkspaceTabUpdate) (WorkspaceTabList, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return WorkspaceTabList{}, fmt.Errorf("update workspace tab: begin: %w", err)
	}
	defer tx.Rollback()
	if err := s.lockUserTabsTx(tx, userID); err != nil {
		return WorkspaceTabList{}, err
	}
	var ephemeral bool
	var lastRoute sql.NullString
	if err := tx.QueryRow(s.q(`
		SELECT ephemeral, last_route FROM user_workspace_tabs WHERE user_id = ? AND workspace_id = ?
	`), userID, workspaceID).Scan(&ephemeral, &lastRoute); err != nil {
		if err == sql.ErrNoRows {
			return WorkspaceTabList{}, sql.ErrNoRows
		}
		return WorkspaceTabList{}, fmt.Errorf("update workspace tab: read: %w", err)
	}
	if u.Pin {
		ephemeral = false
	}
	if u.LastRoute != nil {
		lastRoute = sql.NullString{String: *u.LastRoute, Valid: *u.LastRoute != ""}
	}
	if _, err := tx.Exec(s.q(`
		UPDATE user_workspace_tabs SET ephemeral = ?, last_route = ?, updated_at = ?
		WHERE user_id = ? AND workspace_id = ?
	`), s.dialect.BoolToInt(ephemeral), lastRoute, now(), userID, workspaceID); err != nil {
		return WorkspaceTabList{}, fmt.Errorf("update workspace tab: %w", err)
	}
	return s.commitUserTabsTx(tx, userID)
}

// execer is the write half of *sql.DB and *sql.Tx.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// pruneWorkspaceTabIfNoAccessTx deletes the user's tab for a workspace when
// the user holds neither a membership nor any grant in it. It runs inside the
// transaction of a per-user loss path (member removal, grant revoke), after
// that path's own delete. Keeping the tab when any grant survives is
// deliberate: a member demoted to guest still reaches the workspace, and the
// read filter, not this delete, decides what a grant set can see.
//
// When it deletes a row it bumps the user's tabs revision (BUG-3285).
//
// PRECONDITION: the caller took lockUserTabsTx(tx, userID) BEFORE ITS FIRST
// WRITE. Two things rest on it, both on Postgres:
//
//   - Lock order. Account deletion takes users(U) and then deletes U's
//     memberships; a removal that deleted the membership first and took
//     users(U) here would wait on it in the other order (measured: 40P01,
//     TestWorkspaceTabs_MemberRemovalVsAccountDeletion).
//   - One row set. Every tab write for U holds users(U), so while the caller
//     holds it no tab row of U's can appear or go, and the conditional bump
//     and the DELETE below see the same rows. Without it, under READ
//     COMMITTED, an open committing between the two statements is missed by
//     the bump's EXISTS and then deleted without a bump (codex round 2).
func (s *Store) pruneWorkspaceTabIfNoAccessTx(ex execer, userID, workspaceID string) error {
	const noAccess = `
		  AND NOT EXISTS (SELECT 1 FROM workspace_members WHERE workspace_id = ? AND user_id = ?)
		  AND NOT EXISTS (SELECT 1 FROM collection_grants WHERE workspace_id = ? AND user_id = ?)
		  AND NOT EXISTS (SELECT 1 FROM item_grants WHERE workspace_id = ? AND user_id = ?)`
	args := []any{workspaceID, userID, workspaceID, userID, workspaceID, userID}
	if _, err := ex.Exec(s.q(`
		UPDATE users SET workspace_tabs_revision = workspace_tabs_revision + 1
		WHERE id = ? AND EXISTS (
			SELECT 1 FROM user_workspace_tabs WHERE user_id = ? AND workspace_id = ?`+noAccess+`
		)
	`), append([]any{userID, userID, workspaceID}, args...)...); err != nil {
		return fmt.Errorf("prune workspace tab: bump revision: %w", err)
	}
	if _, err := ex.Exec(s.q(`
		DELETE FROM user_workspace_tabs
		WHERE user_id = ? AND workspace_id = ?`+noAccess+`
	`), append([]any{userID, workspaceID}, args...)...); err != nil {
		return fmt.Errorf("prune workspace tab: %w", err)
	}
	return nil
}

// lockWorkspaceTabHoldersTx locks, on Postgres, the users row of every user
// holding a tab on workspaceID, in id order. Soft delete calls it BEFORE it
// touches the workspaces row, and then deleteWorkspaceTabsForWorkspace.
//
// THE LOCK ORDER (BUG-3285), on Postgres (SQLite serialises every writer).
// Every transaction that locks a users row takes it before the other rows
// it writes:
//
//   - A tab write for U: users(U) (lockUserTabsTx), then U's tab rows. Its
//     FK check on workspaces takes FOR KEY SHARE, which the soft delete's
//     NO KEY UPDATE on the workspaces row does not block.
//   - A prune caller (member removal, grant revoke): users(U) before its
//     first write.
//   - Account deletion: users(D), then D's owned workspaces rows. A soft
//     delete that took the workspaces row first and D's users row second
//     would cross it (measured: 40P01,
//     TestWorkspaceTabs_SoftDeleteVsOwnerAccountDeletion, codex round 3),
//     which is why this runs first.
//   - Two soft deletes: both lock their holders in id order.
//
// A tab a concurrent open commits for the workspace after the holders are
// locked is deleted without a bump. By then the workspace is soft-deleted,
// so every answer filters it out on read anyway.
func (s *Store) lockWorkspaceTabHoldersTx(ex execer, workspaceID string) error {
	if s.dialect.Driver() != DriverPostgres {
		return nil
	}
	if _, err := ex.Exec(s.q(`
		SELECT id FROM users
		WHERE id IN (SELECT user_id FROM user_workspace_tabs WHERE workspace_id = ?)
		ORDER BY id
		FOR NO KEY UPDATE
	`), workspaceID); err != nil {
		return fmt.Errorf("delete workspace tabs: lock holders: %w", err)
	}
	return nil
}

// deleteWorkspaceTabsForWorkspace deletes every user's tab for a workspace,
// bumping each holder's tabs revision first (BUG-3285). Soft delete calls it
// after lockWorkspaceTabHoldersTx; a restore does not bring the rows back
// (PLAN-3002 Q12: the restorer gets an ephemeral tab from the landing, others
// reopen it). Account deletion does not use it (see DeleteAccountAtomic).
func (s *Store) deleteWorkspaceTabsForWorkspace(ex execer, workspaceID string) error {
	if _, err := ex.Exec(s.q(`
		UPDATE users SET workspace_tabs_revision = workspace_tabs_revision + 1
		WHERE id IN (SELECT user_id FROM user_workspace_tabs WHERE workspace_id = ?)
	`), workspaceID); err != nil {
		return fmt.Errorf("delete workspace tabs: bump revisions: %w", err)
	}
	if _, err := ex.Exec(s.q(`
		DELETE FROM user_workspace_tabs WHERE workspace_id = ?
	`), workspaceID); err != nil {
		return fmt.Errorf("delete workspace tabs: %w", err)
	}
	return nil
}
