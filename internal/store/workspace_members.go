package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/kernelevents"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// InvitationTTL is how long a newly-created workspace invitation stays
// accepted-by-code. 14 days mirrors the convention of every password-reset
// or magic-link flow: long enough for a distracted invitee to notice the
// email, short enough that a leaked code doesn't stay exploitable forever.
const InvitationTTL = 14 * 24 * time.Hour

// AddWorkspaceMember adds a user to a workspace with the given role.
//
// Transactional as of TASK-2658 — it was a bare Exec before. The membership
// row and its member.joined event must commit together or not at all (SPEC-3
// §choke point); a self-committing INSERT followed by a separate emit is the
// exact shape that loses events on a crash and leaks them on a later failure.
// The transaction wraps a single INSERT, so it costs nothing beyond the
// BEGIN/COMMIT pair.
//
// With WithPlanLimit() the workspace's members_per_workspace cap is counted
// first, under acquirePlanLimitLock, and a reached cap refuses with
// *PlanLimitError (BUG-2808). The server passes it on the direct add and on
// both invitation accepts (BUG-3098); the owner auto-adds and the ownerless
// backfill pass nothing.
func (s *Store) AddWorkspaceMember(workspaceID, userID, role string, opts ...MintOption) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("add workspace member: %w", err)
	}
	defer tx.Rollback()

	if _, _, err := s.addWorkspaceMemberTx(tx, workspaceID, userID, role, resolveMintOptions(opts), false); err != nil {
		return err
	}
	if err := s.commitWorkspaceMemberTx(tx); err != nil {
		return fmt.Errorf("add workspace member: %w", err)
	}
	return nil
}

// addWorkspaceMemberTx is the one membership insert, shared by
// AddWorkspaceMember and AcceptWorkspaceInvitation so the lock, the cap, the
// INSERT and the member.joined event cannot drift between them (BUG-3281).
//
// With existingOK false it is AddWorkspaceMember's contract, unchanged: a
// plain INSERT, so an existing membership fails on the primary key. With
// existingOK true an existing membership is not an error. It is left exactly
// as it is (role included), nothing is emitted, and added is false. The
// existence read comes before the cap, so a member of a full workspace is not
// refused by a limit their accept would not consume.
//
// Two concurrent existingOK calls are settled by ON CONFLICT DO NOTHING, not
// by a lock: the loser's INSERT waits on the winner's key, affects no row, and
// reads the winner's row back. So the lock is taken exactly where
// AddWorkspaceMember always took it, under WithPlanLimit only.
//
// effectiveRole is the role the membership holds after the call.
func (s *Store) addWorkspaceMemberTx(tx *sql.Tx, workspaceID, userID, role string, mint mintOptions, existingOK bool) (added bool, effectiveRole string, err error) {
	// A bot's membership comes from its install (addAppPrincipalMemberTx),
	// never from the member door, an invitation or a backfill (TASK-3392).
	if err := s.refuseAppPrincipalQ(tx, userID); err != nil {
		return false, "", err
	}
	if mint.planLimit {
		if err := s.acquirePlanLimitLock(tx, workspaceID, "members_per_workspace"); err != nil {
			return false, "", err
		}
	}

	readRole := func() (string, error) {
		var r string
		err := tx.QueryRow(s.q(`SELECT role FROM workspace_members WHERE workspace_id = ? AND user_id = ?`), workspaceID, userID).Scan(&r)
		return r, err
	}

	if existingOK {
		r, err := readRole()
		if err == nil {
			return false, r, nil
		}
		if err != sql.ErrNoRows {
			return false, "", fmt.Errorf("add workspace member: check membership: %w", err)
		}
	}

	if mint.planLimit {
		if err := s.enforceWorkspaceLimitTx(tx, workspaceID, "members_per_workspace"); err != nil {
			return false, "", err
		}
	}

	insert := `
		INSERT INTO workspace_members (workspace_id, user_id, role, created_at)
		VALUES (?, ?, ?, ?)`
	if existingOK {
		insert += ` ON CONFLICT (workspace_id, user_id) DO NOTHING`
	}
	ts := now()
	res, err := tx.Exec(s.q(insert), workspaceID, userID, role, ts)
	if err != nil {
		return false, "", fmt.Errorf("add workspace member: %w", err)
	}
	if existingOK {
		n, err := res.RowsAffected()
		if err != nil {
			return false, "", fmt.Errorf("add workspace member: %w", err)
		}
		if n == 0 {
			// A concurrent accept inserted the row after the read above.
			r, err := readRole()
			if err != nil {
				return false, "", fmt.Errorf("add workspace member: read concurrent membership: %w", err)
			}
			return false, r, nil
		}
	}

	if err := s.emitMemberEventTx(tx, kernelevents.MemberJoined, workspaceID, userID, role, ts); err != nil {
		return false, "", err
	}
	return true, role, nil
}

// commitWorkspaceMemberTx commits a transaction that ran addWorkspaceMemberTx.
// It is routed through the seam so a test can reproduce the one commit outcome
// these paths must survive and cannot otherwise be shown: a commit that lands
// and reports an error (BUG-3026). Nil in production, where this is
// tx.Commit().
func (s *Store) commitWorkspaceMemberTx(tx *sql.Tx) error {
	if s.commitAddWorkspaceMember != nil {
		return s.commitAddWorkspaceMember(tx)
	}
	return tx.Commit()
}

// AcceptWorkspaceInvitation accepts an invitation in one transaction: the
// membership through addWorkspaceMemberTx, then the invitation's accepted_at.
// An existing membership is an idempotent success (BUG-3281, lead ruling):
// the invitation is marked accepted and the member keeps their role in either
// direction, because role changes belong to member management. added reports
// whether this call created the membership; effectiveRole is the role held.
//
// As with AddWorkspaceMember, an error does not prove nothing was written: a
// commit can land and report failure (BUG-3026). A caller reads the
// membership and the invitation before treating an error as "not accepted".
// On a COMMIT error only, added and effectiveRole are still filled in with
// what the transaction wrote, which is the outcome if it landed; on every
// earlier error they are zero.
func (s *Store) AcceptWorkspaceInvitation(invitationID, workspaceID, userID, role string, opts ...MintOption) (added bool, effectiveRole string, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, "", fmt.Errorf("accept workspace invitation: %w", err)
	}
	defer tx.Rollback()

	added, effectiveRole, err = s.addWorkspaceMemberTx(tx, workspaceID, userID, role, resolveMintOptions(opts), true)
	if err != nil {
		return false, "", err
	}
	// The first accept's timestamp stands; a concurrent loser leaves it alone.
	if _, err := tx.Exec(s.q(`UPDATE workspace_invitations SET accepted_at = ? WHERE id = ? AND accepted_at IS NULL`), now(), invitationID); err != nil {
		return false, "", fmt.Errorf("accept workspace invitation: %w", err)
	}
	if err := s.commitWorkspaceMemberTx(tx); err != nil {
		// added and effectiveRole describe what this transaction wrote, so they
		// are returned with a commit error: if the caller finds the commit
		// landed, they are its outcome.
		return added, effectiveRole, fmt.Errorf("accept workspace invitation: %w", err)
	}
	return added, effectiveRole, nil
}

