package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/PerpetualSoftware/pad/internal/kernelevents"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
	"github.com/PerpetualSoftware/pad/internal/usertz"
)

const (
	defaultDeadlineEvalLimit = 100
	deadlineEvalPageSize     = 200
	// Covers zone differences and the end of a date-only local day.
	deadlineEvalSlop = 2 * 24 * time.Hour
)

type DeadlineEvalResult struct {
	Evaluated, Missed int
	CapHit            bool
}

type deadlineCandidate struct {
	sc             models.ItemSchedule
	days, secs     int
	zone, override *string
	approximate    float64
	key, after     string
	at             time.Time
	dateOnly       bool
}

// The occurrence wall is the seed-equivalent; target displacement is not
// subtracted from the deadline displacement.
func (d *deadlineCandidate) resolve() error {
	loc := usertz.Resolve(d.zone).Loc
	var err error
	if !d.sc.Floating {
		loc, err = time.LoadLocation(derefStr(d.sc.Tz))
		if err != nil {
			return err
		}
	}
	at, ok, err := OutstandingInstant(&d.sc, loc)
	if err != nil {
		return err
	}
	if !ok || d.sc.NextWall == nil {
		return errors.New("deadline has no outstanding occurrence")
	}
	d.key = OccurrenceKey(&d.sc, NormalizeInstant(at), *d.sc.NextWall)
	wall, err := schedule.ParseWall(*d.sc.NextWall)
	if err != nil {
		return err
	}
	civil := time.Date(wall.Year, wall.Month, wall.Day, wall.Hour, wall.Minute, wall.Second, 0, time.UTC).AddDate(0, 0, d.days).Add(time.Duration(d.secs) * time.Second)
	w := wallOf(civil)
	w.AllDay = wall.AllDay && d.secs == 0
	value := w.String()
	d.dateOnly = w.AllDay
	d.at = schedule.Resolve(loc, w).UTC()
	if d.override != nil {
		value = *d.override
		d.dateOnly = len(value) == 10
		if d.dateOnly {
			w, err = schedule.ParseWall(value)
			if err != nil {
				return err
			}
			d.at = schedule.Resolve(loc, w).UTC()
		} else {
			d.at, err = time.Parse(time.RFC3339, value)
			if err != nil {
				return err
			}
			d.at = d.at.UTC()
		}
	}
	if d.dateOnly {
		// Expire at the next local midnight, including days with a zone transition.
		civil = time.Date(w.Year, w.Month, w.Day, 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
		end := wallOf(civil)
		end.AllDay = true
		d.at = schedule.Resolve(loc, end).UTC()
	} else if d.override != nil || !d.sc.Floating {
		// A floating deadline keeps its wall as the evidence value, so a change of
		// the recipient zone cannot record the same deadline twice.
		value = d.at.Format(time.RFC3339Nano)
	}
	b, err := json.Marshal(struct {
		Deadline string `json:"deadline"`
	}{value})
	d.after = string(b)
	return err
}

func (d *deadlineCandidate) due(now time.Time) bool {
	if d.dateOnly {
		return !now.Before(d.at)
	}
	return now.After(d.at)
}

const deadlineKeySQL = `CASE WHEN sc.floating OR length(sc.next_wall)=10 THEN sc.next_wall ELSE sc.next_at || '|' || sc.next_wall END`

const deadlineFromSQL = ` FROM item_schedules sc
 JOIN items i ON i.id=sc.item_id AND i.workspace_id=sc.workspace_id
 JOIN workspaces w ON w.id=i.workspace_id
 LEFT JOIN users u ON u.id=COALESCE(NULLIF(i.assigned_user_id,''),NULLIF(i.created_by_user_id,''))
 LEFT JOIN item_reminders r ON r.schedule_id=sc.id
 LEFT JOIN schedule_occurrence_overrides o ON o.schedule_id=sc.id AND o.role='deadline' AND o.occurrence_key=` + deadlineKeySQL + `
 WHERE sc.roles_origin='native' AND sc.deadline_days IS NOT NULL AND sc.ended_at IS NULL
 AND sc.next_at IS NOT NULL AND sc.next_wall IS NOT NULL AND i.deleted_at IS NULL AND w.deleted_at IS NULL`

const deadlineColumnsSQL = `sc.id,sc.workspace_id,sc.item_id,sc.tz,sc.floating,sc.next_at,sc.next_wall,sc.next_override_at,
 CASE WHEN r.fired_at IS NOT NULL THEN 1 ELSE 0 END,sc.deadline_days,sc.deadline_secs,u.timezone,o.value_at`

func scanDeadline(row interface{ Scan(...any) error }, approximate bool) (deadlineCandidate, error) {
	var d deadlineCandidate
	var fired int
	args := []any{&d.sc.ID, &d.sc.WorkspaceID, &d.sc.ItemID, &d.sc.Tz, &d.sc.Floating, &d.sc.NextAt, &d.sc.NextWall, &d.sc.NextOverrideAt, &fired, &d.days, &d.secs, &d.zone, &d.override}
	if approximate {
		args = append(args, &d.approximate)
	}
	err := row.Scan(args...)
	d.sc.ReminderFired = fired != 0
	return d, err
}

// pgInstantShape admits exactly the RFC3339 text the Postgres cast to
// timestamptz accepts: years 0001 to 9999, real month and day ranges with leap
// years, a valid clock, and an offset up to 15:59. Anything else reads as
// unparsed (sorts first), so one bad row never fails the page query; Go
// resolves every row exactly and reports the bad one. It replaces
// pg_input_is_valid, which needs Postgres 16. TestPgInstantShapePopulation
// generates the edge cases.
const (
	pgYear         = `(000[1-9]|00[1-9][0-9]|0[1-9][0-9]{2}|[1-9][0-9]{3})`
	pgLeapYear     = `(([0-9]{2}(0[48]|[2468][048]|[13579][26]))|((0[48]|[2468][048]|[13579][26])00))`
	pgDate         = `(` + pgYear + `-((0[13578]|1[02])-(0[1-9]|[12][0-9]|3[01])|(0[469]|11)-(0[1-9]|[12][0-9]|30)|02-(0[1-9]|1[0-9]|2[0-8]))|` + pgLeapYear + `-02-29)`
	pgInstantShape = `^` + pgDate + `T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](\.[0-9]+)?(Z|[+-](0[0-9]|1[0-5]):[0-5][0-9])$`
)

func (s *Store) deadlinePage(now time.Time, cursor *deadlineCandidate) ([]deadlineCandidate, error) {
	epoch := func(col string) string { return `CAST(strftime('%s',` + col + `) AS REAL)` }
	if s.dialect.Driver() == DriverPostgres {
		epoch = func(col string) string {
			return `CASE WHEN ` + col + ` ~ '` + pgInstantShape + `' THEN CAST(EXTRACT(EPOCH FROM CAST(` + col + ` AS timestamp with time zone)) AS double precision) END`
		}
	}
	// An instant that fails the guard reads as NULL. When no other instant is
	// available the row sorts first, so Go reports the row error and the scan
	// goes on; when the schedule has a valid next_at the row is ordered by that.
	approx := `COALESCE(` + epoch("o.value_at") + `,` + epoch("sc.next_at") + `+sc.deadline_days*86400.0+sc.deadline_secs,0.0)`
	query := `WITH deadlines AS (SELECT ` + deadlineColumnsSQL + `,` + approx + ` AS approximate` + deadlineFromSQL + `)
 SELECT * FROM deadlines WHERE approximate<=?`
	args := []any{now.Add(deadlineEvalSlop).Unix()}
	if cursor != nil {
		query += ` AND (approximate>? OR (approximate=? AND id>?))`
		args = append(args, cursor.approximate, cursor.approximate, cursor.sc.ID)
	}
	query += ` ORDER BY approximate,id LIMIT ?`
	args = append(args, deadlineEvalPageSize)
	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("scan deadlines: %w", err)
	}
	defer rows.Close()
	var out []deadlineCandidate
	for rows.Next() {
		d, err := scanDeadline(rows, true)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// openDeadlineCandidates drops the candidates whose item is in a terminal status,
// judged by each workspace's own terminal options as the agenda does. A terminal
// item never shows overdue work; a reopened one is evaluated again and records
// its miss then. One query per workspace on the page.
func (s *Store) openDeadlineCandidates(q Queryer, candidates []deadlineCandidate) ([]deadlineCandidate, error) {
	byWorkspace := map[string][]string{}
	for _, d := range candidates {
		byWorkspace[d.sc.WorkspaceID] = append(byWorkspace[d.sc.WorkspaceID], d.sc.ItemID)
	}
	open := map[string]bool{}
	for workspaceID, ids := range byWorkspace {
		filter, filterArgs := s.nonTerminalFilter(workspaceID, "i")
		args := []any{workspaceID}
		for _, id := range ids {
			args = append(args, id)
		}
		args = append(args, filterArgs...)
		rows, err := q.Query(s.q(`SELECT i.id FROM items i WHERE i.workspace_id = ? AND i.id IN (`+placeholders(len(ids))+`) AND `+filter), args...)
		if err != nil {
			return nil, fmt.Errorf("deadline items: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			open[id] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	out := candidates[:0:0]
	for _, d := range candidates {
		if open[d.sc.ItemID] {
			out = append(out, d)
		}
	}
	return out, nil
}

type deadlineJournalState struct {
	recorded  bool
	converted time.Time
	err       error
}

// One journal read per page matches the current canonical deadline value and
// finds the latest conversion by parsed instant, not RFC3339Nano text order.
func (s *Store) deadlineJournal(q Queryer, candidates []deadlineCandidate) (map[string]deadlineJournalState, error) {
	states := map[string]deadlineJournalState{}
	byID := map[string]deadlineCandidate{}
	ids := make([]any, 0, len(candidates))
	for _, d := range candidates {
		ids = append(ids, d.sc.ID)
		byID[d.sc.ID] = d
	}
	if len(ids) == 0 {
		return states, nil
	}
	rows, err := q.Query(s.q(`SELECT schedule_id,COALESCE(occurrence_key,''),COALESCE(role,''),change_type,COALESCE(before_json,''),COALESCE(after_json,''),recorded_at
 FROM schedule_changes WHERE schedule_id IN (`+placeholders(len(ids))+`) AND ((change_type='missed_recorded' AND role='deadline') OR (change_type<>'missed_recorded' AND before_json LIKE '%legacy_backfill%'))`), ids...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, key, role, change, before, after, recorded string
		if err := rows.Scan(&id, &key, &role, &change, &before, &after, &recorded); err != nil {
			return nil, err
		}
		state := states[id]
		d := byID[id]
		if change == "missed_recorded" && role == "deadline" && key == d.key && after == d.after {
			state.recorded = true
		}
		var b, a scheduleDateSnapshot
		if json.Unmarshal([]byte(before), &b) == nil && json.Unmarshal([]byte(after), &a) == nil && b.Roles.Origin == "legacy_backfill" && a.Roles.Origin == "native" {
			at, err := time.Parse(time.RFC3339Nano, recorded)
			if err != nil {
				state.err = err
			} else if at.After(state.converted) {
				state.converted = at
			}
		}
		states[id] = state
	}
	return states, rows.Err()
}

func (d deadlineCandidate) eligible(now time.Time, state deadlineJournalState) bool {
	return d.due(now) && !state.recorded && (state.converted.IsZero() || d.at.After(state.converted))
}

// EvaluateDeadlines scans without locks, then pins and rechecks each due row.
// Overdue stays-owed rows are rescanned each tick: recorded rows cost reads,
// but never consume a cap slot. Only a miss recorded by this pass counts toward
// the cap, so a row that keeps failing cannot starve the rows behind it. No occurrence is closed by this pass.
func (s *Store) EvaluateDeadlines(nowTS string, limit int) (DeadlineEvalResult, error) {
	var result DeadlineEvalResult
	nowT, err := time.Parse(time.RFC3339, nowTS)
	if err != nil {
		return result, fmt.Errorf("deadline evaluation time: %w", err)
	}
	if limit <= 0 {
		limit = defaultDeadlineEvalLimit
	}
	var errs []error
	var cursor *deadlineCandidate
	for {
		page, err := s.deadlinePage(nowT, cursor)
		if err != nil {
			return result, errors.Join(append(errs, err)...)
		}
		if len(page) == 0 {
			break
		}
		cursor = &page[len(page)-1]
		resolved := make([]deadlineCandidate, 0, len(page))
		for _, d := range page {
			if err := d.resolve(); err != nil {
				errs = append(errs, fmt.Errorf("deadline %s: %w", d.sc.ID, err))
				continue
			}
			resolved = append(resolved, d)
		}
		resolved, err = s.openDeadlineCandidates(s.db, resolved)
		if err != nil {
			return result, errors.Join(append(errs, err)...)
		}
		states, err := s.deadlineJournal(s.db, resolved)
		if err != nil {
			return result, errors.Join(append(errs, err)...)
		}
		for _, d := range resolved {
			state := states[d.sc.ID]
			if state.err != nil {
				errs = append(errs, state.err)
				continue
			}
			if !d.eligible(nowT, state) {
				continue
			}
			missed, err := s.evaluateDeadline(d, nowT)
			if err != nil {
				var busy interface{ Code() int }
				if !(s.dialect.Driver() == DriverSQLite && errors.As(err, &busy) && busy.Code()&255 == 5) {
					errs = append(errs, fmt.Errorf("deadline %s: %w", d.sc.ID, err))
				}
			} else if missed {
				result.Missed++
				result.Evaluated++
			}
			if result.Evaluated == limit {
				result.CapHit = true
				return result, errors.Join(errs...)
			}
		}
		if len(page) < deadlineEvalPageSize {
			break
		}
	}
	return result, errors.Join(errs...)
}

func (s *Store) evaluateDeadline(candidate deadlineCandidate, nowT time.Time) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := s.pinScheduleTx(tx, candidate.sc.WorkspaceID, candidate.sc.ItemID); err != nil {
		if errors.Is(err, ErrScheduleItemGone) || errors.Is(err, ErrScheduleNotFound) {
			return false, nil
		}
		return false, err
	}
	d, err := scanDeadline(tx.QueryRow(s.q(`SELECT `+deadlineColumnsSQL+deadlineFromSQL+` AND sc.id=?`), candidate.sc.ID), false)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err = d.resolve(); err != nil {
		return false, err
	}
	open, err := s.openDeadlineCandidates(tx, []deadlineCandidate{d})
	if err != nil {
		return false, err
	}
	if len(open) == 0 {
		return false, nil
	}
	states, err := s.deadlineJournal(tx, []deadlineCandidate{d})
	if err != nil {
		return false, err
	}
	state := states[d.sc.ID]
	if state.err != nil {
		return false, state.err
	}
	if !d.eligible(nowT, state) {
		return false, nil
	}
	if err = s.journalScheduleChangeTx(tx, d.sc.ID, "missed_recorded", d.key, "deadline", "", d.after, "system"); err != nil {
		return false, err
	}
	item, err := s.getItemTx(tx, d.sc.ItemID)
	if err != nil {
		return false, err
	}
	if err = s.emitItemEventTx(tx, kernelevents.ItemDeadlineMissed, item, nil, ""); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
