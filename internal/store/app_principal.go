package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The bot principal (SPEC-6 §3, TASK-3392): the users row an installed app
// acts as. It rides every per-handler read check as an ordinary member, and
// is refused everywhere a person's account would be trusted for being a
// person: every credential mint and every credential resolution, both account
// claims, admin and owner roles, and the people counts.

// ErrAppPrincipal refuses an operation on an installed app's bot principal.
// Where a door maps ErrUserDisabled to its refusal, the mints return this
// wrapped with ErrUserDisabled, so a bot gets exactly a disabled account's
// answer and no door needs a new branch to stay closed.
var ErrAppPrincipal = errors.New("this account is an installed app; manage it through the app's install")

// userKindQ returns a user's kind, or "" when the row is gone.
func (s *Store) userKindQ(q rowQueryer, userID string) (string, error) {
	var kind string
	err := q.QueryRow(s.q(`SELECT kind FROM users WHERE id = ?`), userID).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read user kind: %w", err)
	}
	return kind, nil
}

// refuseAppPrincipalQ returns ErrAppPrincipal when userID is a bot.
func (s *Store) refuseAppPrincipalQ(q rowQueryer, userID string) error {
	kind, err := s.userKindQ(q, userID)
	if err != nil {
		return err
	}
	if kind == models.UserKindApp {
		return ErrAppPrincipal
	}
	return nil
}

// addAppPrincipalMemberTx makes a bot a member of its install's workspace.
// The install transaction is its only caller. The ordinary member door
// (addWorkspaceMemberTx) refuses a bot at every role. A bot holds editor or
// viewer, never owner (DOC-3371 §3). It is not a seat under the default
// policy (appPrincipalMemberPolicy), so no plan limit is charged here.
func (s *Store) addAppPrincipalMemberTx(tx *sql.Tx, workspaceID, botUserID, role string) error {
	if role != "editor" && role != "viewer" {
		return ErrAppPrincipal
	}
	kind, err := s.userKindQ(tx, botUserID)
	if err != nil {
		return err
	}
	if kind != models.UserKindApp {
		return fmt.Errorf("add app principal member: %s is not an app principal", botUserID)
	}
	if _, err := tx.Exec(s.q(`
		INSERT INTO workspace_members (workspace_id, user_id, role, created_at)
		VALUES (?, ?, ?, ?)`), workspaceID, botUserID, role, now()); err != nil {
		return fmt.Errorf("add app principal member: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// DECISION POINT: SPEC-6 §11 Q2, waiting on Dave.
//
// Do bots appear in member lists, and do they count as Pad Cloud seats? The
// defaults are the lead's lean: a separate Apps section, and no seat. Both
// positions of both switches are pinned by TestTask3392_MemberListAndSeatPolicy,
// so the answer is a change to the default below and nothing else.
//
// Independent of the answer: a bot is never in the `members` list itself (so
// it is never an assignee candidate, and never what keeps an unusable
// workspace alive in removeUnusableWorkspace), and never an owner.
// ---------------------------------------------------------------------------

// AppMemberListMode says where a workspace's bots are listed.
type AppMemberListMode int

const (
	// AppMembersSeparate lists bots in their own `apps` array beside
	// `members` (the lead's lean).
	AppMembersSeparate AppMemberListMode = iota
	// AppMembersHidden lists them nowhere.
	AppMembersHidden
)

// AppPrincipalMemberPolicy is the Q2 decision.
type AppPrincipalMemberPolicy struct {
	MemberList   AppMemberListMode
	CountsAsSeat bool
}

var appPrincipalMemberPolicy = AppPrincipalMemberPolicy{MemberList: AppMembersSeparate, CountsAsSeat: false}

// AppPrincipalsListed reports whether member surfaces show a workspace's bots.
func AppPrincipalsListed() bool { return appPrincipalMemberPolicy.MemberList == AppMembersSeparate }

// WorkspaceAppPrincipal is one bot in a workspace's Apps section.
type WorkspaceAppPrincipal struct {
	UserID      string `json:"id"`
	DisplayName string `json:"display_name"`
	// AppName is the install's origin until manifests carry a name.
	AppName string `json:"app_name,omitempty"`
	Role    string `json:"role"`
}

// ListWorkspaceAppPrincipals returns the workspace's bots for the Apps
// section, or none when the policy hides them.
func (s *Store) ListWorkspaceAppPrincipals(workspaceID string) ([]WorkspaceAppPrincipal, error) {
	if !AppPrincipalsListed() {
		return []WorkspaceAppPrincipal{}, nil
	}
	rows, err := s.db.Query(s.q(`
		SELECT u.id, u.name, u.email, wm.role
		FROM workspace_members wm
		JOIN users u ON u.id = wm.user_id
		WHERE wm.workspace_id = ? AND u.kind = 'app'
		ORDER BY wm.created_at ASC`), workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list workspace app principals: %w", err)
	}
	type row struct {
		p     WorkspaceAppPrincipal
		email string
	}
	var found []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.p.UserID, &r.p.DisplayName, &r.email, &r.p.Role); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan workspace app principal: %w", err)
		}
		found = append(found, r)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	out := make([]WorkspaceAppPrincipal, 0, len(found))
	for _, r := range found {
		// CreateAppUserTx is the only writer of a bot's address, and it
		// encodes the install id there; nobody else can hold the domain.
		if id, ok := appPrincipalInstallID(r.email); ok {
			var origin string
			err := s.db.QueryRow(s.q(`SELECT origin FROM app_installs WHERE id = ? AND workspace_id = ?`), id, workspaceID).Scan(&origin)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("read app install: %w", err)
			}
			r.p.AppName = origin
		}
		out = append(out, r.p)
	}
	return out, nil
}

// appPrincipalInstallID recovers the install id from a bot's address.
func appPrincipalInstallID(email string) (string, bool) {
	local, domain, ok := strings.Cut(email, "@")
	if !ok || domain != models.AppPrincipalEmailDomain || !strings.HasPrefix(local, "app+") {
		return "", false
	}
	return strings.TrimPrefix(local, "app+"), true
}

// purgeAppPrincipalsOfOwnedWorkspacesTx erases the bots that are members of
// the workspaces ownerID owns, through eraseUserTx, in the caller's
// transaction (lead ruling on TASK-3392: account deletion leaves no orphan
// principals). A bot belongs to exactly one install, in one workspace.
func (s *Store) purgeAppPrincipalsOfOwnedWorkspacesTx(tx *sql.Tx, ownerID string) error {
	rows, err := tx.Query(s.q(`
		SELECT DISTINCT u.id FROM users u
		JOIN workspace_members wm ON wm.user_id = u.id
		JOIN workspaces w ON w.id = wm.workspace_id
		WHERE u.kind = 'app' AND w.owner_id = ?
		ORDER BY u.id`), ownerID)
	if err != nil {
		return fmt.Errorf("purge app principals: %w", err)
	}
	var bots []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("purge app principals: %w", err)
		}
		bots = append(bots, id)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("purge app principals: %w", err)
	}
	for _, id := range bots {
		if err := s.eraseUserTx(tx, id); err != nil {
			return fmt.Errorf("purge app principal %s: %w", id, err)
		}
	}
	return nil
}
