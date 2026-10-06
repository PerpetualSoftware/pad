package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

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
	id, _, err := s.appPrincipalInstallQ(q, userID)
	return id, err
}

// appPrincipalInstallQ is AppPrincipalInstallIDQ that also reports whether
// the user is a bot at all, from the same single users read (codex r1 P3).
//
// That read runs on EVERY VisibleCollectionIDsQ call, people included: one
// primary-key lookup, accepted by the lead (TASK-3401 U6a review) rather than
// widening the shared, hot membership query to carry users.kind. If profiling
// ever flags it, pass the already-loaded request user's kind in instead.
func (s *Store) appPrincipalInstallQ(q Queryer, userID string) (string, bool, error) {
	var kind, email string
	err := q.QueryRow(s.q(`SELECT kind, email FROM users WHERE id = ?`), userID).Scan(&kind, &email)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read principal: %w", err)
	}
	if kind != models.UserKindApp {
		return "", false, nil
	}
	var id string
	err = q.QueryRow(s.q(`SELECT id FROM app_installs WHERE bot_user_id = ?`), userID).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", true, fmt.Errorf("read principal install: %w", err)
	}
	if fromAddr, ok := appPrincipalInstallID(email); ok {
		var bot sql.NullString
		err := q.QueryRow(s.q(`SELECT bot_user_id FROM app_installs WHERE id = ?`), fromAddr).Scan(&bot)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return "", true, nil
		case err != nil:
			return "", true, fmt.Errorf("read principal install: %w", err)
		case bot.Valid && bot.String != "" && bot.String != userID:
			// The install names a different bot: this one is not its.
			return "", true, nil
		}
		return fromAddr, true, nil
	}
	return "", true, nil
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
	installID, isBot, err := s.appPrincipalInstallQ(q, userID)
	if err != nil {
		return nil, err
	}
	if !isBot {
		return visible, nil
	}
	if installID == "" {
		return []string{}, nil // a bot with no install sees nothing
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

// InstallAPIState is what the app API reads about an install per request.
type InstallAPIState struct {
	WorkspaceID     string
	State           string
	ServiceAccess   string // "read", "write", or "" (none)
	DelegatedAccess string // the manifest's delegated access as it stands now (TASK-3399)
	BotUserID       string
}

// GetInstallAPIState reads an install's workspace, state, service access and
// bot, or nil when there is no such install.
func (s *Store) GetInstallAPIState(installID string) (*InstallAPIState, error) {
	var st InstallAPIState
	var access, delegated, bot sql.NullString
	err := s.db.QueryRow(s.q(`SELECT workspace_id, state, service_access, delegated_access, bot_user_id FROM app_installs WHERE id = ?`), installID).
		Scan(&st.WorkspaceID, &st.State, &access, &delegated, &bot)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read install: %w", err)
	}
	st.ServiceAccess, st.DelegatedAccess, st.BotUserID = access.String, delegated.String, bot.String
	return &st, nil
}

// AppItemReadMeta is what the app API's item DTO needs beyond models.Item.
type AppItemReadMeta struct {
	ViaApp         string
	CreatorDisplay string
}

// ItemsAppReadMeta returns each item's via_app (the last install that wrote
// it) and its creator's display name, in one query.
func (s *Store) ItemsAppReadMeta(itemIDs []string) (map[string]AppItemReadMeta, error) {
	out := map[string]AppItemReadMeta{}
	if len(itemIDs) == 0 {
		return out, nil
	}
	ph := make([]string, len(itemIDs))
	args := make([]any, len(itemIDs))
	for i, id := range itemIDs {
		ph[i], args[i] = "?", id
	}
	rows, err := s.db.Query(s.q(`SELECT i.id, COALESCE(i.via_app, ''), COALESCE(u.name, '')
		FROM items i LEFT JOIN users u ON u.id = i.created_by_user_id
		WHERE i.id IN (`+strings.Join(ph, ",")+`)`), args...)
	if err != nil {
		return nil, fmt.Errorf("read item app meta: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var m AppItemReadMeta
		if err := rows.Scan(&id, &m.ViaApp, &m.CreatorDisplay); err != nil {
			return nil, err
		}
		out[id] = m
	}
	return out, rows.Err()
}

// UserKinds returns each user's kind ("human" or "app").
func (s *Store) UserKinds(userIDs []string) (map[string]string, error) {
	out := map[string]string{}
	if len(userIDs) == 0 {
		return out, nil
	}
	ph := make([]string, len(userIDs))
	args := make([]any, len(userIDs))
	for i, id := range userIDs {
		ph[i], args[i] = "?", id
	}
	rows, err := s.db.Query(s.q(`SELECT id, kind FROM users WHERE id IN (`+strings.Join(ph, ",")+`)`), args...)
	if err != nil {
		return nil, fmt.Errorf("read user kinds: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			return nil, err
		}
		out[id] = kind
	}
	return out, rows.Err()
}

// ItemIDsInCollectionsQ keeps the item IDs whose item sits in one of
// collectionIDs, in input order (BUG-3424: an app request's item grants are
// narrowed to its read ceiling). Never nil; empty in, empty out.
func (s *Store) ItemIDsInCollectionsQ(q Queryer, itemIDs, collectionIDs []string) ([]string, error) {
	out := []string{}
	if len(itemIDs) == 0 || len(collectionIDs) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(itemIDs)+len(collectionIDs))
	for _, id := range itemIDs {
		args = append(args, id)
	}
	for _, id := range collectionIDs {
		args = append(args, id)
	}
	rows, err := q.Query(s.q(`SELECT id FROM items WHERE id IN (`+placeholders(len(itemIDs))+`) AND collection_id IN (`+placeholders(len(collectionIDs))+`)`), args...)
	if err != nil {
		return nil, fmt.Errorf("items in collections: %w", err)
	}
	defer rows.Close()
	in := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("items in collections: %w", err)
		}
		in[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("items in collections: %w", err)
	}
	for _, id := range itemIDs {
		if in[id] {
			out = append(out, id)
		}
	}
	return out, nil
}
