package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Milestone forecasts and outcomes (TASK-4358). A forecast is a hand-set
// estimate that lives beside the schedule, never in a role: it arms no reminder
// and nothing in the deadline evaluator reads it. A milestone's outcome is
// derived on read from explicitly selected children (a child that closed
// delivered, or an explicit waiver with a reason); nothing here resolves an
// occurrence or touches an item's status.

const (
	maxMilestoneChildren = 200
	maxForecastSource    = 280
)

// ErrMilestoneChildNotFound is the answer when the named child is not an item of
// the milestone's workspace. The HTTP layer decides it first, with the same
// refusal for a missing item and one the caller cannot read.
var ErrMilestoneChildNotFound = errors.New("milestone child not found in this workspace")

// canonForecastValue is the one canonicaliser for a forecast value, used by the
// write door and by an import: a date (YYYY-MM-DD) or an RFC3339 instant,
// stored as a date or as UTC.
func canonForecastValue(v string) (string, error) {
	if strings.TrimSpace(v) == "" {
		return "", fieldErr("value", "required", "a forecast needs a value")
	}
	if hasNUL(v) {
		return "", fieldErr("value", "format", "value cannot contain a NUL")
	}
	if len(v) == 10 {
		if _, err := time.Parse("2006-01-02", v); err != nil {
			return "", fieldErr("value", "format", "value must be a date (YYYY-MM-DD) or an RFC3339 instant")
		}
		return v, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return "", fieldErr("value", "format", "value must be a date (YYYY-MM-DD) or an RFC3339 instant")
	}
	return t.UTC().Format(time.RFC3339), nil
}

func canonForecastSource(v string) error {
	if hasNUL(v) {
		return fieldErr("source", "format", "source cannot contain a NUL")
	}
	if len([]rune(v)) > maxForecastSource {
		return fieldErr("source", "range", fmt.Sprintf("source is at most %d characters", maxForecastSource))
	}
	return nil
}

func actorKindOf(kind string) string {
	if kind == "agent" {
		return "agent"
	}
	return "user"
}

// SetForecast stores the schedule's forecast, replacing the current one, and
// appends a history event. It touches no role, override or reminder row, so a
// forecast with no target is fine and a past one is never a miss.
func (s *Store) SetForecast(workspaceID, itemID, actor, actorKind, value, source string) error {
	v, err := canonForecastValue(value)
	if err != nil {
		return err
	}
	if err := canonForecastSource(source); err != nil {
		return err
	}
	kind := actorKindOf(actorKind)
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("set forecast: %w", err)
	}
	defer tx.Rollback()
	if err := s.pinScheduleTx(tx, workspaceID, itemID); err != nil {
		return err
	}
	sc, err := s.getScheduleQ(tx, workspaceID, itemID, false)
	if err != nil || sc == nil {
		return firstErr(err, ErrScheduleNotFound)
	}
	prev := ""
	switch err := tx.QueryRow(s.q(`SELECT value FROM schedule_forecasts WHERE schedule_id = ?`), sc.ID).Scan(&prev); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("read forecast: %w", err)
	}
	ts := nowNano()
	if _, err := tx.Exec(s.q(`INSERT INTO schedule_forecasts (schedule_id,workspace_id,item_id,value,set_by,set_by_kind,set_at,source)
 VALUES (?,?,?,?,?,?,?,?)
 ON CONFLICT (schedule_id) DO UPDATE SET value=excluded.value,set_by=excluded.set_by,set_by_kind=excluded.set_by_kind,set_at=excluded.set_at,source=excluded.source`),
		sc.ID, workspaceID, itemID, v, actor, kind, ts, source); err != nil {
		return fmt.Errorf("write forecast: %w", err)
	}
	if err := s.forecastEventTx(tx, workspaceID, itemID, sc.ID, "set", v, prev, actor, kind, source, ts); err != nil {
		return err
	}
	return tx.Commit()
}

