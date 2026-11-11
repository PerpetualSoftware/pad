package store

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// agendaRoleValues is shared by chronological, outstanding and bootstrap reads.
// The immutable seed determines identity; overrides change placement only.
func (s *Store) agendaRoleValues(sc *models.ItemSchedule, seed AgendaEntry, loc *time.Location) (AgendaEntry, error) {
	roles, err := s.scheduleRolesOf(s.db, sc)
	if err != nil {
		return seed, err
	}
	seed.OccurrenceKey = OccurrenceKey(sc, NormalizeInstant(seed.At), seed.Wall.String())
	overrides, err := s.occurrenceOverrides(s.db, sc.ID, seed.OccurrenceKey)
	if err != nil {
		return seed, err
	}
	return resolveAgendaRoles(sc, seed, loc, roles, overrides)
}

func resolveAgendaRoles(sc *models.ItemSchedule, seed AgendaEntry, loc *time.Location, roles scheduleRoles, overrides map[string]string) (AgendaEntry, error) {
	seed.OccurrenceKey = OccurrenceKey(sc, NormalizeInstant(seed.At), seed.Wall.String())
	seed.Roles = models.CommitmentRoles{DeadlineBehavior: roles.DeadlineBehavior, Outcome: roles.Outcome, RolesOrigin: roles.Origin}
	for _, r := range []struct {
		name       string
		days, secs *int
		into       **string
	}{
		{"target", &roles.TargetDays, &roles.TargetSecs, &seed.Roles.Target}, {"earliest", roles.EarliestDays, roles.EarliestSecs, &seed.Roles.Earliest}, {"deadline", roles.DeadlineDays, roles.DeadlineSecs, &seed.Roles.Deadline}, {"occupied_until", roles.OccupiedDays, roles.OccupiedSecs, &seed.Roles.OccupiedUntil},
	} {
		if r.days == nil || r.secs == nil {
			continue
		}
		v, err := resolveRoleValue(loc, seed.Wall.String(), *r.days, *r.secs, overrideFor(overrides, r.name))
		if err != nil {
			return seed, err
		}
		// Pinned elapsed recurrence can visit the same wall twice at a fold.
		// An unchanged role must retain that seed instant, not re-resolve its wall.
		if !sc.Floating && !seed.Wall.AllDay && *r.days == 0 && *r.secs == 0 && overrideFor(overrides, r.name) == nil {
			v.value = seed.At.UTC().Format(time.RFC3339Nano)
		}
		*r.into = ptr(v.value)
	}
	if seed.Roles.Target != nil {
		at, err := agendaRoleInstant(*seed.Roles.Target, loc)
		if err != nil {
			return seed, err
		}
		seed.At = at
		seed.Overdue = seed.State == "outstanding" && seed.Overdue
	}
	return seed, nil
}

func agendaRoleInstant(v string, loc *time.Location) (time.Time, error) {
	if len(v) == 10 {
		w, err := schedule.ParseWall(v)
		if err != nil {
			return time.Time{}, err
		}
		return schedule.Resolve(loc, w), nil
	}
	return time.Parse(time.RFC3339, v)
}

func agendaRoleInWindow(e AgendaEntry, loc *time.Location, from, to time.Time) bool {
	for _, v := range []*string{e.Roles.Target, e.Roles.Earliest, e.Roles.Deadline, e.Roles.OccupiedUntil} {
		if v == nil {
			continue
		}
		at, err := agendaRoleInstant(*v, loc)
		if err == nil && !at.Before(from) && at.Before(to) {
			return true
		}
	}
	return false
}

// AgendaOutstanding reads one outstanding occurrence without a calendar window.
// It shares the same role resolver with ExpandAgendaCut, never a recurrence walk.
func (s *Store) AgendaOutstanding(sc *models.ItemSchedule, view *time.Location, now time.Time) (*AgendaEntry, error) {
	if sc.EndedAt != nil || sc.NextWall == nil {
		return nil, nil
	}
	loc := view
	if !sc.Floating {
		var err error
		loc, err = time.LoadLocation(derefStr(sc.Tz))
		if err != nil {
			return nil, err
		}
	}
	at, ok, err := OutstandingInstant(sc, view)
	if err != nil || !ok {
		return nil, err
	}
	wall, err := schedule.ParseWall(*sc.NextWall)
	if err != nil {
		return nil, err
	}
	e, err := s.agendaRoleValues(sc, AgendaEntry{At: at, Wall: wall, State: "outstanding", Fired: sc.ReminderFired}, loc)
	if err != nil {
		return nil, err
	}
	e.Overdue = !e.At.After(now)
	if e.Roles.Deadline != nil {
		at, err := agendaRoleInstant(*e.Roles.Deadline, loc)
		if err != nil {
			return nil, err
		}
		if len(*e.Roles.Deadline) == 10 {
			at = at.AddDate(0, 0, 1)
		}
		e.DeadlinePassed = !at.After(now)
	}
	if n, _, err := ScheduleMissed(sc, view, now); err == nil {
		e.MissedCount, e.Stale = n, n >= 2
	}
	return &e, nil
}

