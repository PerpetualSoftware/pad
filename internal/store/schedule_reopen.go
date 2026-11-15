package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// ReopenError is shared by REST, CLI and both MCP transports. Dependency
// refusals deliberately contain no information about another item.
type ReopenError struct{ Code string }

func (e *ReopenError) Error() string {
	switch e.Code {
	case "reopen_unavailable":
		return "No saved resolution is available to reopen."
	case "reopen_not_supported":
		return "This resolution cannot be reopened."
	case "reopen_dependency":
		return "This resolution cannot be reopened because of schedule dependencies."
	case "reopen_deadline_passed":
		return "This occurrence cannot be reopened because its deadline has passed."
	default:
		return "The schedule changed after this resolution; reload it before continuing."
	}
}

type ReopenInput struct {
	ResolutionID string
	Actor        string
	ActorUserID  string
	Activity     models.Activity
}

type ReopenResult struct {
	ResolutionID  string `json:"resolution_id"`
	ChangeID      string `json:"change_id"`
	OccurrenceKey string `json:"occurrence_key"`
	PreviousState string `json:"previous_state"`
	MissedRemoved int    `json:"missed_removed"`
	CommentID     string `json:"comment_id,omitempty"`
}

// Row images are local, versioned execution evidence, never part of a bundle.
// Values stay textual (or NULL) so integer sequences survive JSON round trips.
type reopenRow map[string]*string
type reopenOccurrenceIdentity struct{ At, Wall string }
type reopenImage struct {
	Schedule  string
	UpdatedAt string
	Recipient string
	Rows      map[string][]reopenRow
	Context   map[string][]reopenRow
}
type resolutionUndo struct {
	ID, Key, Operation, Actor, ActorUserID, CommentID string
	ScheduleID                                        string
	ItemSeq                                           int64
	BlockedCode                                       string
	Before                                            reopenImage
	After                                             reopenImage
	Identities                                        []reopenOccurrenceIdentity
	JournalIDs                                        []string
}

// Keep full evidence in the journal, but apply the same child visibility used
// by the milestone reader before returning that evidence to a caller.
func reopenAuditForRead(raw string, canRead func(string) bool) (string, error) {
	var audit map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &audit); err != nil {
		return "", err
	}
	var removed map[string][]reopenRow
	if body := audit["removed_effects"]; body != nil {
		if err := json.Unmarshal(body, &removed); err != nil {
			return "", err
		}
		visible := []reopenRow{}
		for _, row := range removed["milestone_completion_evidence"] {
			if canRead(derefStr(row["child_item_id"])) {
				visible = append(visible, row)
			}
		}
		delete(removed, "milestone_completion_evidence")
		if len(visible) > 0 {
			removed["milestone_completion_evidence"] = visible
		}
		body, err := json.Marshal(removed)
		if err != nil {
			return "", err
		}
		audit["removed_effects"] = body
	}
	out, err := json.Marshal(audit)
	return string(out), err
}

// Imported audit is history only. Remap its journal references so a retained
// correction still shows its reversal, without importing executable evidence.
func importedReopenAudit(raw string, changes, items map[string]string) (string, error) {
	var audit map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &audit); err != nil {
		return "", err
	}
	if audit == nil {
		return raw, nil
	}
	var original string
	var ids []string
	if err := json.Unmarshal(audit["reversed_resolution_id"], &original); err == nil {
		audit["reversed_resolution_id"], _ = json.Marshal(changes[original])
	}
	if err := json.Unmarshal(audit["reversed_change_ids"], &ids); err == nil {
		mapped := []string{}
		for _, id := range ids {
			if changes[id] != "" {
				mapped = append(mapped, changes[id])
			}
		}
		audit["reversed_change_ids"], _ = json.Marshal(mapped)
	}
	var removed map[string][]reopenRow
	if err := json.Unmarshal(audit["removed_effects"], &removed); err == nil && removed != nil {
		visible := []reopenRow{}
		for _, row := range removed["milestone_completion_evidence"] {
			if id := items[derefStr(row["child_item_id"])]; id != "" {
				row["child_item_id"] = ptr(id)
				visible = append(visible, row)
			}
		}
		delete(removed, "milestone_completion_evidence")
		if len(visible) > 0 {
			removed["milestone_completion_evidence"] = visible
		}
		audit["removed_effects"], _ = json.Marshal(removed)
	}
	out, err := json.Marshal(audit)
	return string(out), err
}