// ClearForecast removes the forecast and records the clear. Clearing an absent
// forecast writes nothing.
func (s *Store) ClearForecast(workspaceID, itemID, actor, actorKind string) error {
	kind := actorKindOf(actorKind)
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("clear forecast: %w", err)
	}
	defer tx.Rollback()
	if err := s.pinScheduleTx(tx, workspaceID, itemID); err != nil {
		return err
	}
	sc, err := s.getScheduleQ(tx, workspaceID, itemID, false)
	if err != nil || sc == nil {
		return firstErr(err, ErrScheduleNotFound)
	}
	var prev string
	switch err := tx.QueryRow(s.q(`SELECT value FROM schedule_forecasts WHERE schedule_id = ?`), sc.ID).Scan(&prev); {
	case errors.Is(err, sql.ErrNoRows):
		return tx.Commit()
	case err != nil:
		return fmt.Errorf("read forecast: %w", err)
	}
	if _, err := tx.Exec(s.q(`DELETE FROM schedule_forecasts WHERE schedule_id = ?`), sc.ID); err != nil {
		return fmt.Errorf("clear forecast: %w", err)
	}
	if err := s.forecastEventTx(tx, workspaceID, itemID, sc.ID, "clear", "", prev, actor, kind, "", nowNano()); err != nil {
		return err
	}
	return tx.Commit()
}

func firstErr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

func (s *Store) forecastEventTx(tx *sql.Tx, workspaceID, itemID, scheduleID, action, value, prev, actor, kind, source, ts string) error {
	if _, err := tx.Exec(s.q(`INSERT INTO schedule_forecast_events (id,workspace_id,item_id,schedule_id,action,value,prev_value,set_by,set_by_kind,source,recorded_at)
 VALUES (?,?,?,?,?,?,?,?,?,?,?)`), newID(), workspaceID, itemID, scheduleID, action, value, prev, actor, kind, source, ts); err != nil {
		return fmt.Errorf("record forecast event: %w", err)
	}
	return nil
}

// SetMilestoneChild selects a child (kind "child") or waives it (kind "waiver",
// reason required) on the item's schedule. Selecting a selected child updates
// it in place; a child that was removed is selected again as a new row, so the
// removal stays in the history. The caller has already decided that the actor
// may read the child.
func (s *Store) SetMilestoneChild(workspaceID, itemID, childID, kind, reason, actor string) error {
	switch {
	case kind == "":
		return fieldErr("kind", "required", "kind is required")
	case !oneOf(kind, []string{"child", "waiver"}):
		return fieldErr("kind", "enum", "kind must be child or waiver")
	case hasNUL(reason):
		return fieldErr("reason", "format", "reason cannot contain a NUL")
	case kind == "waiver" && strings.TrimSpace(reason) == "":
		return fieldErr("reason", "combination", "a waiver needs a reason")
	case childID == itemID:
		return fieldErr("child", "combination", "a milestone cannot select itself")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("set milestone child: %w", err)
	}
	defer tx.Rollback()
	if err := s.pinScheduleTx(tx, workspaceID, itemID); err != nil {
		return err
	}
	sc, err := s.getScheduleQ(tx, workspaceID, itemID, false)
	if err != nil || sc == nil {
		return firstErr(err, ErrScheduleNotFound)
	}
	var one int
	switch err := tx.QueryRow(s.q(`SELECT 1 FROM items WHERE id = ? AND workspace_id = ?`), childID, workspaceID).Scan(&one); {
	case errors.Is(err, sql.ErrNoRows):
		return ErrMilestoneChildNotFound
	case err != nil:
		return fmt.Errorf("check milestone child: %w", err)
	}
	res, err := tx.Exec(s.q(`UPDATE milestone_children SET kind = ?, reason = ? WHERE schedule_id = ? AND child_item_id = ? AND removed_at IS NULL`),
		kind, reason, sc.ID, childID)
	if err != nil {
		return fmt.Errorf("update milestone child: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		var active int
		if err := tx.QueryRow(s.q(`SELECT COUNT(*) FROM milestone_children WHERE schedule_id = ? AND removed_at IS NULL`), sc.ID).Scan(&active); err != nil {
			return fmt.Errorf("count milestone children: %w", err)
		}
		if active >= maxMilestoneChildren {
			return fieldErr("children", "range", fmt.Sprintf("a milestone selects at most %d children", maxMilestoneChildren))
		}
		if _, err := tx.Exec(s.q(`INSERT INTO milestone_children (id,workspace_id,schedule_id,child_item_id,kind,reason,added_by,added_at) VALUES (?,?,?,?,?,?,?,?)`),
			newID(), workspaceID, sc.ID, childID, kind, reason, actor, nowNano()); err != nil {
			return fmt.Errorf("select milestone child: %w", err)
		}
	}
	return tx.Commit()
}

// RemoveMilestoneChild soft-removes a selection: the row stays as history.
// Removing a child that is not selected writes nothing.
func (s *Store) RemoveMilestoneChild(workspaceID, itemID, childID, actor string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("remove milestone child: %w", err)
	}
	defer tx.Rollback()
	if err := s.pinScheduleTx(tx, workspaceID, itemID); err != nil {
		return err
	}
	sc, err := s.getScheduleQ(tx, workspaceID, itemID, false)
	if err != nil || sc == nil {
		return firstErr(err, ErrScheduleNotFound)
	}
	if _, err := tx.Exec(s.q(`UPDATE milestone_children SET removed_by = ?, removed_at = ? WHERE schedule_id = ? AND child_item_id = ? AND removed_at IS NULL`),
		actor, nowNano(), sc.ID, childID); err != nil {
		return fmt.Errorf("remove milestone child: %w", err)
	}
	return tx.Commit()
}