// ExpandAgendaCut uses the existing bounded seed expansion for each role's
// inverse window, then resolves and deduplicates by immutable occurrence key.
func (s *Store) ExpandAgendaCut(sc *models.ItemSchedule, view *time.Location, from, to, now time.Time) (AgendaExpansion, error) {
	if view == nil {
		view = time.UTC
	}
	loc := view
	if !sc.Floating {
		var err error
		loc, err = time.LoadLocation(derefStr(sc.Tz))
		if err != nil {
			return AgendaExpansion{Unavailable: "internal"}, nil
		}
	}
	roles, err := s.scheduleRolesOf(s.db, sc)
	if err != nil {
		return AgendaExpansion{}, err
	}
	doneSQL, args := s.buildChildrenDoneExpr(s.doneFiltersForWorkspace(sc.WorkspaceID), "i")
	var terminal bool
	err = s.db.QueryRow(s.q(`SELECT CASE WHEN `+doneSQL+` THEN 1 ELSE 0 END FROM items i WHERE i.id=?`), append(args, sc.ItemID)...).Scan(&terminal)
	if err != nil {
		return AgendaExpansion{}, err
	}
	gates, err := s.evaluateGatesQ(s.db, sc.WorkspaceID, sc.ItemID)
	if err != nil {
		return AgendaExpansion{}, err
	}
	if !terminal && gates.state != models.GateStateReleased {
		return AgendaExpansion{}, nil
	}
	overrides, err := s.scheduleOverridesQ(s.db, sc.WorkspaceID, sc.ID)
	if err != nil {
		return AgendaExpansion{}, err
	}
	byKey := map[string]map[string]string{}
	for _, o := range overrides[sc.ID] {
		if byKey[o.OccurrenceKey] == nil {
			byKey[o.OccurrenceKey] = map[string]string{}
		}
		byKey[o.OccurrenceKey][o.Role] = o.ValueAt
	}
	var x AgendaExpansion
	budget := time.Now().Add(1500 * time.Millisecond)
	candidates := map[string]AgendaEntry{}
	add := func(es []AgendaEntry) {
		for _, e := range es {
			key := OccurrenceKey(sc, NormalizeInstant(e.At), e.Wall.String())
			candidates[key] = e
		}
	}
	// Terminal reads walk only recorded rows, regardless of the rule's health.
	if terminal {
		var rows []agendaRow
		for _, window := range agendaSeedWindows(roles, loc, from, to) {
			part, err := s.agendaOccurrences(sc.ID, window[0], window[1])
			if err != nil {
				return x, err
			}
			rows = append(rows, part...)
		}
		for _, r := range rows {
			w, err := schedule.ParseWall(r.wall)
			if err != nil {
				continue
			}
			at, err := time.Parse(time.RFC3339, r.at)
			if sc.Floating || w.AllDay {
				at, err = schedule.Resolve(loc, w), nil
			}
			if err != nil {
				continue
			}
			add([]AgendaEntry{{At: at, Wall: w, State: r.state, ResolvedAt: r.resolvedAt, Actor: r.actor, CommentID: r.commentID}})
		}
	} else {
		for _, window := range agendaSeedWindows(roles, loc, from, to) {
			if time.Now().After(budget) {
				x.Truncated, x.Unpromised = true, true
				break
			}
			part, err := s.expandAgendaSeedsCut(sc, view, window[0], window[1], now)
			if err != nil {
				return x, err
			}
			add(part.Entries)
			x.Truncated = x.Truncated || part.Truncated
			x.Unexpanded = x.Unexpanded || part.Unexpanded
			x.Unpromised = x.Unpromised || part.Unpromised
			if part.Unavailable != "" {
				x.Unavailable = part.Unavailable
			}
			x.NextFrom = earlier(x.NextFrom, part.NextFrom)
			if part.Truncated && (window[0] != from || window[1] != to) {
				x.Unpromised = true
			}
		}
		// Sparse exceptions can move from anywhere into the window. They are
		// candidates only; role membership is checked below.
		for _, o := range overrides[sc.ID] {
			if time.Now().After(budget) {
				x.Truncated, x.Unpromised = true, true
				break
			}
			parts := strings.Split(o.OccurrenceKey, "|")
			wall := parts[len(parts)-1]
			w, err := schedule.ParseWall(wall)
			if err != nil {
				continue
			}
			at := schedule.Resolve(loc, w)
			if len(parts) == 2 {
				at, err = time.Parse(time.RFC3339, parts[0])
				if err != nil {
					continue
				}
			}
			key := OccurrenceKey(sc, NormalizeInstant(at), wall)
			if _, ok := candidates[key]; ok {
				continue
			}
			rows, err := s.agendaOccurrences(sc.ID, at, at.Add(time.Second))
			if err != nil {
				return x, err
			}
			es, err := s.agendaEntries(sc, []schedule.Occurrence{{At: at, Wall: w}}, rows, view, loc, at, at, at.Add(time.Second), now)
			if err != nil {
				return x, err
			}
			add(es)
		}
	}
	// Sort seeds before a time cut so repeated reads make the same promise.
	keys := make([]string, 0, len(candidates))
	for key := range candidates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		seed := candidates[key]
		if time.Now().After(budget) {
			x.Truncated, x.Unpromised = true, true
			break
		}
		e, err := resolveAgendaRoles(sc, seed, loc, roles, byKey[key])
		if err != nil {
			return AgendaExpansion{Unavailable: "internal"}, nil
		}
		if !terminal && e.State == "outstanding" {
			e.Overdue = !e.At.After(now)
		}
		if agendaRoleInWindow(e, loc, from, to) {
			x.Entries = append(x.Entries, e)
		}
	}
	sort.Slice(x.Entries, func(i, j int) bool {
		a, b := x.Entries[i], x.Entries[j]
		if !a.At.Equal(b.At) {
			return a.At.Before(b.At)
		}
		return a.OccurrenceKey < b.OccurrenceKey
	})
	if len(x.Entries) > AgendaMaxPerSchedule {
		x.Entries = x.Entries[:AgendaMaxPerSchedule]
		x.Truncated, x.Unpromised = true, true
	}
	return x, nil
}

