package store

import (
	"fmt"
	"strings"
)

// App attribution for human-facing JSON (SPEC-6 U9c, TASK-3413): which
// installed app wrote an item, a comment or a version row, and its name.
//
// The name is the install's bot display name, else its origin: the rule
// ListUserAppGrants uses. app_installs rows are never deleted (an uninstalled
// install is a tombstone) and the bot's users row is kept disabled, so the
// name survives uninstall.

// AppAttribution is one row's app: the install id and its display name.
type AppAttribution struct {
	InstallID string
	Name      string
}

// attributionQuery runs one batch lookup: table.idCol IN ids, reading
// table.installCol, joined to the install and its bot.
func (s *Store) attributionQuery(table, installCol string, ids []string) (map[string]AppAttribution, error) {
	out := map[string]AppAttribution{}
	if len(ids) == 0 {
		return out, nil
	}
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i], args[i] = "?", id
	}
	rows, err := s.db.Query(s.q(`SELECT t.id, a.id, COALESCE(u.name, ''), a.origin
		FROM `+table+` t
		JOIN app_installs a ON a.id = t.`+installCol+`
		LEFT JOIN users u ON u.id = a.bot_user_id
		WHERE t.id IN (`+strings.Join(ph, ",")+`)`), args...)
	if err != nil {
		return nil, fmt.Errorf("app attribution (%s): %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, install, bot, origin string
		if err := rows.Scan(&id, &install, &bot, &origin); err != nil {
			return nil, err
		}
		name := bot
		if name == "" {
			name = origin
		}
		out[id] = AppAttribution{InstallID: install, Name: name}
	}
	return out, rows.Err()
}

// ItemsCreatedViaApp returns, for the items an app CREATED, that app
// (items.created_via_app, set once). The item JSON's via_app is the creator,
// agreed with the U9 label (Wren, day 88); the last app writer
// (items.via_app) is not exposed here.
func (s *Store) ItemsCreatedViaApp(itemIDs []string) (map[string]AppAttribution, error) {
	return s.attributionQuery("items", "created_via_app", itemIDs)
}

// CommentsViaApp returns, for the comments an app wrote, that app.
func (s *Store) CommentsViaApp(commentIDs []string) (map[string]AppAttribution, error) {
	return s.attributionQuery("comments", "via_app", commentIDs)
}

// VersionsViaApp returns, for the item version rows an app's write made,
// that app.
func (s *Store) VersionsViaApp(versionIDs []string) (map[string]AppAttribution, error) {
	return s.attributionQuery("item_versions", "via_app", versionIDs)
}