// --- the reader --------------------------------------------------------------

// msChild is one selection row with its child as it stands now.
type msChild struct {
	ChildID, Kind, Reason, AddedBy, AddedAt, RemovedBy, RemovedAt string
	Ref, State, DoneValue                                         string
	terminals                                                     []string
}

// msEvidence is one completion-evidence row. Gone is a child that no longer
// exists at all.
type msEvidence struct {
	OccurrenceKey, ChildID, Kind, State, DoneValue, RecordedAt string
	Seq                                                        int64
	Ref                                                        string
	Gone                                                       bool
	terminals                                                  []string
}

// msRows is everything the two milestone tables hold for one schedule that the
// reader may see. Hidden is true when any selected child, or any child in the
// latest completion snapshot, is one the reader cannot read; those rows are
// absent from the lists and counted nowhere.
type msRows struct {
	Active, Removed []msChild
	Evidence        []msEvidence
	Hidden          bool
}

func itemRefOf(slug sql.NullString, number sql.NullInt64, prefix sql.NullString) string {
	if number.Valid && prefix.Valid {
		return formatRef(prefix.String, int(number.Int64))
	}
	return slug.String
}

// childStateOf classifies an item against its collection exactly as the
// reports do (models.ItemCloseState): a trashed item is missing, an abandoned
// one is closed without delivering.
func childStateOf(fields, schema, settings sql.NullString, deleted bool) (state, doneValue string, terminals []string) {
	if deleted {
		return "missing", "", nil
	}
	var f map[string]any
	if fields.Valid && fields.String != "" {
		_ = json.Unmarshal([]byte(fields.String), &f)
	}
	var sch models.CollectionSchema
	if schema.Valid && schema.String != "" {
		_ = models.UnmarshalItemFieldSchema([]byte(schema.String), &sch)
	}
	var set models.CollectionSettings
	if settings.Valid && settings.String != "" {
		_ = json.Unmarshal([]byte(settings.String), &set)
	}
	key, terms := models.TerminalValuesForDoneField(sch, set)
	switch models.ItemCloseState(f, sch, set) {
	case models.CloseStateDone:
		if v, ok := f[key].(string); ok {
			doneValue = v
		}
		return "done", doneValue, terms
	case models.CloseStateAbandoned:
		return "abandoned", "", terms
	}
	return "open", "", terms
}

