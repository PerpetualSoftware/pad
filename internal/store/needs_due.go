package store

import (
	"log/slog"
	"time"

	"github.com/PerpetualSoftware/pad/internal/usertz"
)

// NeedsDueOccurrences returns the earliest outstanding human-audience claim
// per open item received by userID. BootstrapAgenda applies visibility,
// recipient, audience and terminal filters in SQL before its schedule scan cap.
// A capped read returns its partial set and logs the cap.
func (s *Store) NeedsDueOccurrences(workspaceID string, scope CatchUpScope, userID string, now time.Time) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	if scope.seesNothing() {
		return out, nil
	}
	user, err := s.GetUser(userID)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return out, nil
	}
	set, err := s.BootstrapAgenda(BootstrapAgendaParams{
		WorkspaceID: workspaceID, UserID: userID, Now: now, Loc: usertz.Resolve(user.Timezone).Loc,
		Scope: PendingReminderScope{CollectionIDs: scope.CollectionIDs, ItemIDs: scope.ItemIDs},
	})
	if err != nil {
		return nil, err
	}
	if set.Truncated {
		slog.Warn("needs: due occurrence scan truncated", "workspace_id", workspaceID, "limit", bootstrapAgendaScan)
	}
	for _, rows := range [][]BootstrapAgendaRow{set.DueToday, set.Carried, set.Stale} {
		for _, row := range rows {
			if row.Due.After(now) {
				continue
			}
			id := row.Schedule.ItemID
			if old, ok := out[id]; !ok || row.Due.Before(old) {
				out[id] = row.Due
			}
		}
	}
	return out, nil
}