func (s *Store) invalidateScheduleReopenTx(tx *sql.Tx, id string) error {
	_, err := tx.Exec(s.q(`UPDATE schedule_reopen_checkpoints SET blocked_code='reopen_conflict' WHERE schedule_id=? AND reopened_change_id IS NULL AND blocked_code IS NULL`), id)
	return err
}

func (s *Store) reopenDependencyQ(q Queryer, sc *models.ItemSchedule) (bool, error) {
	var n int
	err := q.QueryRow(s.q(`SELECT (SELECT COUNT(*) FROM schedule_links WHERE parent_schedule_id=? OR child_schedule_id=?) + (SELECT COUNT(*) FROM schedule_gates WHERE blocked_item_id=? OR blocker_item_id=?) + (SELECT COUNT(*) FROM milestone_children WHERE schedule_id=? AND removed_at IS NULL)`), sc.ID, sc.ID, sc.ItemID, sc.ItemID, sc.ID).Scan(&n)
	return n > 0, err
}

// captureResolutionUndoTx runs under the schedule pin, before even the skip
// deadline guard. Only the current row and the bounded missed walk are read.
func (s *Store) captureResolutionUndoTx(tx *sql.Tx, sc *models.ItemSchedule, operation, actor, actorUserID string, x, now time.Time) (*resolutionUndo, error) {
	u := &resolutionUndo{ID: newID(), ScheduleID: sc.ID, Operation: operation, Actor: actor, ActorUserID: actorUserID}
	if sc.NextWall != nil {
		u.Key = OccurrenceKey(sc, NormalizeInstant(x), *sc.NextWall)
		u.Identities = append(u.Identities, reopenOccurrenceIdentity{NormalizeInstant(x), *sc.NextWall})
	}
	dependency, err := s.reopenDependencyQ(tx, sc)
	if err != nil {
		return nil, err
	}
	if dependency {
		u.BlockedCode = "reopen_dependency"
	}
	if !dependency && operation != "done" && operation != "skip" && operation != "snooze" && operation != "move" {
		u.BlockedCode = "reopen_not_supported"
	}
	if u.BlockedCode == "" {
		loc, err := s.scheduleViewZone(tx, sc)
		if err != nil {
			return nil, err
		}
		if operation == "done" || operation == "skip" || operation == "move" {
			missed, _, err := missedOccurrences(sc, loc, now)
			if err != nil {
				return nil, err
			}
			for _, m := range missed {
				u.Identities = append(u.Identities, reopenOccurrenceIdentity{NormalizeInstant(m.At), m.Wall.String()})
			}
		}
		if u.Before, err = s.reopenImageQ(tx, sc, u, false); err != nil {
			return nil, err
		}
	}
	return u, nil
}