// loadMilestoneRows is the ONLY function that reads milestone_children and
// milestone_completion_evidence, and the one place a reader's visibility is
// applied: every caller (the commitment read, its history, the export, the
// carry counts, the evidence snapshot) goes through it. canRead nil means the
// caller reads everything (an export, an internal snapshot, a store test);
// otherwise a child the reader cannot read is dropped from every list and sets
// Hidden. Visibility is decided now, for this reader, on snapshot rows too.
// allEvidence reads every snapshot instead of the latest.
func (s *Store) loadMilestoneRows(q Queryer, workspaceID, scheduleID string, canRead func(string) bool, allEvidence bool) (*msRows, error) {
	out := &msRows{}
	readable := func(id string) bool { return canRead == nil || canRead(id) }
	rows, err := q.Query(s.q(`SELECT mc.child_item_id,mc.kind,mc.reason,mc.added_by,mc.added_at,mc.removed_by,mc.removed_at,
 i.slug,i.item_number,c.prefix,i.fields,i.deleted_at,c.schema,c.settings
 FROM milestone_children mc JOIN items i ON i.id = mc.child_item_id JOIN collections c ON c.id = i.collection_id
 WHERE mc.schedule_id = ? AND mc.workspace_id = ? ORDER BY mc.added_at, mc.id`), scheduleID, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("read milestone children: %w", err)
	}
	for rows.Next() {
		var m msChild
		var rb, ra, slug, prefix, fields, deletedAt, schema, settings sql.NullString
		var number sql.NullInt64
		if err := rows.Scan(&m.ChildID, &m.Kind, &m.Reason, &m.AddedBy, &m.AddedAt, &rb, &ra, &slug, &number, &prefix, &fields, &deletedAt, &schema, &settings); err != nil {
			rows.Close()
			return nil, err
		}
		m.RemovedBy, m.RemovedAt = rb.String, ra.String
		if !readable(m.ChildID) {
			if !ra.Valid {
				out.Hidden = true
			}
			continue
		}
		m.Ref = itemRefOf(slug, number, prefix)
		m.State, m.DoneValue, m.terminals = childStateOf(fields, schema, settings, deletedAt.Valid)
		if ra.Valid {
			out.Removed = append(out.Removed, m)
		} else {
			out.Active = append(out.Active, m)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	query := `SELECT e.occurrence_key,e.child_item_id,e.kind,e.close_state,e.done_value,e.recorded_at,e.transition_seq,
 i.id,i.slug,i.item_number,c.prefix,i.fields,i.deleted_at,c.schema,c.settings
 FROM milestone_completion_evidence e LEFT JOIN items i ON i.id = e.child_item_id LEFT JOIN collections c ON c.id = i.collection_id
 WHERE e.schedule_id = ? AND e.workspace_id = ?`
	args := []any{scheduleID, workspaceID}
	if !allEvidence {
		var key string
		switch err := q.QueryRow(s.q(`SELECT occurrence_key FROM milestone_completion_evidence WHERE schedule_id = ? AND workspace_id = ? ORDER BY recorded_at DESC, id DESC LIMIT 1`),
			scheduleID, workspaceID).Scan(&key); {
		case errors.Is(err, sql.ErrNoRows):
			return out, nil
		case err != nil:
			return nil, fmt.Errorf("read completion evidence: %w", err)
		}
		query += ` AND e.occurrence_key = ?`
		args = append(args, key)
	}
	erows, err := q.Query(s.q(query+` ORDER BY e.recorded_at, e.id`), args...)
	if err != nil {
		return nil, fmt.Errorf("read completion evidence: %w", err)
	}
	defer erows.Close()
	for erows.Next() {
		var e msEvidence
		var id, slug, prefix, fields, deletedAt, schema, settings sql.NullString
		var number sql.NullInt64
		if err := erows.Scan(&e.OccurrenceKey, &e.ChildID, &e.Kind, &e.State, &e.DoneValue, &e.RecordedAt, &e.Seq, &id, &slug, &number, &prefix, &fields, &deletedAt, &schema, &settings); err != nil {
			return nil, err
		}
		if !id.Valid {
			e.Gone = true
		} else if !readable(e.ChildID) {
			out.Hidden = true
			continue
		} else {
			e.Ref = itemRefOf(slug, number, prefix)
			_, _, e.terminals = childStateOf(fields, schema, settings, deletedAt.Valid)
		}
		out.Evidence = append(out.Evidence, e)
	}
	return out, erows.Err()
}

func isTerminalValue(terminals []string, v string) bool {
	for _, t := range terminals {
		if strings.EqualFold(t, v) {
			return true
		}
	}
	return false
}

// deriveMilestone is the outcome for the rows a reader may see. It resolves
// nothing: a completed result is a reading, not a state.
func (s *Store) deriveMilestone(q Queryer, rows *msRows, completedAt string) (models.CommitmentMilestone, error) {
	m := models.CommitmentMilestone{Children: []models.MilestoneChild{}, Changes: []models.MilestoneChange{},
		Evidence: []models.MilestoneEvidence{}, History: []models.MilestoneHistoryRow{}}
	if completedAt != "" {
		m.CompletedAt = &completedAt
	}
	live := map[string]msChild{}
	for _, a := range rows.Active {
		counts := (a.Kind == "child" && a.State == "done") || a.Kind == "waiver"
		m.Children = append(m.Children, models.MilestoneChild{ItemRef: a.Ref, Kind: a.Kind, CloseState: a.State, DoneValue: a.DoneValue,
			Counts: counts, Waived: a.Kind == "waiver", Reason: a.Reason, AddedBy: a.AddedBy, AddedAt: a.AddedAt})
		live[a.ChildID] = a
		m.Total++
		if counts {
			m.Counted++
		}
		if a.Kind == "waiver" {
			m.Waivers++
		}
	}
	for _, r := range rows.Removed {
		m.History = append(m.History, models.MilestoneHistoryRow{ItemRef: r.Ref, Kind: r.Kind, Reason: r.Reason, AddedBy: r.AddedBy,
			AddedAt: r.AddedAt, RemovedBy: r.RemovedBy, RemovedAt: r.RemovedAt})
	}
	for _, e := range rows.Evidence {
		if !e.Gone {
			m.Evidence = append(m.Evidence, models.MilestoneEvidence{ItemRef: e.Ref, Kind: e.Kind, CloseState: e.State, DoneValue: e.DoneValue, RecordedAt: e.RecordedAt})
		}
	}
	var result string
	switch {
	case rows.Hidden:
		result = "unknown"
	case m.Total == 0:
	case m.Counted == m.Total:
		result = "completed"
	default:
		result = "pending"
	}
	if result != "" {
		m.Result = &result
	}
	if rows.Hidden {
		return m, nil
	}
	changed := false
	if len(rows.Evidence) > 0 {
		snap := map[string]msEvidence{}
		for _, e := range rows.Evidence {
			snap[e.ChildID] = e
			if e.Gone {
				changed = true
			}
		}
		add := func(ref, was, now string, reopened bool) {
			changed = true
			m.Changes = append(m.Changes, models.MilestoneChange{ItemRef: ref, Was: was, Now: now, Reopened: reopened})
		}
		reopened, err := s.reopenedAfterEvidence(q, rows.Evidence)
		if err != nil {
			return m, err
		}
		for _, e := range rows.Evidence {
			if e.Gone {
				continue
			}
			a, ok := live[e.ChildID]
			switch {
			case !ok:
				add(e.Ref, e.State, "unselected", reopened[e.ChildID])
			case a.Kind != e.Kind || a.State != e.State || reopened[e.ChildID]:
				add(e.Ref, e.State, a.State, reopened[e.ChildID])
			}
		}
		for _, a := range rows.Active {
			if _, ok := snap[a.ChildID]; !ok {
				add(a.Ref, "unselected", a.State, false)
			}
		}
	}
	m.EvidenceChanged = &changed
	return m, nil
}

// reopenedAfterEvidence names the snapshot children that went from a terminal
// to a non-terminal done-field value after their evidence was recorded. A child
// reopened and closed again reads the same as before; this is what still shows
// the reopen. status_transitions is written by item create, update, move and
// the fenced writers; the bundle importer and any direct SQL do not write it.
func (s *Store) reopenedAfterEvidence(q Queryer, ev []msEvidence) (map[string]bool, error) {
	out := map[string]bool{}
	byID := map[string]msEvidence{}
	var ids []string
	var floor int64 = -1
	for _, e := range ev {
		if e.Gone {
			continue
		}
		byID[e.ChildID] = e
		ids = append(ids, e.ChildID)
		if floor < 0 || e.Seq < floor {
			floor = e.Seq
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	args := []any{floor}
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := q.Query(s.q(`SELECT item_id,from_status,to_status,seq FROM status_transitions WHERE seq > ? AND item_id IN (`+placeholders(len(ids))+`)`), args...)
	if err != nil {
		return nil, fmt.Errorf("read status transitions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, from, to string
		var seq int64
		if err := rows.Scan(&id, &from, &to, &seq); err != nil {
			return nil, err
		}
		e := byID[id]
		if seq > e.Seq && isTerminalValue(e.terminals, from) && !isTerminalValue(e.terminals, to) {
			out[id] = true
		}
	}
	return out, rows.Err()
}

// snapshotMilestoneEvidenceTx records the active selection as it stands when an
// occurrence resolves done. A schedule with no selection pays one query.
func (s *Store) snapshotMilestoneEvidenceTx(tx *sql.Tx, workspaceID, scheduleID, key string) error {
	rows, err := s.loadMilestoneRows(tx, workspaceID, scheduleID, nil, false)
	if err != nil || len(rows.Active) == 0 {
		return err
	}
	ids := make([]string, 0, len(rows.Active))
	for _, a := range rows.Active {
		ids = append(ids, a.ChildID)
	}
	seqs, err := s.maxTransitionSeqs(tx, ids)
	if err != nil {
		return err
	}
	ts := nowNano()
	for _, a := range rows.Active {
		if _, err := tx.Exec(s.q(`INSERT INTO milestone_completion_evidence (id,workspace_id,schedule_id,occurrence_key,child_item_id,kind,close_state,done_value,transition_seq,recorded_at)
 VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`), newID(), workspaceID, scheduleID, key, a.ChildID, a.Kind, a.State, a.DoneValue, seqs[a.ChildID], ts); err != nil {
			return fmt.Errorf("record completion evidence: %w", err)
		}
	}
	return nil
}

func (s *Store) maxTransitionSeqs(q Queryer, ids []string) (map[string]int64, error) {
	out := map[string]int64{}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := q.Query(s.q(`SELECT item_id, MAX(seq) FROM status_transitions WHERE item_id IN (`+placeholders(len(ids))+`) GROUP BY item_id`), args...)
	if err != nil {
		return nil, fmt.Errorf("read transition marks: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var seq sql.NullInt64
		if err := rows.Scan(&id, &seq); err != nil {
			return nil, err
		}
		out[id] = seq.Int64
	}
	return out, rows.Err()
}

// readForecast is the forecast and its events; nil when none was ever set or
// the last act was a clear.
func (s *Store) readForecast(q Queryer, scheduleID string, loc *time.Location, nowT time.Time) (*models.CommitmentForecast, error) {
	f := &models.CommitmentForecast{History: []models.ForecastEvent{}}
	err := q.QueryRow(s.q(`SELECT value,set_by,set_by_kind,set_at,source FROM schedule_forecasts WHERE schedule_id = ?`), scheduleID).
		Scan(&f.Value, &f.SetBy, &f.SetByKind, &f.SetAt, &f.Source)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read forecast: %w", err)
	}
	events, err := s.forecastEvents(q, scheduleID)
	if err != nil {
		return nil, err
	}
	f.History = events
	if len(f.Value) == 10 {
		f.Passed = nowT.In(loc).Format("2006-01-02") > f.Value
	} else if t, err := time.Parse(time.RFC3339, f.Value); err == nil {
		f.Passed = nowT.After(t)
	}
	return f, nil
}

func (s *Store) forecastEvents(q Queryer, scheduleID string) ([]models.ForecastEvent, error) {
	rows, err := q.Query(s.q(`SELECT id,action,value,prev_value,set_by,set_by_kind,source,recorded_at FROM schedule_forecast_events WHERE schedule_id = ? ORDER BY recorded_at, id`), scheduleID)
	if err != nil {
		return nil, fmt.Errorf("read forecast events: %w", err)
	}
	defer rows.Close()
	out := []models.ForecastEvent{}
	for rows.Next() {
		var e models.ForecastEvent
		if err := rows.Scan(&e.ID, &e.Action, &e.Value, &e.PrevValue, &e.SetBy, &e.SetByKind, &e.Source, &e.RecordedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// completedAtOf is when the schedule's latest done occurrence was completed, ""
// when none was.
func (s *Store) completedAtOf(q Queryer, scheduleID string) (string, error) {
	var at sql.NullString
	err := q.QueryRow(s.q(`SELECT COALESCE(actual_done_at, resolved_at) FROM schedule_occurrences WHERE schedule_id = ? AND state = 'done' ORDER BY resolved_at DESC LIMIT 1`), scheduleID).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return at.String, err
}

// milestoneForRead fills the commitment's forecast and milestone members for
// one reader.
func (s *Store) milestoneForRead(q Queryer, out *models.Commitment, sc *models.ItemSchedule, canRead func(string) bool) error {
	loc := time.UTC
	if l, err := s.scheduleViewZone(q, sc); err == nil {
		loc = l
	}
	var err error
	if out.Forecast, err = s.readForecast(q, sc.ID, loc, time.Now()); err != nil {
		return err
	}
	rows, err := s.loadMilestoneRows(q, sc.WorkspaceID, sc.ID, canRead, false)
	if err != nil {
		return err
	}
	completed := ""
	if len(rows.Evidence) > 0 || len(rows.Active) > 0 || rows.Hidden {
		if completed, err = s.completedAtOf(q, sc.ID); err != nil {
			return err
		}
	}
	out.Milestone, err = s.deriveMilestone(q, rows, completed)
	return err
}

// MilestoneChildIDs lists the active selection's child ids so a caller can ask
// which of them its reader may see before a transaction opens. It feeds a
// visibility filter and is never a response.
func (s *Store) MilestoneChildIDs(workspaceID, itemID string) ([]string, error) {
	sc, err := s.GetSchedule(workspaceID, itemID)
	if err != nil || sc == nil {
		return nil, err
	}
	rows, err := s.loadMilestoneRows(s.db, workspaceID, sc.ID, nil, false)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(rows.Active))
	for _, a := range rows.Active {
		ids = append(ids, a.ChildID)
	}
	sort.Strings(ids)
	return ids, nil
}

// carryDropCounts is what a copy or cross-workspace move leaves behind: the
// forecast and the selection the actor can see.
func (s *Store) carryDropCounts(q Queryer, workspaceID, scheduleID string, canRead func(string) bool) (forecast, children int, err error) {
	if err = q.QueryRow(s.q(`SELECT COUNT(*) FROM schedule_forecasts WHERE schedule_id = ?`), scheduleID).Scan(&forecast); err != nil {
		return 0, 0, fmt.Errorf("count forecast: %w", err)
	}
	rows, err := s.loadMilestoneRows(q, workspaceID, scheduleID, canRead, false)
	if err != nil {
		return 0, 0, err
	}
	return forecast, len(rows.Active), nil
}

// --- bundle export and import -------------------------------------------------

// exportMilestoneInto fills a schedule's export with its forecast, forecast
// events, selection (active and removed) and completion evidence. An export is
// the workspace's own full copy, so it reads everything (canRead nil).
func (s *Store) exportMilestoneInto(q Queryer, workspaceID string, x *models.ScheduleExport) error {
	var f models.ForecastExport
	switch err := q.QueryRow(s.q(`SELECT value,set_by,set_by_kind,set_at,source FROM schedule_forecasts WHERE schedule_id = ?`), x.ID).
		Scan(&f.Value, &f.SetBy, &f.SetByKind, &f.SetAt, &f.Source); {
	case err == nil:
		x.Forecast = &f
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("export forecast: %w", err)
	}
	events, err := s.forecastEvents(q, x.ID)
	if err != nil {
		return err
	}
	for i := range events {
		events[i].ID = ""
	}
	if len(events) > 0 {
		x.ForecastEvents = events
	}
	rows, err := s.loadMilestoneRows(q, workspaceID, x.ID, nil, true)
	if err != nil {
		return err
	}
	for _, c := range append(append([]msChild{}, rows.Active...), rows.Removed...) {
		x.Children = append(x.Children, models.MilestoneChildExport{ItemID: c.ChildID, Kind: c.Kind, Reason: c.Reason,
			AddedBy: c.AddedBy, AddedAt: c.AddedAt, RemovedBy: c.RemovedBy, RemovedAt: c.RemovedAt})
	}
	for _, e := range rows.Evidence {
		x.CompletionEvidence = append(x.CompletionEvidence, models.MilestoneEvidenceExport{OccurrenceKey: e.OccurrenceKey, ItemID: e.ChildID,
			Kind: e.Kind, CloseState: e.State, DoneValue: e.DoneValue, RecordedAt: e.RecordedAt})
	}
	return nil
}

func importStamp(v string) (string, bool) {
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return "", false
	}
	return t.UTC().Format(time.RFC3339Nano), true
}

// importMilestoneTx stores a bundle schedule's forecast and milestone members on
// the schedule just created. Everything is validated before the first insert (a
// failed statement poisons a Postgres transaction); a member that fails is
// dropped and its reason returned, never trusted. A child whose item was not
// imported is dropped with the reason children:orphan.
func (s *Store) importMilestoneTx(tx *sql.Tx, workspaceID, scheduleID, itemID string, floating bool, sx models.ScheduleExport, mapChild func(string) (string, bool)) ([]string, error) {
	var drops []string
	type fx struct{ value, setBy, kind, setAt, source string }
	var forecast *fx
	if f := sx.Forecast; f != nil {
		v, err := canonForecastValue(f.Value)
		at, okAt := importStamp(f.SetAt)
		if err != nil || !okAt || !oneOf(f.SetByKind, []string{"user", "agent"}) || hasNUL(f.SetBy) || canonForecastSource(f.Source) != nil {
			drops = append(drops, "forecast:invalid")
		} else {
			forecast = &fx{v, f.SetBy, f.SetByKind, at, f.Source}
		}
	}
	var events []models.ForecastEvent
	eventsOK := true
	for _, e := range sx.ForecastEvents {
		at, okAt := importStamp(e.RecordedAt)
		bad := !okAt || !oneOf(e.Action, []string{"set", "clear"}) || !oneOf(e.SetByKind, []string{"user", "agent"}) || hasNUL(e.SetBy, e.PrevValue) || canonForecastSource(e.Source) != nil
		if !bad && e.Action == "set" {
			var err error
			e.Value, err = canonForecastValue(e.Value)
			bad = err != nil
		}
		if !bad && e.PrevValue != "" {
			var err error
			e.PrevValue, err = canonForecastValue(e.PrevValue)
			bad = err != nil
		}
		if bad {
			eventsOK = false
			break
		}
		e.RecordedAt = at
		events = append(events, e)
	}
	if !eventsOK {
		events = nil
		drops = append(drops, "forecast_events:invalid")
	}

	type child struct {
		id, kind, reason, addedBy, addedAt, removedBy, removedAt string
	}
	var children []child
	childrenOK, orphan, active := true, false, 0
	seen := map[string]bool{}
	for _, c := range sx.Children {
		id, ok := mapChild(c.ItemID)
		if !ok {
			orphan = true
			continue
		}
		added, okA := importStamp(c.AddedAt)
		removed := ""
		okR := true
		if c.RemovedAt != "" {
			removed, okR = importStamp(c.RemovedAt)
		}
		bad := !okA || !okR || !oneOf(c.Kind, []string{"child", "waiver"}) || hasNUL(c.Reason, c.AddedBy, c.RemovedBy) ||
			(c.Kind == "waiver" && strings.TrimSpace(c.Reason) == "") || id == itemID
		if c.RemovedAt == "" {
			active++
			bad = bad || seen[id]
			seen[id] = true
		}
		if bad {
			childrenOK = false
			break
		}
		children = append(children, child{id, c.Kind, c.Reason, c.AddedBy, added, c.RemovedBy, removed})
	}
	if active > maxMilestoneChildren {
		childrenOK = false
	}
	if !childrenOK {
		children = nil
		drops = append(drops, "children:invalid")
	} else if orphan {
		drops = append(drops, "children:orphan")
	}

	type evid struct{ key, id, kind, state, done, at string }
	var evidence []evid
	evidenceOK := true
	for _, e := range sx.CompletionEvidence {
		id, ok := mapChild(e.ItemID)
		if !ok {
			continue
		}
		key, err := canonicalCarriedKey(e.OccurrenceKey, floating)
		at, okAt := importStamp(e.RecordedAt)
		if err != nil || !okAt || !oneOf(e.Kind, []string{"child", "waiver"}) || !oneOf(e.CloseState, []string{"open", "done", "abandoned", "missing"}) || hasNUL(e.DoneValue) {
			evidenceOK = false
			break
		}
		evidence = append(evidence, evid{key, id, e.Kind, e.CloseState, e.DoneValue, at})
	}
	if !evidenceOK {
		evidence = nil
		drops = append(drops, "completion_evidence:invalid")
	}

	if forecast != nil {
		if _, err := tx.Exec(s.q(`INSERT INTO schedule_forecasts (schedule_id,workspace_id,item_id,value,set_by,set_by_kind,set_at,source) VALUES (?,?,?,?,?,?,?,?)`),
			scheduleID, workspaceID, itemID, forecast.value, forecast.setBy, forecast.kind, forecast.setAt, forecast.source); err != nil {
			return nil, fmt.Errorf("import forecast: %w", err)
		}
	}
	for _, e := range events {
		if _, err := tx.Exec(s.q(`INSERT INTO schedule_forecast_events (id,workspace_id,item_id,schedule_id,action,value,prev_value,set_by,set_by_kind,source,recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`),
			newID(), workspaceID, itemID, scheduleID, e.Action, e.Value, e.PrevValue, e.SetBy, e.SetByKind, e.Source, e.RecordedAt); err != nil {
			return nil, fmt.Errorf("import forecast event: %w", err)
		}
	}
	for _, c := range children {
		if _, err := tx.Exec(s.q(`INSERT INTO milestone_children (id,workspace_id,schedule_id,child_item_id,kind,reason,added_by,added_at,removed_by,removed_at) VALUES (?,?,?,?,?,?,?,?,?,?)`),
			newID(), workspaceID, scheduleID, c.id, c.kind, c.reason, c.addedBy, c.addedAt, nullIfEmpty(c.removedBy), nullIfEmpty(c.removedAt)); err != nil {
			return nil, fmt.Errorf("import milestone child: %w", err)
		}
	}
	if len(evidence) > 0 {
		ids := make([]string, 0, len(evidence))
		for _, e := range evidence {
			ids = append(ids, e.id)
		}
		seqs, err := s.maxTransitionSeqs(tx, ids)
		if err != nil {
			return nil, err
		}
		for _, e := range evidence {
			if _, err := tx.Exec(s.q(`INSERT INTO milestone_completion_evidence (id,workspace_id,schedule_id,occurrence_key,child_item_id,kind,close_state,done_value,transition_seq,recorded_at)
 VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`), newID(), workspaceID, scheduleID, e.key, e.id, e.kind, e.state, e.done, seqs[e.id], e.at); err != nil {
				return nil, fmt.Errorf("import completion evidence: %w", err)
			}
		}
	}
	return drops, nil
}