// Each role is inverted on the civil clock. Padding accounts for offset
// transitions; splitting retains the engine's 92-day window bound.
func agendaSeedWindows(roles scheduleRoles, loc *time.Location, from, to time.Time) [][2]time.Time {
	var out [][2]time.Time
	seen := map[[2]time.Time]bool{}
	for _, r := range [][2]*int{{&roles.TargetDays, &roles.TargetSecs}, {roles.EarliestDays, roles.EarliestSecs}, {roles.DeadlineDays, roles.DeadlineSecs}, {roles.OccupiedDays, roles.OccupiedSecs}} {
		if r[0] == nil || r[1] == nil {
			continue
		}
		lo, hi := from, to
		if *r[0] != 0 || *r[1] != 0 {
			inverse := func(t time.Time) time.Time {
				v := t.In(loc)
				naive := time.Date(v.Year(), v.Month(), v.Day(), v.Hour(), v.Minute(), v.Second(), 0, time.UTC).Add(-time.Duration(*r[1])*time.Second).AddDate(0, 0, -*r[0])
				return schedule.Resolve(loc, wallOf(naive))
			}
			lo, hi = inverse(from).Add(-36*time.Hour), inverse(to).Add(36*time.Hour)
		}
		for lo.Before(hi) {
			end := lo.Add(schedule.DefaultLimits.MaxWindow)
			if end.After(hi) {
				end = hi
			}
			window := [2]time.Time{lo, end}
			if !seen[window] {
				out = append(out, window)
				seen[window] = true
			}
			lo = end
		}
	}
	return out
}

// AgendaWorkCandidate is an admitted open item before group classification.
type AgendaWorkCandidate struct{ ItemID, ItemRef, ItemTitle, Collection, Fields, Schema string }

// ListAgendaWork applies visibility and recipient admission before the scan cap.
// Empty recipient means the workspace agenda's entire visible work set.
func (s *Store) ListAgendaWork(workspaceID string, scope PendingReminderScope, recipient, audience string, limit int) ([]AgendaWorkCandidate, bool, error) {
	scopeSQL, args := scopeClause("i", scope)
	open, oa := s.nonTerminalFilter(workspaceID, "i")
	query := `SELECT i.id,i.slug,i.title,c.slug,c.prefix,i.item_number,i.fields,c.schema FROM items i JOIN collections c ON c.id=i.collection_id WHERE i.workspace_id=? AND i.deleted_at IS NULL AND c.deleted_at IS NULL AND ` + open + scopeSQL
	params := append(append([]any{workspaceID}, oa...), args...)
	if recipient != "" {
		query += ` AND (CASE WHEN i.assigned_user_id IS NULL OR i.assigned_user_id='' THEN i.created_by_user_id ELSE i.assigned_user_id END)=?`
		params = append(params, recipient)
		query += ` AND (NOT EXISTS (SELECT 1 FROM item_schedules sc WHERE sc.item_id=i.id) OR EXISTS (SELECT 1 FROM item_schedules sc WHERE sc.item_id=i.id AND sc.audience IN (?, 'both')))`
		params = append(params, audience)
	}
	rows, err := s.db.Query(s.q(query+` ORDER BY i.id LIMIT ?`), append(params, limit+1)...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []AgendaWorkCandidate
	for rows.Next() {
		var c AgendaWorkCandidate
		var prefix string
		var num *int64
		if err := rows.Scan(&c.ItemID, &c.ItemRef, &c.ItemTitle, &c.Collection, &prefix, &num, &c.Fields, &c.Schema); err != nil {
			return nil, false, err
		}
		if num != nil {
			c.ItemRef = fmt.Sprintf("%s-%d", prefix, *num)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}
