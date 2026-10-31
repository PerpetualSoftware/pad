package store

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// The workspace agenda's store half (TASK-4104 D1): which schedules a caller
// may see, and one schedule's occurrences in a window with their state.

// AgendaMaxPerSchedule bounds one schedule's entries, so a runaway rule (a
// stored MINUTELY) cannot crowd every other schedule out of the response.
const AgendaMaxPerSchedule = 100

// AgendaSchedule is a schedule with the facts of its item the agenda shows.
type AgendaSchedule struct {
	Schedule   *models.ItemSchedule
	ItemRef    string
	ItemTitle  string
	Collection string // slug
}

// ListSchedulesForAgenda returns the schedules of the open, live items scope
// allows, by id, and whether there were more than limit. The scope and the
// terminal filter are applied in SQL, BEFORE the bound, so rows the caller
// cannot see, that are done, or that ended before from with no history row near
// [from, to) (an early resolve ends a schedule before its due instant) never
// push a visible schedule past it.
func (s *Store) ListSchedulesForAgenda(workspaceID string, scope PendingReminderScope, from, to time.Time, limit int) ([]AgendaSchedule, bool, error) {
	if scope.CollectionIDs != nil && len(scope.CollectionIDs) == 0 && len(scope.ItemIDs) == 0 {
		return nil, false, nil
	}
	scopeSQL, scopeArgs := scopeClause("i", scope)
	open, openArgs := s.nonTerminalFilter(workspaceID, "i")
	f := time.RFC3339
	args := append(append([]any{workspaceID, from.UTC().Format(f), from.UTC().Add(-36 * time.Hour).Format(f), to.UTC().Add(36 * time.Hour).Format(f), workspaceID}, scopeArgs...), openArgs...)
	// item_id IN (...), not a join: scheduleColumns names item_schedules.id.
	rows, err := s.db.Query(s.q(`SELECT `+scheduleColumns+` FROM item_schedules
		WHERE workspace_id = ? AND (ended_at IS NULL OR ended_at >= ? OR EXISTS (SELECT 1 FROM schedule_occurrences o
			WHERE o.schedule_id = item_schedules.id AND o.occurrence_at >= ? AND o.occurrence_at < ?)) AND item_id IN (
			SELECT i.id FROM items i JOIN collections c ON c.id = i.collection_id
			WHERE i.workspace_id = ? AND i.deleted_at IS NULL AND c.deleted_at IS NULL`+scopeSQL+` AND `+open+`)
		ORDER BY item_schedules.id LIMIT ?`), append(args, limit+1)...)
	if err != nil {
		return nil, false, fmt.Errorf("list schedules for agenda: %w", err)
	}
	defer rows.Close()
	var out []AgendaSchedule
	for rows.Next() {
		sc, err := scanSchedule(rows)
		if err != nil {
			return nil, false, fmt.Errorf("scan agenda schedule: %w", err)
		}
		out = append(out, AgendaSchedule{Schedule: sc})
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := len(out) > limit
	if truncated {
		out = out[:limit]
	}
	if len(out) == 0 {
		return nil, false, nil
	}
	ids, byID := make([]any, len(out)), map[string]*AgendaSchedule{}
	for i := range out {
		ids[i], byID[out[i].Schedule.ItemID] = out[i].Schedule.ItemID, &out[i]
	}
	irows, err := s.db.Query(s.q(`SELECT i.id, i.title, i.slug, c.slug, c.prefix, i.item_number
		FROM items i JOIN collections c ON c.id = i.collection_id WHERE i.id IN (`+placeholders(len(ids))+`)`), ids...)
	if err != nil {
		return nil, false, fmt.Errorf("read agenda items: %w", err)
	}
	defer irows.Close()
	for irows.Next() {
		var id, title, slug, coll, prefix string
		var number *int64
		if err := irows.Scan(&id, &title, &slug, &coll, &prefix, &number); err != nil {
			return nil, false, fmt.Errorf("scan agenda item: %w", err)
		}
		a := byID[id]
		a.ItemTitle, a.Collection, a.ItemRef = title, coll, slug // a legacy item has no ref
		if number != nil {
			a.ItemRef = fmt.Sprintf("%s-%d", prefix, *number)
		}
	}
	return out, truncated, irows.Err()
}

// AgendaEntry is one occurrence on the agenda. At is the grid instant, in the
// view's zone for a floating schedule; EffectiveAt is where a MOVE put it.
type AgendaEntry struct {
	At          time.Time
	Wall        schedule.Wall
	State       string // outstanding | scheduled | done | skipped | moved | missed | unrecorded
	Fired       bool
	Overdue     bool
	EffectiveAt *time.Time
	ResolvedAt  string
	Actor       string
	CommentID   string
	MissedCount int // the outstanding entry only
	Stale       bool
}

type agendaRow struct{ at, wall, state, commentID, actor, resolvedAt string }

// agendaOccurrences reads a schedule's history rows around [from, to), padded a
// day and a half each way because a floating row is the recipient's reading, plus
// its moved rows, which can sit anywhere.
func (s *Store) agendaOccurrences(scheduleID string, from, to time.Time) ([]agendaRow, error) {
	const f = "2006-01-02T15:04:05Z"
	rows, err := s.db.Query(s.q(`SELECT occurrence_at, occurrence_wall, state, COALESCE(comment_id, ''), actor, resolved_at
		FROM schedule_occurrences WHERE schedule_id = ? AND ((occurrence_at >= ? AND occurrence_at < ?) OR state = 'moved')`),
		scheduleID, from.UTC().Add(-36*time.Hour).Format(f), to.UTC().Add(36*time.Hour).Format(f))
	if err != nil {
		return nil, fmt.Errorf("list agenda occurrences: %w", err)
	}
	defer rows.Close()
	var out []agendaRow
	for rows.Next() {
		var r agendaRow
		if err := rows.Scan(&r.at, &r.wall, &r.state, &r.commentID, &r.actor, &r.resolvedAt); err != nil {
			return nil, fmt.Errorf("scan agenda occurrence: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ExpandAgenda lists sc's occurrences with from <= At < to, plus its outstanding
// occurrence when a move placed it in the window. view is the zone a floating
// schedule is read in; a pinned one ignores it.
//
// unavailable is a stable code when the schedule cannot be expanded (a stored
// error_code, a rule that no longer compiles, a cap, `internal` for stored state
// that will not parse): one such schedule is a per-schedule outcome, never an
// error for the whole agenda. err is a database failure only. truncated says
// more existed than the entries show.
//
// A history row is matched by the WALL for a floating or all-day schedule (a
// resolve stores the recipient's reading, and the same wall is another instant
// in the caller's zone) and by the INSTANT for a pinned timed one, whose elapsed
// rules can repeat a wall.
func (s *Store) ExpandAgenda(sc *models.ItemSchedule, view *time.Location, from, to, now time.Time) (entries []AgendaEntry, truncated bool, unavailable string, err error) {
	if sc.ErrorCode != nil {
		return nil, false, *sc.ErrorCode, nil
	}
	start := sc.Dtstart
	if sc.AnchorWall != nil {
		start = *sc.AnchorWall // a re-phased rule never walks from dtstart
	}
	rows, err := s.agendaOccurrences(sc.ID, from, to)
	if err != nil {
		return nil, false, "", err
	}
	c, err := compileScheduleSpec(storedSpecInput(sc, start))
	if err == nil {
		entries, truncated, err = s.expandCompiled(c, sc, rows, view, from, to, now)
	}
	if code := ScheduleErrorCode(err); code != "" {
		return nil, false, code, nil
	}
	if err != nil {
		slog.Warn("agenda: schedule unreadable", "schedule_id", sc.ID, "error_class", fmt.Sprintf("%T", err))
		return nil, false, "internal", nil
	}
	if len(entries) > AgendaMaxPerSchedule {
		entries, truncated = entries[:AgendaMaxPerSchedule], true
	}
	return entries, truncated, "", nil
}

func (s *Store) expandCompiled(c *schedule.Compiled, sc *models.ItemSchedule, rows []agendaRow, view *time.Location, from, to, now time.Time) ([]AgendaEntry, bool, error) {
	loc := view
	if !sc.Floating {
		var err error
		if loc, err = time.LoadLocation(c.Spec().Zone); err != nil {
			return nil, false, fmt.Errorf("load schedule zone: %w", err)
		}
	}
	lo := from
	if floor := schedule.Resolve(loc, c.Spec().Start); floor.After(lo) {
		lo = floor // nothing before the walk's own start
	}
	truncated := false
	occs, err := c.Between(lo, to, view, schedule.DefaultLimits)
	var capErr *schedule.CapError
	if errors.As(err, &capErr) && capErr.Kind == schedule.CapInstances {
		truncated, err = true, nil
	}
	if err != nil {
		return nil, false, err
	}
	entries, err := s.agendaEntries(sc, occs, rows, view, loc, lo, from, to, now)
	return entries, truncated, err
}

// agendaEntries states each occurrence. loc is the zone the schedule's walls
// resolve in and lo where the walk began (the later of from and the rule's start,
// which a re-anchor moves up): history rows between from and lo are listed from
// the rows themselves, since the walk does not reach them.
func (s *Store) agendaEntries(sc *models.ItemSchedule, occs []schedule.Occurrence, rows []agendaRow, view, loc *time.Location, lo, from, to, now time.Time) ([]AgendaEntry, error) {
	byKey := map[string]agendaRow{}
	key := func(at time.Time, w schedule.Wall) string {
		if sc.Floating || w.AllDay {
			return w.String()
		}
		return at.UTC().Format(time.RFC3339)
	}
	for _, r := range rows {
		if sc.Floating || len(r.wall) == 10 {
			byKey[r.wall] = r
		} else {
			byKey[r.at] = r
		}
	}
	var o time.Time
	var hasO bool
	var err error
	if sc.EndedAt == nil {
		if o, hasO, err = walkOrigin(sc, view); err != nil {
			return nil, err
		}
	}
	isO := func(at time.Time, w schedule.Wall) bool {
		if sc.NextWall == nil || !hasO {
			return false
		}
		if sc.Floating || w.AllDay {
			return w.String() == *sc.NextWall
		}
		return at.Equal(o)
	}
	var effective *time.Time
	outstanding := func(e *AgendaEntry, r agendaRow, have bool) {
		e.State, e.Fired = "outstanding", sc.ReminderFired
		due := e.At
		if have && r.state == "moved" && sc.NextOverrideAt != nil {
			if t, err := time.Parse(time.RFC3339, *sc.NextOverrideAt); err == nil {
				e.EffectiveAt, due = &t, t
				effective = &t
			}
		}
		e.Overdue = !due.After(now)
		if n, _, err := ScheduleMissed(sc, view, now); err == nil {
			e.MissedCount, e.Stale = n, n >= 2
		}
	}
	var out []AgendaEntry
	seenO := false
	for _, occ := range occs {
		e := AgendaEntry{At: occ.At, Wall: occ.Wall}
		r, have := byKey[key(occ.At, occ.Wall)]
		if have {
			e.State, e.ResolvedAt, e.Actor, e.CommentID = r.state, r.resolvedAt, r.actor, r.commentID
		}
		switch {
		case isO(occ.At, occ.Wall):
			seenO = true
			outstanding(&e, r, have)
		case have:
		case hasO && occ.At.After(o) && !occ.At.After(now.Add(-time.Second)):
			e.State = "missed" // derived, as missed_count counts it
		case hasO && occ.At.After(o):
			e.State = "scheduled"
		default:
			e.State = "unrecorded" // never invent a state
		}
		out = append(out, e)
	}
	if !seenO && hasO && sc.NextWall != nil {
		w, err := schedule.ParseWall(*sc.NextWall)
		if err != nil {
			return nil, fmt.Errorf("parse next_wall: %w", err)
		}
		e := AgendaEntry{At: o, Wall: w}
		r, have := byKey[key(o, w)]
		if have {
			e.ResolvedAt, e.Actor, e.CommentID = r.resolvedAt, r.actor, r.commentID
		}
		if outstanding(&e, r, have); effective != nil && !effective.Before(from) && effective.Before(to) {
			out = append(out, e)
		}
	}
	for _, r := range rows {
		w, err := schedule.ParseWall(r.wall)
		if err != nil || !lo.After(from) || (sc.NextWall != nil && r.wall == *sc.NextWall) {
			continue
		}
		at, err := time.Parse(time.RFC3339, r.at)
		if sc.Floating || w.AllDay {
			at, err = schedule.Resolve(loc, w), nil
		}
		if err == nil && !at.Before(from) && at.Before(lo) && at.Before(to) {
			out = append(out, AgendaEntry{At: at, Wall: w, State: r.state, ResolvedAt: r.resolvedAt, Actor: r.actor, CommentID: r.commentID})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}