func readReopenRows(q Queryer, query string, args ...any) ([]reopenRow, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := []reopenRow{}
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		dest := make([]any, len(cols))
		for i := range vals {
			dest[i] = &vals[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		row := reopenRow{}
		for i, col := range cols {
			if vals[i].Valid {
				v := vals[i].String
				row[col] = &v
			} else {
				row[col] = nil
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Store) reopenImageQ(q Queryer, sc *models.ItemSchedule, u *resolutionUndo, lock bool) (reopenImage, error) {
	image := reopenImage{Rows: map[string][]reopenRow{}, Context: map[string][]reopenRow{}}
	// scheduleSnapshotTx uses only QueryRow and does not acquire additional locks.
	var err error
	if image.Schedule, err = s.scheduleSnapshotQ(q, sc.ID); err != nil {
		return image, err
	}
	if err = q.QueryRow(s.q(`SELECT updated_at FROM item_schedules WHERE id=?`), sc.ID).Scan(&image.UpdatedAt); err != nil {
		return image, err
	}
	if sc.Floating {
		if err = q.QueryRow(s.q(`SELECT COALESCE(NULLIF(i.assigned_user_id,''),NULLIF(i.created_by_user_id,''),'') || '|' || COALESCE(u.timezone,'') FROM items i LEFT JOIN users u ON u.id=COALESCE(NULLIF(i.assigned_user_id,''),NULLIF(i.created_by_user_id,'')) WHERE i.id=?`), sc.ItemID).Scan(&image.Recipient); err != nil {
			return image, err
		}
	}
	suffix := ""
	if lock && s.dialect.Driver() == DriverPostgres {
		suffix = " FOR UPDATE"
	}
	// Child rows are acquired in this fixed order, at their strongest mode.
	for _, table := range []string{"item_reminders", "schedule_occurrences", "schedule_occurrence_overrides", "milestone_completion_evidence"} {
		where := "schedule_id=?"
		args := []any{sc.ID}
		switch table {
		case "schedule_occurrences":
			terms := []string{}
			for _, key := range u.Identities {
				terms = append(terms, "occurrence_at=?")
				args = append(args, key.At)
				if len(key.Wall) == 10 {
					terms = append(terms, "occurrence_wall=?")
					args = append(args, key.Wall)
				}
			}
			if len(terms) == 0 {
				where += " AND 1=0"
			} else {
				where += " AND (" + strings.Join(terms, " OR ") + ")"
			}
		case "schedule_occurrence_overrides", "milestone_completion_evidence":
			where += " AND occurrence_key=?"
			args = append(args, u.Key)
		}
		image.Rows[table], err = readReopenRows(q, s.q("SELECT * FROM "+table+" WHERE "+where+" ORDER BY id"+suffix), args...)
		if err != nil {
			return image, err
		}
	}
	// Forecast belongs to this schedule; partner rows are never after-images.
	image.Context["forecast"], err = readReopenRows(q, s.q("SELECT * FROM schedule_forecasts WHERE schedule_id=?"), sc.ID)
	if err != nil {
		return image, err
	}
	return image, nil
}

func (s *Store) finishResolutionUndoTx(tx *sql.Tx, sc *models.ItemSchedule, u *resolutionUndo) error {
	if u == nil {
		return nil
	}
	// Capture seq after the resolution's own comment/activity writes.
	if err := tx.QueryRow(s.q(`SELECT seq FROM items WHERE id=?`), sc.ItemID).Scan(&u.ItemSeq); err != nil {
		return err
	}
	var err error
	if u.BlockedCode == "" {
		if u.After, err = s.reopenImageQ(tx, sc, u, false); err != nil {
			return err
		}
	}
	before, err := json.Marshal(u)
	if err != nil {
		return err
	}
	after, err := json.Marshal(u.After)
	if err != nil {
		return err
	}
	_, err = tx.Exec(s.q(`INSERT INTO schedule_reopen_checkpoints(schedule_id,resolution_id,occurrence_key,operation,snapshot_version,item_seq,before_json,after_json,blocked_code,reopened_change_id) VALUES(?,?,?,?,1,?,?,?,?,NULL)
 ON CONFLICT(schedule_id) DO UPDATE SET resolution_id=excluded.resolution_id,occurrence_key=excluded.occurrence_key,operation=excluded.operation,snapshot_version=excluded.snapshot_version,item_seq=excluded.item_seq,before_json=excluded.before_json,after_json=excluded.after_json,blocked_code=excluded.blocked_code,reopened_change_id=NULL`), sc.ID, u.ID, u.Key, u.Operation, u.ItemSeq, string(before), string(after), nilIfEmpty(u.BlockedCode))
	return err
}

func (s *Store) loadResolutionUndoQ(q Queryer, sc *models.ItemSchedule) (*resolutionUndo, error) {
	var raw string
	var blocked, consumed *string
	var version int
	var u resolutionUndo
	err := q.QueryRow(s.q(`SELECT resolution_id,occurrence_key,operation,before_json,blocked_code,reopened_change_id,snapshot_version FROM schedule_reopen_checkpoints WHERE schedule_id=?`), sc.ID).Scan(&u.ID, &u.Key, &u.Operation, &raw, &blocked, &consumed, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &ReopenError{"reopen_unavailable"}
	}
	if err != nil {
		return nil, err
	}
	// A checkpoint in any state proves this schedule was resolved locally.
	// Dependencies use only its own relationship rows, never partner state.
	dependency, err := s.reopenDependencyQ(q, sc)
	if err != nil {
		return nil, err
	}
	if dependency || derefStr(blocked) == "reopen_dependency" {
		u.BlockedCode = "reopen_dependency"
		return &u, nil
	}
	if version != 1 || json.Unmarshal([]byte(raw), &u) != nil {
		return nil, &ReopenError{"reopen_conflict"}
	}
	if consumed != nil {
		u.BlockedCode = "reopen_conflict"
	} else if blocked != nil {
		u.BlockedCode = *blocked
	}
	return &u, nil
}

func (s *Store) validateResolutionUndoQ(q Queryer, sc *models.ItemSchedule, u *resolutionUndo, now time.Time, lock bool) error {
	dependency, err := s.reopenDependencyQ(q, sc)
	if err != nil {
		return err
	}
	if dependency || u.BlockedCode == "reopen_dependency" {
		return &ReopenError{"reopen_dependency"}
	}
	if u.BlockedCode != "" {
		return &ReopenError{u.BlockedCode}
	}
	var seq int64
	if err = q.QueryRow(s.q(`SELECT seq FROM items WHERE id=?`), sc.ItemID).Scan(&seq); err != nil {
		return err
	}
	if seq != u.ItemSeq {
		return &ReopenError{"reopen_conflict"}
	}
	current, err := s.reopenImageQ(q, sc, u, lock)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, u.After) {
		return &ReopenError{"reopen_conflict"}
	}
	return s.validateReopenDeadlineQ(q, sc, u, now)
}

func (s *Store) validateReopenDeadlineQ(q Queryer, sc *models.ItemSchedule, u *resolutionUndo, now time.Time) error {
	var snap scheduleDateSnapshot
	if err := json.Unmarshal([]byte(u.Before.Schedule), &snap); err != nil {
		return &ErrScheduleBroken{Code: "snapshot"}
	}
	restored := *sc
	restored.NextAt, restored.NextWall, restored.NextOverrideAt = snap.NextAt, snap.NextWall, snap.NextOverrideAt
	restored.EndedAt, restored.AnchorWall = snap.EndedAt, snap.AnchorWall
	restored.ReminderFired = snap.ReminderFiredAt != nil
	if restored.ErrorCode != nil {
		return &ErrScheduleBroken{Code: *restored.ErrorCode}
	}
	if _, err := compileScheduleSpec(storedSpecInput(&restored, func() string {
		if snap.AnchorWall != nil {
			return *snap.AnchorWall
		}
		return snap.Dtstart
	}())); err != nil {
		return &ErrScheduleBroken{Code: "syntax"}
	}
	if snap.Roles.Origin != "native" || snap.Roles.DeadlineDays == nil {
		return nil
	}
	filter, err := s.itemDoneFilterQ(q, sc.ItemID)
	if err != nil {
		return err
	}
	doneExpr, args := s.buildChildrenDoneExpr([]collectionDoneFilter{filter}, "i")
	var terminal bool
	if err = q.QueryRow(s.q(`SELECT `+doneExpr+` FROM items i WHERE i.id=?`), append(args, sc.ItemID)...).Scan(&terminal); err != nil {
		return err
	}
	if terminal {
		return nil
	}
	loc, err := s.scheduleViewZone(q, &restored)
	if err != nil {
		return &ErrScheduleBroken{Code: "timezone"}
	}
	zone := loc.String()
	d := deadlineCandidate{sc: restored, days: *snap.Roles.DeadlineDays, secs: *snap.Roles.DeadlineSecs, zone: &zone, behavior: derefStr(snap.Roles.DeadlineBehavior)}
	for _, row := range u.Before.Rows["schedule_occurrence_overrides"] {
		if derefStr(row["role"]) == "deadline" {
			d.override = row["value_at"]
		}
	}
	if err = d.resolve(); err != nil {
		return &ErrScheduleBroken{Code: "deadline"}
	}
	states, err := s.deadlineJournal(q, []deadlineCandidate{d})
	if err != nil {
		return err
	}
	state := states[sc.ID]
	if state.err != nil {
		return &ErrScheduleBroken{Code: "deadline"}
	}
	if d.dueAfterConversion(now, state) && (d.behavior == "lapses" || !state.recorded) {
		return &ReopenError{"reopen_deadline_passed"}
	}
	return nil
}

func (s *Store) reopenAvailabilityQ(q Queryer, sc *models.ItemSchedule, now time.Time) (*models.ScheduleLastResolution, error) {
	u, err := s.loadResolutionUndoQ(q, sc)
	if err != nil {
		var refusal *ReopenError
		if errors.As(err, &refusal) {
			return &models.ScheduleLastResolution{ReopenCode: refusal.Code}, nil
		}
		return nil, err
	}
	out := &models.ScheduleLastResolution{ResolutionID: u.ID, OccurrenceKey: u.Key, State: u.Operation, CanReopen: true}
	if err = s.validateResolutionUndoQ(q, sc, u, now, false); err != nil {
		var refusal *ReopenError
		var broken *ErrScheduleBroken
		switch {
		case errors.As(err, &refusal):
			out.ReopenCode = refusal.Code
		case errors.As(err, &broken):
			out.ReopenCode = "schedule_error"
		default:
			return nil, err
		}
		out.CanReopen = false
	}
	return out, nil
}

func (s *Store) ReopenOccurrence(workspaceID, itemID string, in ReopenInput) (*ReopenResult, error) {
	return s.reopenOccurrenceAt(workspaceID, itemID, in, time.Now())
}
func (s *Store) reopenOccurrenceAt(workspaceID, itemID string, in ReopenInput, now time.Time) (*ReopenResult, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = s.pinScheduleTx(tx, workspaceID, itemID); err != nil {
		return nil, err
	}
	sc, err := s.getScheduleQ(tx, workspaceID, itemID, false)
	if err != nil {
		return nil, err
	}
	u, err := s.loadResolutionUndoQ(tx, sc)
	if err != nil {
		return nil, err
	}
	if u.BlockedCode == "reopen_dependency" {
		return nil, &ReopenError{"reopen_dependency"}
	}
	if in.ResolutionID != u.ID {
		return nil, &ReopenError{"reopen_conflict"}
	}
	if err = s.validateResolutionUndoQ(tx, sc, u, now, true); err != nil {
		return nil, err
	}
	removed := map[string][]reopenRow{}
	result := &ReopenResult{ResolutionID: u.ID, ChangeID: newID(), OccurrenceKey: u.Key, PreviousState: u.Operation}
	if u.CommentID != "" {
		var exists bool
		if err = tx.QueryRow(s.q(`SELECT EXISTS(SELECT 1 FROM comments WHERE id=?)`), u.CommentID).Scan(&exists); err != nil {
			return nil, err
		}
		if exists {
			result.CommentID = u.CommentID
		}
	}
	for _, table := range []string{"schedule_occurrences", "schedule_occurrence_overrides", "milestone_completion_evidence", "item_reminders"} {
		before := map[string]reopenRow{}
		after := map[string]reopenRow{}
		for _, r := range u.Before.Rows[table] {
			before[derefStr(r["id"])] = r
		}
		for _, r := range u.After.Rows[table] {
			after[derefStr(r["id"])] = r
		}
		ids := map[string]bool{}
		for id := range before {
			ids[id] = true
		}
		for id := range after {
			ids[id] = true
		}
		ordered := make([]string, 0, len(ids))
		for id := range ids {
			ordered = append(ordered, id)
		}
		sort.Strings(ordered)
		for _, id := range ordered {
			b, a := before[id], after[id]
			if reflect.DeepEqual(b, a) {
				continue
			}
			if b == nil && (table == "schedule_occurrences" || table == "milestone_completion_evidence") {
				removed[table] = append(removed[table], a)
			}
			if a != nil {
				if _, err = tx.Exec(s.q("DELETE FROM "+table+" WHERE id=?"), id); err != nil {
					return nil, err
				}
				if table == "schedule_occurrences" && b == nil && derefStr(a["state"]) == "missed" {
					result.MissedRemoved++
				}
			}
			if b == nil {
				continue
			}
			if table == "item_reminders" {
				b["updated_at"] = ptr(now.UTC().Format(time.RFC3339Nano))
			}
			// A deleted comment remains deleted; never recreate its row or reference.
			if table == "schedule_occurrences" && b["comment_id"] != nil {
				var n int
				if err = tx.QueryRow(s.q(`SELECT COUNT(*) FROM comments WHERE id=?`), *b["comment_id"]).Scan(&n); err != nil {
					return nil, err
				}
				if n == 0 {
					b["comment_id"] = nil
				}
			}
			cols := make([]string, 0, len(b))
			for c := range b {
				cols = append(cols, c)
			}
			sort.Strings(cols)
			args := make([]any, len(cols))
			for i, c := range cols {
				args[i] = b[c]
			}
			if _, err = tx.Exec(s.q("INSERT INTO "+table+" ("+strings.Join(cols, ",")+") VALUES ("+placeholders(len(cols))+")"), args...); err != nil {
				return nil, err
			}
		}
	}
	var snap scheduleDateSnapshot
	if err = json.Unmarshal([]byte(u.Before.Schedule), &snap); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(s.q(`UPDATE item_schedules SET next_at=?,next_wall=?,next_override_at=?,anchor_wall=?,ended_at=?,ended_dismissed_at=?,error_code=?,updated_at=? WHERE id=?`), snap.NextAt, snap.NextWall, snap.NextOverrideAt, snap.AnchorWall, snap.EndedAt, snap.EndedDismissedAt, snap.ErrorCode, now.UTC().Format(time.RFC3339Nano), sc.ID); err != nil {
		return nil, err
	}
	actor := in.Actor
	if actor != "agent" {
		actor = "user"
	}
	var root string
	if err = tx.QueryRow(s.q(`SELECT root_change_id FROM schedule_changes WHERE id=?`), u.ID).Scan(&root); err != nil {
		return nil, err
	}
	audit := map[string]any{}
	if err = json.Unmarshal([]byte(u.Before.Schedule), &audit); err != nil {
		return nil, err
	}
	metadata := map[string]any{"reversed_resolution_id": u.ID, "reversed_change_ids": u.JournalIDs, "removed_effects": removed, "original_actor": u.Actor, "original_actor_user_id": u.ActorUserID, "actor_user_id": in.ActorUserID}
	for key, value := range metadata {
		audit[key] = value
	}
	after, err := json.Marshal(audit)
	if err != nil {
		return nil, err
	}
	if err = s.journalScheduleChangeRowTx(tx, result.ChangeID, sc.ID, "reopen", u.Key, "", u.After.Schedule, string(after), actor, "Reopened schedule occurrence", "user", u.ID, root); err != nil {
		return nil, err
	}
	activity := in.Activity
	activity.WorkspaceID, activity.DocumentID, activity.Action, activity.Actor, activity.UserID = workspaceID, itemID, "schedule_reopened", actor, in.ActorUserID
	if activity.Source == "" {
		activity.Source = "api"
	}
	meta := map[string]any{"resolution_id": u.ID, "change_id": result.ChangeID, "state": u.Operation, "original_actor": u.Actor, "original_actor_user_id": u.ActorUserID}
	if activity.Metadata != "" {
		var supplied map[string]any
		if json.Unmarshal([]byte(activity.Metadata), &supplied) == nil {
			for k, v := range supplied {
				if _, exists := meta[k]; !exists {
					meta[k] = v
				}
			}
		}
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	activity.Metadata = string(raw)
	if _, err = s.createActivityQ(tx, activity); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(s.q(`UPDATE schedule_reopen_checkpoints SET reopened_change_id=? WHERE schedule_id=?`), result.ChangeID, sc.ID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit reopen: %w", err)
	}
	return result, nil
}

func (s *Store) journalResolutionTx(tx *sql.Tx, u *resolutionUndo, id, change, key, role, before, after, actor, reason, source string) error {
	changeID := newID()
	if u != nil {
		changeID = u.ID
		u.JournalIDs = append(u.JournalIDs, changeID)
	}
	return s.journalScheduleChangeRowTx(tx, changeID, id, change, key, role, before, after, actor, reason, source, "", "")
}

// Relationship ineligibility is sticky even when the relationship is removed.
func (s *Store) invalidateReopenDependencyTx(tx *sql.Tx, scheduleIDs ...string) error {
	sort.Strings(scheduleIDs)
	for _, id := range scheduleIDs {
		if _, err := tx.Exec(s.q(`UPDATE schedule_reopen_checkpoints SET blocked_code='reopen_dependency' WHERE schedule_id=? AND reopened_change_id IS NULL`), id); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) invalidateItemReopenDependencyTx(tx *sql.Tx, itemIDs ...string) error {
	args := make([]any, len(itemIDs))
	for i, id := range itemIDs {
		args[i] = id
	}
	rows, err := tx.Query(s.q(`SELECT id FROM item_schedules WHERE item_id IN (`+placeholders(len(args))+`) ORDER BY id`), args...)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	return s.invalidateReopenDependencyTx(tx, ids...)
}
