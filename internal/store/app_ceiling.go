package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The app ceiling (SPEC-6 §4, U6a, TASK-3401).
//
// An installed app reads at most its install's companion collections plus
// the workspace's system collections, and writes at most its companions.
// For the install's BOT (a service actor) the read ceiling is applied here, in
// VisibleCollectionIDsQ itself, so every caller of the store's visibility
// (item visibility, SSE, workspace /me, collab, reports, and any added later)
// gets it by construction, whatever the bot's membership grants. For a
// DELEGATED token (U5b) the actor is a person, whose ceiling belongs to the
// token, not the user; the app API applies it from the request context.

// AppPrincipalInstallIDQ returns the install a bot acts for: the install
// whose bot_user_id it is, else the one its address names. "" for a person,
// or a bot whose install is gone.
func (s *Store) AppPrincipalInstallIDQ(q Queryer, userID string) (string, error) {
	var kind, email string
	err := q.QueryRow(s.q(`SELECT kind, email FROM users WHERE id = ?`), userID).Scan(&kind, &email)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read principal: %w", err)
	}
	if kind != models.UserKindApp {
		return "", nil
	}
	var id string
	err = q.QueryRow(s.q(`SELECT id FROM app_installs WHERE bot_user_id = ?`), userID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("read principal install: %w", err)
	}
	if fromAddr, ok := appPrincipalInstallID(email); ok {
		var bot sql.NullString
		err := q.QueryRow(s.q(`SELECT bot_user_id FROM app_installs WHERE id = ?`), fromAddr).Scan(&bot)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return "", nil
		case err != nil:
			return "", fmt.Errorf("read principal install: %w", err)
		case bot.Valid && bot.String != "" && bot.String != userID:
			// The install names a different bot: this one is not its.
			return "", nil
		}
		return fromAddr, nil
	}
	return "", nil
}

// InstallCompanionCollectionIDsQ returns the install's companion collections:
// the live collections in its workspace stamped via_app = the install.
func (s *Store) InstallCompanionCollectionIDsQ(q Queryer, installID string) ([]string, error) {
	return s.collectIDs(q, `
		SELECT c.id FROM collections c
		JOIN app_installs i ON i.id = c.via_app AND i.workspace_id = c.workspace_id
		WHERE c.via_app = ? AND c.deleted_at IS NULL
		ORDER BY c.id`, installID)
}

// InstallReadCeilingQ is the install's read ceiling: its companion
// collections and the workspace's system collections.
func (s *Store) InstallReadCeilingQ(q Queryer, installID string) ([]string, error) {
	return s.collectIDs(q, `
		SELECT c.id FROM collections c
		JOIN app_installs i ON i.workspace_id = c.workspace_id
		WHERE i.id = ? AND c.deleted_at IS NULL AND (c.is_system OR c.via_app = i.id)
		ORDER BY c.id`, installID)
}

func (s *Store) collectIDs(q Queryer, query string, args ...any) ([]string, error) {
	rows, err := q.Query(s.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("read collection ids: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// applyAppPrincipalCeilingQ narrows visible to a bot's install ceiling.
// visible nil means "every collection", which under a ceiling becomes the
// ceiling itself; the result for a bot is never nil, so no caller can read
// it as "unrestricted". A person's visibility is returned unchanged.
func (s *Store) applyAppPrincipalCeilingQ(q Queryer, workspaceID, userID string, visible []string) ([]string, error) {
	installID, err := s.AppPrincipalInstallIDQ(q, userID)
	if err != nil {
		return nil, err
	}
	if installID == "" {
		var kind string
		if err := q.QueryRow(s.q(`SELECT kind FROM users WHERE id = ?`), userID).Scan(&kind); err == nil && kind == models.UserKindApp {
			return []string{}, nil // a bot with no install sees nothing
		}
		return visible, nil
	}
	ceiling, err := s.InstallReadCeilingQ(q, installID)
	if err != nil {
		return nil, err
	}
	return IntersectCollectionIDs(visible, ceiling), nil
}

// IntersectCollectionIDs narrows visible (nil = every collection) to ceiling,
// never returning nil.
func IntersectCollectionIDs(visible, ceiling []string) []string {
	if visible == nil {
		out := make([]string, len(ceiling))
		copy(out, ceiling)
		return out
	}
	in := make(map[string]bool, len(ceiling))
	for _, id := range ceiling {
		in[id] = true
	}
	out := []string{}
	for _, id := range visible {
		if in[id] {
			out = append(out, id)
		}
	}
	return out
}
