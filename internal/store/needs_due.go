package store

import (
	"log/slog"
	"time"

	"github.com/PerpetualSoftware/pad/internal/usertz"
)

// NeedsDue is one item's outstanding due claims for the viewer: the earliest
// (the routing claim) and the latest (when the newest one came due, which
// releases a "not mine" made before it, TASK-4643).
type NeedsDue struct {
	Earliest time.Time
	Latest   time.Time
}

// NeedsDueOccurrences returns the earliest and latest outstanding
// human-audience claims per open item received by userID. BootstrapAgenda applies visibility,
// recipient, audience and terminal filters in SQL before its schedule scan cap.
// A capped read returns its partial set and logs the cap. A moved occurrence
// claims from its moved instant, and a snoozed one waits until the snooze
// ends (TASK-4591): the person asked to hear about it later.
//
// Latest is the newest of: the outstanding occurrence's due instant, its
// snooze re-fire instant once that has passed (TASK-4711), and the newest
// later occurrence of the rule due at or before now (TASK-4683), walked in the
// viewer's zone with the same bounded walk ScheduleMissed counts.
func (s *Store) NeedsDueOccurrences(workspaceID string, scope CatchUpScope, userID string, now time.Time) (map[string]NeedsDue, error) {
	out := map[string]NeedsDue{}
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
	loc := usertz.Resolve(user.Timezone).Loc
	set, err := s.BootstrapAgenda(BootstrapAgendaParams{
		ExcludeAcknowledged: true,
		WorkspaceID:         workspaceID, UserID: userID, Now: now, Loc: loc,
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
			if row.Schedule.ReminderAckedAt != nil || row.Due.After(now) || row.SnoozedUntil != nil {
				continue
			}
			id := row.Schedule.ItemID
			d, ok := out[id]
			if !ok || row.Due.Before(d.Earliest) {
				d.Earliest = row.Due
			}
			if latest := needsRowLatest(row, loc, now); !ok || latest.After(d.Latest) {
				d.Latest = latest
			}
			out[id] = d
		}
	}
	return out, nil
}

// needsRowLatest is when the newest claim of an outstanding row came due: its
// due instant, a passed snooze re-fire, or the newest occurrence of the rule
// after the outstanding one that is due at or before now.
func needsRowLatest(row BootstrapAgendaRow, loc *time.Location, now time.Time) time.Time {
	latest := row.Due
	if row.RefireAt != nil && !row.RefireAt.After(now) && row.RefireAt.After(latest) {
		latest = *row.RefireAt
	}
	// The walk counts occurrences at or before its clock less a second.
	occs, _, err := missedOccurrences(row.Schedule, loc, now.Add(time.Second))
	if err != nil {
		return latest
	}
	if n := len(occs); n > 0 && occs[n-1].At.After(latest) && !occs[n-1].At.After(now) {
		latest = occs[n-1].At
	}
	return latest
}