// RemoveWorkspaceMember removes a user from a workspace.
//
// The user's workspace tab goes with it when no grant keeps them in the
// workspace (TASK-3256).
func (s *Store) RemoveWorkspaceMember(workspaceID, userID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()
	// users(U) before any other row: the prune below bumps U's tabs revision,
	// and every transaction that does takes that lock first (BUG-3285).
	if err := s.lockUserTabsTx(tx, userID); err != nil {
		return err
	}
	// Removing a bot is uninstalling its app (TASK-3392).
	if err := s.refuseAppPrincipalQ(tx, userID); err != nil {
		return err
	}
	if err := s.guardOwnerLossTx(tx, workspaceID, userID); err != nil {
		return err
	}
	result, err := tx.Exec(
		s.q("DELETE FROM workspace_members WHERE workspace_id = ? AND user_id = ?"),
		workspaceID, userID,
	)
	if err != nil {
		return fmt.Errorf("remove workspace member: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	if err := s.pruneWorkspaceTabIfNoAccessTx(tx, userID, workspaceID); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveWorkspaceMemberAndRevokeGrants atomically removes a user from a workspace
// and revokes all their grants in a single transaction. This prevents the user
// from retaining guest access if the member removal succeeds but grant revocation fails.
func (s *Store) RemoveWorkspaceMemberAndRevokeGrants(workspaceID, userID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()
	// users(U) before any other row: the prune below bumps U's tabs revision,
	// and every transaction that does takes that lock first (BUG-3285).
	if err := s.lockUserTabsTx(tx, userID); err != nil {
		return err
	}
	// Removing a bot is uninstalling its app (TASK-3392).
	if err := s.refuseAppPrincipalQ(tx, userID); err != nil {
		return err
	}
	if err := s.guardOwnerLossTx(tx, workspaceID, userID); err != nil {
		return err
	}

	// Revoke grants first (before removing membership)
	if _, err := tx.Exec(s.q("DELETE FROM collection_grants WHERE workspace_id = ? AND user_id = ?"), workspaceID, userID); err != nil {
		return fmt.Errorf("revoke collection grants: %w", err)
	}
	if _, err := tx.Exec(s.q("DELETE FROM item_grants WHERE workspace_id = ? AND user_id = ?"), workspaceID, userID); err != nil {
		return fmt.Errorf("revoke item grants: %w", err)
	}

	// Remove membership
	result, err := tx.Exec(s.q("DELETE FROM workspace_members WHERE workspace_id = ? AND user_id = ?"), workspaceID, userID)
	if err != nil {
		return fmt.Errorf("remove workspace member: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	if err := s.pruneWorkspaceTabIfNoAccessTx(tx, userID, workspaceID); err != nil {
		return err
	}

	return tx.Commit()
}

// GetWorkspaceMember retrieves a single membership record.
func (s *Store) GetWorkspaceMember(workspaceID, userID string) (*models.WorkspaceMember, error) {
	return s.GetWorkspaceMemberQ(s.db, workspaceID, userID)
}

// GetWorkspaceMemberQ is GetWorkspaceMember parameterized over its executor
// (see Queryer).
func (s *Store) GetWorkspaceMemberQ(q Queryer, workspaceID, userID string) (*models.WorkspaceMember, error) {
	var m models.WorkspaceMember
	var createdAt string

	err := q.QueryRow(s.q(`
		SELECT workspace_id, user_id, role, collection_access, created_at
		FROM workspace_members
		WHERE workspace_id = ? AND user_id = ?
	`), workspaceID, userID).Scan(
		&m.WorkspaceID, &m.UserID, &m.Role, &m.CollectionAccess, &createdAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get workspace member: %w", err)
	}

	m.CreatedAt = parseTime(createdAt)
	return &m, nil
}

// ListWorkspaceMembers returns all members of a workspace, enriched with
// user name and email from a join.
func (s *Store) ListWorkspaceMembers(workspaceID string) ([]models.WorkspaceMember, error) {
	rows, err := s.db.Query(s.q(`
		SELECT wm.workspace_id, wm.user_id, wm.role, wm.collection_access, wm.created_at,
		       u.name, u.email, u.username
		FROM workspace_members wm
		JOIN users u ON u.id = wm.user_id
		WHERE wm.workspace_id = ? AND u.kind = 'human'
		ORDER BY wm.created_at ASC
	`), workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list workspace members: %w", err)
	}
	defer rows.Close()

	var result []models.WorkspaceMember
	for rows.Next() {
		var m models.WorkspaceMember
		var createdAt string
		if err := rows.Scan(
			&m.WorkspaceID, &m.UserID, &m.Role, &m.CollectionAccess, &createdAt,
			&m.UserName, &m.UserEmail, &m.UserUsername,
		); err != nil {
			return nil, fmt.Errorf("scan workspace member: %w", err)
		}
		m.CreatedAt = parseTime(createdAt)
		result = append(result, m)
	}
	return result, rows.Err()
}

// VisibleCollectionIDs returns the set of collection IDs a member can see.
// Returns nil if the member has "all" access (meaning no filtering needed).
// System collections (conventions, playbooks) are ordinary collections here:
// a restricted member sees one only when it is in their
// member_collection_access or granted (TASK-3376). Migration 108 listed them
// for every member restricted before that change.
func (s *Store) VisibleCollectionIDs(workspaceID, userID string) ([]string, error) {
	return s.VisibleCollectionIDsQ(s.db, workspaceID, userID)
}

// VisibleCollectionIDsQ is VisibleCollectionIDs parameterized over its
// executor (see Queryer).
func (s *Store) VisibleCollectionIDsQ(q Queryer, workspaceID, userID string) ([]string, error) {
	ids, err := s.membershipVisibleCollectionIDsQ(q, workspaceID, userID)
	if err != nil {
		return nil, err
	}
	// An installed app's bot sees no further than its install's ceiling,
	// whatever its membership says, at EVERY caller (SPEC-6 U6a, TASK-3401).
	return s.applyAppPrincipalCeilingQ(q, workspaceID, userID, ids)
}

// membershipVisibleCollectionIDsQ is the visibility a membership (or a
// guest's grants) gives, before any app ceiling.
func (s *Store) membershipVisibleCollectionIDsQ(q Queryer, workspaceID, userID string) ([]string, error) {
	member, err := s.GetWorkspaceMemberQ(q, workspaceID, userID)
	if err != nil {
		return nil, err
	}
	if member == nil {
		// Not a member — check for guest access via grants
		return s.GuestVisibleCollectionIDsQ(q, workspaceID, userID)
	}

	// "all" access — return nil to indicate no filtering needed
	if member.CollectionAccess == "all" || member.CollectionAccess == "" {
		return nil, nil
	}

	// "specific" access — get the granted collection IDs
	rows, err := q.Query(s.q(`
		SELECT collection_id FROM member_collection_access
		WHERE workspace_id = ? AND user_id = ?
	`), workspaceID, userID)
	if err != nil {
		return nil, fmt.Errorf("get member collection access: %w", err)
	}
	defer rows.Close()

	ids := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Also include collections from direct collection grants. This ensures
	// that members with "specific" access who are granted additional
	// collections can see them even if they aren't in member_collection_access.
	// Note: we only merge full collection grants here, NOT collections derived
	// from item grants. Item grants should not promote to collection-wide
	// visibility for members — the item-level filtering in handlers handles that.
	collGrantRows, err := q.Query(s.q(`
		SELECT DISTINCT collection_id FROM collection_grants
		WHERE workspace_id = ? AND user_id = ?
	`), workspaceID, userID)
	if err != nil {
		return nil, fmt.Errorf("get member collection grants: %w", err)
	}
	defer collGrantRows.Close()
	for collGrantRows.Next() {
		var id string
		if err := collGrantRows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	if err := collGrantRows.Err(); err != nil {
		return nil, err
	}

	// Also include collections that contain items with item grants, so the
	// collection appears in navigation. The actual item-level filtering is
	// handled by the request handlers for members who also have item grants.
	//
	// Live collections only (BUG-3333). Unlike the member's assigned
	// collections and direct collection grants above, which already gave
	// them the whole collection (and stay, so a grant on a deleted
	// collection remains revocable), this is a nav-only promotion. For a
	// deleted collection GuestVisibleResources returns no item grant, so the
	// handlers fell back to this list and served EVERY live item in the
	// deleted collection to a member granted one of them.
	itemCollRows, err := q.Query(s.q(`
		SELECT DISTINCT i.collection_id
		FROM item_grants ig
		JOIN items i ON i.id = ig.item_id
		JOIN collections c ON c.id = i.collection_id
		WHERE ig.workspace_id = ? AND ig.user_id = ? AND i.deleted_at IS NULL AND c.deleted_at IS NULL
	`), workspaceID, userID)
	if err != nil {
		return nil, fmt.Errorf("get member item grant collections: %w", err)
	}
	defer itemCollRows.Close()
	for itemCollRows.Next() {
		var id string
		if err := itemCollRows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	if err := itemCollRows.Err(); err != nil {
		return nil, err
	}

	result := make([]string, 0, len(ids))
	for id := range ids {
		result = append(result, id)
	}
	return result, nil
}

// SetMemberCollectionAccess updates a member's collection_access mode and
// replaces their specific collection grants atomically.
func (s *Store) SetMemberCollectionAccess(workspaceID, userID, mode string, collectionIDs []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()
	if err := s.setMemberCollectionAccessTx(tx, workspaceID, userID, mode, collectionIDs); err != nil {
		return err
	}
	return tx.Commit()
}

// setMemberCollectionAccessTx is SetMemberCollectionAccess on the caller's
// transaction: the app installer scopes its bot to the companion collections
// in the provisioning transaction (TASK-3397, U8b; DOC-3371 §3).
func (s *Store) setMemberCollectionAccessTx(tx *sql.Tx, workspaceID, userID, mode string, collectionIDs []string) error {
	ts := now()

	// Validate that all collection IDs belong to this workspace
	if mode == "specific" && len(collectionIDs) > 0 {
		for _, collID := range collectionIDs {
			var count int
			if err := tx.QueryRow(s.q(`SELECT COUNT(*) FROM collections WHERE id = ? AND workspace_id = ?`), collID, workspaceID).Scan(&count); err != nil {
				return fmt.Errorf("validate collection: %w", err)
			}
			if count == 0 {
				return fmt.Errorf("collection %s does not belong to workspace", collID)
			}
		}
	}

	// Update the mode on workspace_members
	_, err := tx.Exec(s.q(`
		UPDATE workspace_members SET collection_access = ?
		WHERE workspace_id = ? AND user_id = ?
	`), mode, workspaceID, userID)
	if err != nil {
		return fmt.Errorf("update collection_access: %w", err)
	}

	// Clear existing grants
	_, err = tx.Exec(s.q(`
		DELETE FROM member_collection_access
		WHERE workspace_id = ? AND user_id = ?
	`), workspaceID, userID)
	if err != nil {
		return fmt.Errorf("clear collection access: %w", err)
	}

	// Insert new grants (only if mode is "specific")
	if mode == "specific" {
		for _, collID := range collectionIDs {
			_, err := tx.Exec(s.q(`
				INSERT INTO member_collection_access (workspace_id, user_id, collection_id, created_at)
				VALUES (?, ?, ?, ?)
			`), workspaceID, userID, collID, ts)
			if err != nil {
				return fmt.Errorf("insert collection access: %w", err)
			}
		}
	}

	return nil
}

// GetMemberCollectionAccess returns the collection IDs a member has been
// explicitly granted access to (only meaningful when collection_access = "specific").
func (s *Store) GetMemberCollectionAccess(workspaceID, userID string) ([]string, error) {
	return s.GetMemberCollectionAccessQ(s.db, workspaceID, userID)
}

// GetMemberCollectionAccessQ is GetMemberCollectionAccess parameterized over
// its executor (see Queryer).
func (s *Store) GetMemberCollectionAccessQ(q Queryer, workspaceID, userID string) ([]string, error) {
	rows, err := q.Query(s.q(`
		SELECT collection_id FROM member_collection_access
		WHERE workspace_id = ? AND user_id = ?
	`), workspaceID, userID)
	if err != nil {
		return nil, fmt.Errorf("get member collection access: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetUserMemberWorkspaces returns only workspaces where the user has
// a workspace_members row — NO guest-grant fallback. Mirrors the
// first half of GetUserWorkspaces but without the UNION with
// collection_grants / item_grants tables.
//
// Use this when the caller needs the strict "is the user a member?"
// shape — e.g. BUG-1617's bearer-admin cross-workspace enumeration:
// RequireWorkspaceAccess gives bearer-admin membership-only access
// (no grants fallback), and the cross-ws backlinks enumeration must
// match that policy. Without this, a bearer-admin with a stray
// collection grant on workspace C would still see cross-ws backlinks
// from C even though direct bearer access to C is denied.
//
// The returned slice does NOT include the "last item activity"
// MAX(items.updated_at) computation — callers wanting that detail
// should use GetUserWorkspaces. This helper is for membership-set
// enumeration only, where the UpdatedAt of each row is the raw
// workspace row timestamp.
func (s *Store) GetUserMemberWorkspaces(userID string) ([]models.Workspace, error) {
	rows, err := s.db.Query(s.q(`
		SELECT w.id, w.name, w.slug, w.owner_id, COALESCE(ou.username, ''), w.description, w.settings, w.source, w.created_at, w.updated_at, w.deleted_at,
		       wm.sort_order
		FROM workspaces w
		JOIN workspace_members wm ON wm.workspace_id = w.id
		LEFT JOIN users ou ON ou.id = w.owner_id
		WHERE wm.user_id = ? AND w.deleted_at IS NULL
		ORDER BY wm.sort_order ASC, w.name ASC
	`), userID)
	if err != nil {
		return nil, fmt.Errorf("get user member workspaces: %w", err)
	}
	defer rows.Close()

	var result []models.Workspace
	for rows.Next() {
		var ws models.Workspace
		var createdAt, updatedAt string
		var deletedAt *string
		if err := rows.Scan(
			&ws.ID, &ws.Name, &ws.Slug, &ws.OwnerID, &ws.OwnerUsername, &ws.Description, &ws.Settings, &ws.Source,
			&createdAt, &updatedAt, &deletedAt,
			&ws.SortOrder,
		); err != nil {
			return nil, fmt.Errorf("scan workspace: %w", err)
		}
		ws.CreatedAt = parseTime(createdAt)
		ws.UpdatedAt = parseTime(updatedAt)
		ws.DeletedAt = parseTimePtr(deletedAt)
		ws.HydrateDerivedFields()
		result = append(result, ws)
	}
	return result, rows.Err()
}

// GetUserWorkspaces returns all workspaces a user has access to,
// sorted by the user's custom sort order (then name as tiebreaker).
func (s *Store) GetUserWorkspaces(userID string) ([]models.Workspace, error) {
	// BUG-1481: include MAX(items.updated_at) so the workspace's
	// effective UpdatedAt reflects item activity, not just row mtime.
	// For members with collection_access='specific', restrict MAX to
	// items in collections the member can actually see (via
	// member_collection_access, collection_grants, or item_grants) — otherwise the freshness signal leaks activity
	// for collections the member doesn't have access to. Members with
	// 'all' (or empty) access see every collection, so the predicate
	// short-circuits and behaves like an unrestricted MAX. The
	// visibility rule mirrors VisibleCollectionIDs above.
	rows, err := s.db.Query(s.q(`
		SELECT w.id, w.name, w.slug, w.owner_id, COALESCE(ou.username, ''), w.description, w.settings, w.source, w.created_at, w.updated_at, w.deleted_at,
		       wm.sort_order,
		       (
		           SELECT MAX(i.updated_at) FROM items i
		           JOIN collections c ON c.id = i.collection_id
		           WHERE i.workspace_id = w.id
		             AND c.workspace_id = w.id
		             AND i.deleted_at IS NULL
		             AND c.deleted_at IS NULL
		             AND (
		                 COALESCE(wm.collection_access, '') IN ('', 'all')
		                 OR EXISTS (
		                     SELECT 1 FROM member_collection_access mca
		                     WHERE mca.workspace_id = w.id
		                       AND mca.user_id = wm.user_id
		                       AND mca.collection_id = i.collection_id
		                 )
		                 OR EXISTS (
		                     SELECT 1 FROM collection_grants cg
		                     WHERE cg.workspace_id = w.id
		                       AND cg.collection_id = i.collection_id
		                       AND cg.user_id = wm.user_id
		                 )
		                 OR EXISTS (
		                     SELECT 1 FROM item_grants ig
		                     WHERE ig.workspace_id = w.id
		                       AND ig.item_id = i.id
		                       AND ig.user_id = wm.user_id
		                 )
		             )
		       )
		FROM workspaces w
		JOIN workspace_members wm ON wm.workspace_id = w.id
		LEFT JOIN users ou ON ou.id = w.owner_id
		WHERE wm.user_id = ? AND w.deleted_at IS NULL
		ORDER BY wm.sort_order ASC, w.name ASC
	`), userID)
	if err != nil {
		return nil, fmt.Errorf("get user workspaces: %w", err)
	}
	defer rows.Close()

	var result []models.Workspace
	for rows.Next() {
		var ws models.Workspace
		var createdAt, updatedAt string
		var deletedAt *string
		var lastItemActivity sql.NullString
		if err := rows.Scan(
			&ws.ID, &ws.Name, &ws.Slug, &ws.OwnerID, &ws.OwnerUsername, &ws.Description, &ws.Settings, &ws.Source,
			&createdAt, &updatedAt, &deletedAt,
			&ws.SortOrder,
			&lastItemActivity,
		); err != nil {
			return nil, fmt.Errorf("scan workspace: %w", err)
		}
		ws.CreatedAt = parseTime(createdAt)
		ws.UpdatedAt = effectiveWorkspaceUpdatedAt(updatedAt, lastItemActivity)
		ws.DeletedAt = parseTimePtr(deletedAt)
		ws.HydrateDerivedFields()
		result = append(result, ws)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Also include workspaces where the user has grants but is NOT a member (guest access)
	memberIDs := make(map[string]bool)
	for _, ws := range result {
		memberIDs[ws.ID] = true
	}

	// BUG-1481: for guests, the workspace freshness must reflect ONLY
	// items the guest can actually see — items in collections they have
	// a collection_grant on, or specific items they have an item_grant
	// on. Otherwise the workspace's UpdatedAt leaks activity timing for
	// items behind grants the guest doesn't hold. The visible-items
	// subquery mirrors the WHERE-clause grant logic below.
	guestRows, err := s.db.Query(s.q(`
		SELECT DISTINCT w.id, w.name, w.slug, w.owner_id, COALESCE(ou.username, ''), w.description, w.settings, w.source, w.created_at, w.updated_at, w.deleted_at,
		       (
		           SELECT MAX(i.updated_at) FROM items i
		           JOIN collections c ON c.id = i.collection_id
		           WHERE i.workspace_id = w.id
		             AND c.workspace_id = w.id
		             AND i.deleted_at IS NULL
		             AND c.deleted_at IS NULL
		             AND (
		                 EXISTS (
		                     SELECT 1 FROM collection_grants cg
		                     WHERE cg.workspace_id = w.id
		                       AND cg.collection_id = i.collection_id
		                       AND cg.user_id = ?
		                 )
		                 OR EXISTS (
		                     SELECT 1 FROM item_grants ig
		                     WHERE ig.workspace_id = w.id
		                       AND ig.item_id = i.id
		                       AND ig.user_id = ?
		                 )
		             )
		       )
		FROM workspaces w
		LEFT JOIN users ou ON ou.id = w.owner_id
		WHERE w.deleted_at IS NULL AND (
			EXISTS (
				SELECT 1 FROM collection_grants cg
				JOIN collections c ON c.id = cg.collection_id
				WHERE cg.workspace_id = w.id AND cg.user_id = ? AND c.deleted_at IS NULL
			)
			OR EXISTS (
				SELECT 1 FROM item_grants ig
				JOIN items i ON i.id = ig.item_id
				JOIN collections c ON c.id = i.collection_id
				WHERE ig.workspace_id = w.id AND ig.user_id = ? AND i.deleted_at IS NULL AND c.deleted_at IS NULL
			)
		)
	`), userID, userID, userID, userID)
	if err != nil {
		return nil, fmt.Errorf("get guest workspaces: %w", err)
	}
	defer guestRows.Close()

	for guestRows.Next() {
		var ws models.Workspace
		var createdAt, updatedAt string
		var deletedAt *string
		var lastItemActivity sql.NullString
		if err := guestRows.Scan(
			&ws.ID, &ws.Name, &ws.Slug, &ws.OwnerID, &ws.OwnerUsername, &ws.Description, &ws.Settings, &ws.Source,
			&createdAt, &updatedAt, &deletedAt,
			&lastItemActivity,
		); err != nil {
			return nil, fmt.Errorf("scan guest workspace: %w", err)
		}
		// Skip workspaces the user is already a member of
		if memberIDs[ws.ID] {
			continue
		}
		ws.CreatedAt = parseTime(createdAt)
		ws.UpdatedAt = effectiveWorkspaceUpdatedAt(updatedAt, lastItemActivity)
		ws.DeletedAt = parseTimePtr(deletedAt)
		ws.IsGuest = true
		ws.HydrateDerivedFields()
		result = append(result, ws)
	}
	if err := guestRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate guest workspaces: %w", err)
	}

	return result, nil
}

// UpdateWorkspaceSortOrder sets the sort_order for a workspace in a user's membership.
func (s *Store) UpdateWorkspaceSortOrder(userID, workspaceID string, sortOrder int) error {
	result, err := s.db.Exec(
		s.q("UPDATE workspace_members SET sort_order = ? WHERE user_id = ? AND workspace_id = ?"),
		sortOrder, userID, workspaceID,
	)
	if err != nil {
		return fmt.Errorf("update workspace sort order: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// IsWorkspaceMember checks if a user is a member of a workspace.
func (s *Store) IsWorkspaceMember(workspaceID, userID string) (bool, error) {
	var count int
	err := s.db.QueryRow(
		s.q("SELECT COUNT(*) FROM workspace_members WHERE workspace_id = ? AND user_id = ?"),
		workspaceID, userID,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check workspace membership: %w", err)
	}
	return count > 0, nil
}

// UpdateWorkspaceMemberRole changes a member's role in a workspace.
func (s *Store) UpdateWorkspaceMemberRole(workspaceID, userID, role string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()
	// A bot's role belongs to its install (TASK-3392).
	if err := s.refuseAppPrincipalQ(tx, userID); err != nil {
		return err
	}
	if role != "owner" {
		if err := s.guardOwnerLossTx(tx, workspaceID, userID); err != nil {
			return err
		}
	}
	result, err := tx.Exec(
		s.q("UPDATE workspace_members SET role = ? WHERE workspace_id = ? AND user_id = ?"),
		role, workspaceID, userID,
	)
	if err != nil {
		return fmt.Errorf("update workspace member role: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

// ErrCanonicalOwner refuses demoting or removing a workspace's canonical
// owner (workspaces.owner_id) through the member doors (BUG-3355). owner_id
// sets the workspace's URL, who may restore it and whose plan it counts
// against, so authority must not move away from it; moving owner_id itself
// is an ownership transfer, a separate operation.
var ErrCanonicalOwner = errors.New("the workspace's owner cannot be demoted or removed; transfer ownership first")

// ErrLastOwner refuses a demotion or removal that would leave a workspace
// with no owner member (BUG-3355).
var ErrLastOwner = errors.New("a workspace must keep at least one owner")

// guardOwnerLossTx is called inside the transaction that demotes userID away
// from owner or removes them. It refuses the canonical owner outright, and
// refuses the workspace's last owner member.
//
// The canonical-owner check is the invariant's anchor, and needs no lock:
// owner_id changes only by an ownership transfer, which does not exist, and
// the canonical owner holds an owner membership in every live workspace
// (census 2026-10-02: 28 of 28 on the dev instance), so refusing to demote or
// remove that one row keeps every such workspace with an owner whatever runs
// concurrently. The last-owner count below only matters for a legacy row
// whose canonical owner is NOT an owner member, and is best-effort there: it
// takes no row locks, deliberately. Locking the other owners' rows FOR UPDATE
// (codex r1) neither serialized a concurrent promote-then-demote under READ
// COMMITTED nor kept a lock order account deletion could agree with.
func (s *Store) guardOwnerLossTx(tx *sql.Tx, workspaceID, userID string) error {
	var ownerID sql.NullString
	switch err := tx.QueryRow(s.q(`SELECT owner_id FROM workspaces WHERE id = ?`), workspaceID).Scan(&ownerID); {
	case errors.Is(err, sql.ErrNoRows):
		// No workspace: the write below finds no member row either.
	case err != nil:
		return fmt.Errorf("read workspace owner: %w", err)
	case ownerID.Valid && ownerID.String == userID:
		return ErrCanonicalOwner
	}

	// A bot is never an owner; if one were planted it would still not be
	// "another owner" (TASK-3392).
	rows, err := tx.Query(s.q(`SELECT wm.user_id FROM workspace_members wm JOIN users u ON u.id = wm.user_id
		WHERE wm.workspace_id = ? AND wm.role = 'owner' AND u.kind = 'human'`), workspaceID)
	if err != nil {
		return fmt.Errorf("read workspace owners: %w", err)
	}
	defer rows.Close()
	owners, isOwner := 0, false
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("read workspace owners: %w", err)
		}
		owners++
		isOwner = isOwner || id == userID
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read workspace owners: %w", err)
	}
	if isOwner && owners <= 1 {
		return ErrLastOwner
	}
	return nil
}

// --- Invitations ---

// CreateInvitation creates a pending workspace invitation.
// Generates a 128-bit (16-byte) random code and stores only its SHA-256 hash.
// The plaintext code is returned once to be shared with the invitee.
func (s *Store) CreateInvitation(workspaceID, email, role, invitedBy string) (*models.WorkspaceInvitation, error) {
	if models.IsReservedAppEmail(email) {
		return nil, ErrReservedAppEmail
	}
	// Generate a random 128-bit join code (32 hex chars)
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate invitation code: %w", err)
	}
	code := hex.EncodeToString(raw)

	// Store SHA-256 hash of the code (plaintext never stored)
	hash := sha256.Sum256([]byte(code))
	codeHash := hex.EncodeToString(hash[:])

	// The mailbox-only proof (TASK-3352): a second 128-bit secret that only
	// the invitation EMAIL carries. Hash stored, plaintext returned once.
	rawProof := make([]byte, 16)
	if _, err := rand.Read(rawProof); err != nil {
		return nil, fmt.Errorf("generate invitation proof: %w", err)
	}
	proof := hex.EncodeToString(rawProof)
	proofHash := invitationProofHash(proof)

	id := newID()
	ts := now()
	expiresAt := time.Now().UTC().Add(InvitationTTL).Format(time.RFC3339)

	// An invitation is a credential a disabled inviter cannot mint (BUG-3349,
	// requireActiveUserTx): the code becomes a membership for whoever redeems
	// it.
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("insert invitation: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if invitedBy != "" {
		if err := s.requireActiveUserTx(tx, invitedBy); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(s.q(`
		INSERT INTO workspace_invitations (id, workspace_id, email, role, invited_by, code, code_hash, created_at, expires_at, proof_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`), id, workspaceID, strings.ToLower(strings.TrimSpace(email)), role, invitedBy, id, codeHash, ts, expiresAt, proofHash); err != nil {
		return nil, fmt.Errorf("insert invitation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("insert invitation: commit: %w", err)
	}

	inv, err := s.GetInvitation(id)
	if err != nil {
		return nil, err
	}
	if inv == nil {
		// Deleted between the insert and this read, e.g. by the inviter's
		// account deletion, which deletes the invitations it sent
		// (BUG-3289). GetInvitation answers nil, nil for a missing row.
		return nil, fmt.Errorf("create invitation: deleted concurrently: %w", sql.ErrNoRows)
	}
	// Return the plaintext code to the caller (not stored in DB)
	inv.Code = code
	inv.Proof = proof
	return inv, nil
}

func invitationProofHash(proof string) string {
	h := sha256.Sum256([]byte(proof))
	return hex.EncodeToString(h[:])
}

// ConsumeInvitationProof spends an invitation's mailbox-only proof
// (TASK-3352): if proof is the one minted with invitationID, it is cleared
// and userID's email is marked verified, in one transaction, so the proof is
// single-use and the verification happens iff it is spent. It returns false,
// with nothing changed, for a wrong, empty or already-used proof, for an
// invitation past its expiry (decided here, inside the transaction, since a
// slow request can pass the handler's check and cross it), or for a disabled
// account.
//
// The caller MUST have checked that the account's email is the invitation's
// (invitationEmailMatches) before calling: the proof proves the MAILBOX, the
// match ties it to this account. Both callers do, handleAcceptInvitation and
// handleRegister; a new caller owes the same check.
//
// The user row is locked first, the order requireActiveUserTx establishes for
// a write that pairs an account with something else.
func (s *Store) ConsumeInvitationProof(invitationID, userID, proof string) (bool, error) {
	if invitationID == "" || userID == "" || proof == "" {
		return false, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("consume invitation proof: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(s.q(`UPDATE users SET updated_at = updated_at WHERE id = ? AND disabled_at IS NULL`), userID)
	if err != nil {
		return false, fmt.Errorf("consume invitation proof: lock user: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return false, fmt.Errorf("consume invitation proof: lock user: %w", err)
	} else if n == 0 {
		return false, nil // gone or disabled
	}

	res, err = tx.Exec(s.q(`
		UPDATE workspace_invitations SET proof_hash = ''
		WHERE id = ? AND proof_hash = ? AND proof_hash <> ''
		  AND (expires_at IS NULL OR expires_at > ?)`),
		invitationID, invitationProofHash(proof), time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return false, fmt.Errorf("consume invitation proof: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return false, fmt.Errorf("consume invitation proof: %w", err)
	} else if n == 0 {
		return false, nil
	}

	if _, err := tx.Exec(s.q(`UPDATE users SET email_verified_at = ?, updated_at = ? WHERE id = ?`), now(), now(), userID); err != nil {
		return false, fmt.Errorf("consume invitation proof: verify: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("consume invitation proof: commit: %w", err)
	}
	return true, nil
}

// GetInvitation retrieves an invitation by ID.
func (s *Store) GetInvitation(id string) (*models.WorkspaceInvitation, error) {
	var inv models.WorkspaceInvitation
	var acceptedAt, expiresAt *string
	var createdAt string

	err := s.db.QueryRow(s.q(`
		SELECT id, workspace_id, email, role, invited_by, code, accepted_at, expires_at, created_at
		FROM workspace_invitations WHERE id = ?
	`), id).Scan(
		&inv.ID, &inv.WorkspaceID, &inv.Email, &inv.Role, &inv.InvitedBy,
		&inv.Code, &acceptedAt, &expiresAt, &createdAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get invitation: %w", err)
	}

	inv.CreatedAt = parseTime(createdAt)
	inv.AcceptedAt = parseTimePtr(acceptedAt)
	inv.ExpiresAt = parseTimePtr(expiresAt)
	return &inv, nil
}

// GetInvitationByCode retrieves a pending invitation by its join code.
// Looks up by SHA-256 hash first (new invitations), then falls back to
// plaintext lookup for legacy invitations created before hashing.
func (s *Store) GetInvitationByCode(code string) (*models.WorkspaceInvitation, error) {
	// Hash the provided code for lookup
	hash := sha256.Sum256([]byte(code))
	codeHash := hex.EncodeToString(hash[:])

	var inv models.WorkspaceInvitation
	var acceptedAt, expiresAt *string
	var createdAt string

	// Try hashed lookup first (new invitations). Both lookups JOIN the live
	// workspace (BUG-3104): an invitation to a SOFT-DELETED workspace answers
	// exactly like an unknown code. DeleteAccountAtomic removes only the
	// invitations the departing user SENT, so one sent by another owner-role
	// member survives the workspace — and every door (accept, register with
	// invitation, preview) reads it through here. Not "expired": that answer
	// tells the invitee to ask for a new one, which cannot succeed.
	err := s.db.QueryRow(s.q(`
		SELECT i.id, i.workspace_id, i.email, i.role, i.invited_by, i.code, i.accepted_at, i.expires_at, i.created_at
		FROM workspace_invitations i
		JOIN workspaces w ON w.id = i.workspace_id AND w.deleted_at IS NULL
		WHERE i.code_hash = ? AND i.accepted_at IS NULL
	`), codeHash).Scan(
		&inv.ID, &inv.WorkspaceID, &inv.Email, &inv.Role, &inv.InvitedBy,
		&inv.Code, &acceptedAt, &expiresAt, &createdAt,
	)
	if err == nil {
		inv.CreatedAt = parseTime(createdAt)
		inv.AcceptedAt = parseTimePtr(acceptedAt)
		inv.ExpiresAt = parseTimePtr(expiresAt)
		return &inv, nil
	}
	if err != sql.ErrNoRows {
		return nil, fmt.Errorf("get invitation by code hash: %w", err)
	}

	// Fall back to plaintext lookup (legacy invitations), with the same
	// live-workspace join.
	err = s.db.QueryRow(s.q(`
		SELECT i.id, i.workspace_id, i.email, i.role, i.invited_by, i.code, i.accepted_at, i.expires_at, i.created_at
		FROM workspace_invitations i
		JOIN workspaces w ON w.id = i.workspace_id AND w.deleted_at IS NULL
		WHERE i.code = ? AND i.accepted_at IS NULL
	`), code).Scan(
		&inv.ID, &inv.WorkspaceID, &inv.Email, &inv.Role, &inv.InvitedBy,
		&inv.Code, &acceptedAt, &expiresAt, &createdAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get invitation by code: %w", err)
	}

	inv.CreatedAt = parseTime(createdAt)
	inv.AcceptedAt = parseTimePtr(acceptedAt)
	inv.ExpiresAt = parseTimePtr(expiresAt)
	return &inv, nil
}

// AcceptInvitation marks an invitation as accepted.
func (s *Store) AcceptInvitation(id string) error {
	_, err := s.db.Exec(
		s.q("UPDATE workspace_invitations SET accepted_at = ? WHERE id = ?"),
		now(), id,
	)
	if err != nil {
		return fmt.Errorf("accept invitation: %w", err)
	}
	return nil
}

// DeleteInvitation removes a pending invitation.
func (s *Store) DeleteInvitation(workspaceID, invitationID string) error {
	result, err := s.db.Exec(
		s.q("DELETE FROM workspace_invitations WHERE id = ? AND workspace_id = ? AND accepted_at IS NULL"),
		invitationID, workspaceID,
	)
	if err != nil {
		return fmt.Errorf("delete invitation: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListWorkspaceInvitations returns all invitations for a workspace.
func (s *Store) ListWorkspaceInvitations(workspaceID string) ([]models.WorkspaceInvitation, error) {
	rows, err := s.db.Query(s.q(`
		SELECT id, workspace_id, email, role, invited_by, code, accepted_at, expires_at, created_at
		FROM workspace_invitations
		WHERE workspace_id = ? AND accepted_at IS NULL
		ORDER BY created_at ASC
	`), workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list workspace invitations: %w", err)
	}
	defer rows.Close()

	var result []models.WorkspaceInvitation
	for rows.Next() {
		var inv models.WorkspaceInvitation
		var acceptedAt, expiresAt *string
		var createdAt string
		if err := rows.Scan(
			&inv.ID, &inv.WorkspaceID, &inv.Email, &inv.Role, &inv.InvitedBy,
			&inv.Code, &acceptedAt, &expiresAt, &createdAt,
		); err != nil {
			return nil, fmt.Errorf("scan invitation: %w", err)
		}
		inv.CreatedAt = parseTime(createdAt)
		inv.AcceptedAt = parseTimePtr(acceptedAt)
		inv.ExpiresAt = parseTimePtr(expiresAt)
		result = append(result, inv)
	}
	return result, rows.Err()
}

// backfillWorkspaceOwners ensures every workspace has at least one owner.
// For workspaces with no members, the first admin user is added as owner.
// This handles the migration case where workspaces existed before the user system.
func (s *Store) backfillWorkspaceOwners() error {
	// Find the first admin user (if any)
	var adminID string
	err := s.db.QueryRow(
		s.q("SELECT id FROM users WHERE role = 'admin' AND kind = 'human' ORDER BY created_at ASC LIMIT 1"),
	).Scan(&adminID)
	if err == sql.ErrNoRows {
		return nil // No users yet — nothing to backfill
	}
	if err != nil {
		return fmt.Errorf("find admin user: %w", err)
	}

	// Find workspaces with no members
	rows, err := s.db.Query(s.q(`
		SELECT w.id FROM workspaces w
		WHERE w.deleted_at IS NULL
		AND NOT EXISTS (SELECT 1 FROM workspace_members wm WHERE wm.workspace_id = w.id)
	`))
	if err != nil {
		return fmt.Errorf("find ownerless workspaces: %w", err)
	}
	defer rows.Close()

	var wsIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		wsIDs = append(wsIDs, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, wsID := range wsIDs {
		if err := s.AddWorkspaceMember(wsID, adminID, "owner"); err != nil {
			return fmt.Errorf("add owner to workspace %s: %w", wsID, err)
		}
	}

	// Backfill owner_id column on workspaces that don't have one set.
	// Uses D3 logic: earliest owner member → earliest member → first admin.
	ownerlessRows, err := s.db.Query(s.q(`
		SELECT id FROM workspaces WHERE (owner_id = '' OR owner_id IS NULL) AND deleted_at IS NULL
	`))
	if err != nil {
		return fmt.Errorf("find workspaces without owner_id: %w", err)
	}
	defer ownerlessRows.Close()

	var ownerlessIDs []string
	for ownerlessRows.Next() {
		var id string
		if err := ownerlessRows.Scan(&id); err != nil {
			return err
		}
		ownerlessIDs = append(ownerlessIDs, id)
	}
	if err := ownerlessRows.Err(); err != nil {
		return err
	}

	for _, wsID := range ownerlessIDs {
		var ownerID string

		// Try: earliest member with "owner" role
		err := s.db.QueryRow(s.q(`
			SELECT wm.user_id FROM workspace_members wm JOIN users u ON u.id = wm.user_id
			WHERE wm.workspace_id = ? AND wm.role = 'owner' AND u.kind = 'human'
			ORDER BY wm.created_at ASC LIMIT 1
		`), wsID).Scan(&ownerID)

		if err == sql.ErrNoRows {
			// Try: earliest member regardless of role
			err = s.db.QueryRow(s.q(`
				SELECT wm.user_id FROM workspace_members wm JOIN users u ON u.id = wm.user_id
				WHERE wm.workspace_id = ? AND u.kind = 'human'
				ORDER BY wm.created_at ASC LIMIT 1
			`), wsID).Scan(&ownerID)
		}

		if err == sql.ErrNoRows {
			// Fall back to first admin
			ownerID = adminID
		} else if err != nil {
			return fmt.Errorf("find owner for workspace %s: %w", wsID, err)
		}

		_, err = s.db.Exec(s.q(`UPDATE workspaces SET owner_id = ? WHERE id = ?`), ownerID, wsID)
		if err != nil {
			return fmt.Errorf("set owner_id for workspace %s: %w", wsID, err)
		}
	}

	return nil
}

// AdminInvitation holds enriched invitation data for the admin panel.
type AdminInvitation struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	Role          string `json:"role"`
	WorkspaceID   string `json:"workspace_id"`
	WorkspaceName string `json:"workspace_name"`
	WorkspaceSlug string `json:"workspace_slug"`
	InvitedByID   string `json:"invited_by_id"`
	InvitedByName string `json:"invited_by_name"`
	CreatedAt     string `json:"created_at"`
}

// ListPendingInvitationsAdmin returns all pending invitations across all workspaces
// with workspace and inviter details for the admin panel.
func (s *Store) ListPendingInvitationsAdmin(query string) ([]AdminInvitation, error) {
	var where []string
	var args []interface{}

	where = append(where, "i.accepted_at IS NULL")
	where = append(where, "w.deleted_at IS NULL")

	if query != "" {
		q := "%" + strings.ToLower(query) + "%"
		where = append(where, "(LOWER(i.email) LIKE ? OR LOWER(w.name) LIKE ?)")
		args = append(args, q, q)
	}

	whereClause := "WHERE " + strings.Join(where, " AND ")

	rows, err := s.db.Query(s.q(`
		SELECT i.id, i.email, i.role, w.id, w.name, w.slug, i.invited_by, COALESCE(u.name, ''), i.created_at
		FROM workspace_invitations i
		JOIN workspaces w ON w.id = i.workspace_id
		LEFT JOIN users u ON u.id = i.invited_by
		`+whereClause+`
		ORDER BY i.created_at DESC
	`), args...)
	if err != nil {
		return nil, fmt.Errorf("list pending invitations admin: %w", err)
	}
	defer rows.Close()

	var result []AdminInvitation
	for rows.Next() {
		var inv AdminInvitation
		if err := rows.Scan(&inv.ID, &inv.Email, &inv.Role, &inv.WorkspaceID, &inv.WorkspaceName,
			&inv.WorkspaceSlug, &inv.InvitedByID, &inv.InvitedByName, &inv.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan admin invitation: %w", err)
		}
		result = append(result, inv)
	}
	return result, rows.Err()
}

// DeleteInvitationAdmin removes a pending invitation by ID (no workspace scoping).
func (s *Store) DeleteInvitationAdmin(invitationID string) error {
	result, err := s.db.Exec(
		s.q("DELETE FROM workspace_invitations WHERE id = ? AND accepted_at IS NULL"),
		invitationID,
	)
	if err != nil {
		return fmt.Errorf("delete invitation admin: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// AdminUserWorkspace is a lightweight workspace membership for admin views.
type AdminUserWorkspace struct {
	WorkspaceID   string `json:"workspace_id"`
	WorkspaceName string `json:"workspace_name"`
	WorkspaceSlug string `json:"workspace_slug"`
	OwnerUsername string `json:"owner_username"`
	Role          string `json:"role"`
	JoinedAt      string `json:"joined_at"`
}

// AdminUserWorkspaceDetail extends AdminUserWorkspace with per-workspace
// aggregations rendered in the admin user modal's Workspaces tab.
// PLAN-1542 / TASK-1545.
type AdminUserWorkspaceDetail struct {
	AdminUserWorkspace
	// CollectionsCount excludes system collections (playbooks, conventions —
	// collections.is_system = 1). Counts non-deleted user-facing collections.
	CollectionsCount int `json:"collections_count"`
	// ItemsOpen counts non-deleted items whose status is NOT in the
	// terminal set. The terminal set is currently hardcoded (see
	// adminOpenItemsCountClause); a schema-aware terminal_options check
	// is a separate follow-up.
	ItemsOpen int `json:"items_open"`
	// ItemsTotal counts all non-deleted items in the workspace.
	ItemsTotal int `json:"items_total"`
	// MembersCount counts the workspace's people (includes the owner); an
	// installed app's bot is not one (TASK-3392).
	MembersCount int `json:"members_count"`
	// StorageBytes mirrors WorkspaceStorageUsage's definition: SUM of
	// non-deleted attachment size_bytes (including derived blobs).
	StorageBytes int64 `json:"storage_bytes"`
	// LastActivityAt is MAX(items.updated_at) across non-deleted items.
	// Empty string when the workspace has no items yet.
	LastActivityAt string `json:"last_activity_at,omitempty"`
}

// adminOpenItemsCountClause returns a SQL fragment (no leading AND) that
// excludes terminal-status items. Single-sourced from
// models.DefaultTerminalStatuses to match the rest of the codebase, and
// uses the dialect's JSON-extract helper so the query runs identically on
// SQLite and Postgres. The extracted value is COALESCEd against the empty
// string and LOWER-cased so items with NULL/missing status (NULL NOT IN
// (...) is not TRUE in SQL) and case-variant statuses still register as
// "open" — matching how search.go and items.go interpret terminal-status
// checks.
func (s *Store) adminOpenItemsCountClause() (clause string, args []interface{}) {
	terms := models.DefaultTerminalStatuses
	if len(terms) == 0 {
		return "1=1", nil
	}
	placeholders := make([]string, len(terms))
	args = make([]interface{}, len(terms))
	for i, v := range terms {
		placeholders[i] = "?"
		args[i] = strings.ToLower(v)
	}
	expr := "LOWER(COALESCE(" + s.dialect.JSONFieldText("i.fields", "status") + ", ''))"
	return expr + " NOT IN (" + strings.Join(placeholders, ", ") + ")", args
}

// GetUserWorkspacesDetailed returns each workspace this user is a member
// of with the per-workspace aggregations the admin modal's Workspaces tab
// renders. Caps the result at 50 rows (frontend caps display at 20 in
// T1552; the headroom is for future tabs that might want the full list).
// PLAN-1542 / TASK-1545.
func (s *Store) GetUserWorkspacesDetailed(userID string) ([]AdminUserWorkspaceDetail, error) {
	openClause, openArgs := s.adminOpenItemsCountClause()

	// Aggregations live in correlated subqueries rather than a wide JOIN +
	// GROUP BY because the workspaces a single user belongs to are at most
	// tens, not thousands — the subquery cost is fine and the SQL stays
	// readable. items_open uses JSON_EXTRACT on the fields blob to read
	// the status field, matching how the rest of the store queries it
	// (see search.go).
	query := `
		SELECT
			w.id, w.name, w.slug,
			COALESCE(ou.username, ''),
			wm.role, wm.created_at,
			(SELECT COUNT(*) FROM collections c
				WHERE c.workspace_id = w.id AND c.deleted_at IS NULL AND NOT c.is_system),
			(SELECT COUNT(*) FROM items i
				WHERE i.workspace_id = w.id AND i.deleted_at IS NULL AND ` + openClause + `),
			(SELECT COUNT(*) FROM items i
				WHERE i.workspace_id = w.id AND i.deleted_at IS NULL),
			(SELECT COUNT(*) FROM workspace_members wm2 JOIN users u2 ON u2.id = wm2.user_id WHERE wm2.workspace_id = w.id AND u2.kind = 'human'),
			(SELECT COALESCE(SUM(a.size_bytes), 0) FROM attachments a
				WHERE a.workspace_id = w.id AND a.deleted_at IS NULL),
			(SELECT MAX(i.updated_at) FROM items i
				WHERE i.workspace_id = w.id AND i.deleted_at IS NULL)
		FROM workspace_members wm
		JOIN workspaces w ON w.id = wm.workspace_id
		LEFT JOIN users ou ON ou.id = w.owner_id
		WHERE wm.user_id = ? AND w.deleted_at IS NULL
		ORDER BY w.name ASC
		LIMIT 50`

	// Two placeholder copies of openArgs because the subquery is
	// referenced once but each `?` is a separate placeholder. The order
	// is: openClause placeholders first, then wm.user_id.
	args := make([]interface{}, 0, len(openArgs)+1)
	args = append(args, openArgs...)
	args = append(args, userID)

	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("get user workspaces detailed: %w", err)
	}
	defer rows.Close()

	out := make([]AdminUserWorkspaceDetail, 0)
	for rows.Next() {
		var d AdminUserWorkspaceDetail
		var lastActivity sql.NullString
		if err := rows.Scan(
			&d.WorkspaceID, &d.WorkspaceName, &d.WorkspaceSlug,
			&d.OwnerUsername,
			&d.Role, &d.JoinedAt,
			&d.CollectionsCount,
			&d.ItemsOpen,
			&d.ItemsTotal,
			&d.MembersCount,
			&d.StorageBytes,
			&lastActivity,
		); err != nil {
			return nil, fmt.Errorf("scan workspace detail: %w", err)
		}
		if lastActivity.Valid {
			d.LastActivityAt = lastActivity.String
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetUserWorkspaceMemberships returns workspace memberships for admin user detail.
func (s *Store) GetUserWorkspaceMemberships(userID string) ([]AdminUserWorkspace, error) {
	rows, err := s.db.Query(s.q(`
		SELECT w.id, w.name, w.slug, COALESCE(ou.username, ''), wm.role, wm.created_at
		FROM workspace_members wm
		JOIN workspaces w ON w.id = wm.workspace_id
		LEFT JOIN users ou ON ou.id = w.owner_id
		WHERE wm.user_id = ? AND w.deleted_at IS NULL
		ORDER BY w.name ASC
	`), userID)
	if err != nil {
		return nil, fmt.Errorf("get user workspace memberships: %w", err)
	}
	defer rows.Close()

	var result []AdminUserWorkspace
	for rows.Next() {
		var m AdminUserWorkspace
		if err := rows.Scan(&m.WorkspaceID, &m.WorkspaceName, &m.WorkspaceSlug, &m.OwnerUsername, &m.Role, &m.JoinedAt); err != nil {
			return nil, fmt.Errorf("scan workspace membership: %w", err)
		}
		result = append(result, m)
	}
	return result, rows.Err()
}
