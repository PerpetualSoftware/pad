package store

import (
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

var commitmentRoles = []string{"target", "earliest", "deadline", "occupied_until"}
var scheduleChangeTypes = []string{"create", "update", "delete", "move", "snooze", "done", "skip", "missed_recorded", "end", "import", "carry"}

func validateCarriedRoles(r *models.ScheduleRolesExport) error {
	if r == nil {
		return nil
	}
	for _, pair := range [][2]*int{{r.EarliestDays, r.EarliestSecs}, {r.DeadlineDays, r.DeadlineSecs}, {r.OccupiedDays, r.OccupiedSecs}} {
		if (pair[0] == nil) != (pair[1] == nil) {
			return dropf("roles", "pairing")
		}
	}
	if (r.DeadlineDays == nil) != (r.DeadlineBehavior == nil) {
		return dropf("roles", "pairing")
	}
	if !oneOf(r.RolesOrigin, []string{"legacy_backfill", "native"}) || (r.DeadlineBehavior != nil && !oneOf(*r.DeadlineBehavior, []string{"lapses", "stays_owed"})) {
		return dropf("roles", "enum")
	}
	for _, n := range []*int{&r.TargetDays, &r.TargetSecs, r.EarliestDays, r.EarliestSecs, r.DeadlineDays, r.DeadlineSecs, r.OccupiedDays, r.OccupiedSecs} {
		if n != nil && (int64(*n) < math.MinInt32 || int64(*n) > math.MaxInt32) {
			return dropf("roles", "range")
		}
	}
	return nil
}

func canonicalCarriedKey(key string, floating bool) (string, error) {
	parts := strings.Split(key, "|")
	var at, wall string
	switch len(parts) {
	case 1:
		wall = parts[0]
	case 2:
		var err error
		at, err = carryInstant("override", parts[0])
		if err != nil || at == "" {
			return "", dropf("override", "key")
		}
		wall = parts[1]
	default:
		return "", dropf("override", "key")
	}
	w, err := carryWall("override", wall)
	if err != nil || (!floating && !w.AllDay && at == "") {
		return "", dropf("override", "key")
	}
	return OccurrenceKey(&models.ItemSchedule{Floating: floating}, at, w.String()), nil
}

func prepareCarriedOverrides(in []models.ScheduleOverrideExport, floating bool) ([]models.ScheduleOverrideExport, []string, error) {
	var out []models.ScheduleOverrideExport
	var drops []string
	seen := map[string]bool{}
	for _, o := range in {
		if !oneOf(o.Role, commitmentRoles) {
			return nil, nil, dropf("override", "role")
		}
		key, err := canonicalCarriedKey(o.OccurrenceKey, floating)
		if err != nil {
			drops = append(drops, "override:key")
			continue
		}
		unique := key + "|" + o.Role
		if seen[unique] {
			return nil, nil, dropf("override", "duplicate")
		}
		seen[unique] = true
		o.OccurrenceKey = key
		o.ValueAt, err = carryInstant("override", o.ValueAt)
		if err != nil || o.ValueAt == "" {
			return nil, nil, dropf("override", "instant")
		}
		if o.ValueWall != "" {
			w, err := carryWall("override", o.ValueWall)
			if err != nil {
				return nil, nil, err
			}
			o.ValueWall = w.String()
		}
		for _, ts := range []string{o.CreatedAt, o.UpdatedAt} {
			if _, err := time.Parse(time.RFC3339, ts); err != nil {
				return nil, nil, dropf("override", "instant")
			}
		}
		out = append(out, o)
	}
	return out, drops, nil
}

func (s *Store) scheduleRolesQ(q Queryer, id string) (*models.ScheduleRolesExport, error) {
	var r models.ScheduleRolesExport
	err := q.QueryRow(s.q(`SELECT target_days,target_secs,earliest_days,earliest_secs,deadline_days,deadline_secs,occupied_days,occupied_secs,deadline_behavior,outcome,roles_origin FROM item_schedules WHERE id=?`), id).Scan(&r.TargetDays, &r.TargetSecs, &r.EarliestDays, &r.EarliestSecs, &r.DeadlineDays, &r.DeadlineSecs, &r.OccupiedDays, &r.OccupiedSecs, &r.DeadlineBehavior, &r.Outcome, &r.RolesOrigin)
	if err != nil {
		return nil, fmt.Errorf("read schedule roles: %w", err)
	}
	return &r, nil
}

func (s *Store) scheduleOverridesQ(q Queryer, workspaceID, scheduleID string) (map[string][]models.ScheduleOverrideExport, error) {
	query := `SELECT o.schedule_id,o.occurrence_key,o.role,o.value_at,COALESCE(o.value_wall,''),o.created_at,o.updated_at FROM schedule_occurrence_overrides o JOIN item_schedules sc ON sc.id=o.schedule_id WHERE sc.workspace_id=?`
	args := []any{workspaceID}
	if scheduleID != "" {
		query += ` AND sc.id=?`
		args = append(args, scheduleID)
	}
	rows, err := q.Query(s.q(query+` ORDER BY o.schedule_id,o.occurrence_key,o.role`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]models.ScheduleOverrideExport{}
	for rows.Next() {
		var id string
		var o models.ScheduleOverrideExport
		if err := rows.Scan(&id, &o.OccurrenceKey, &o.Role, &o.ValueAt, &o.ValueWall, &o.CreatedAt, &o.UpdatedAt); err != nil {
			return nil, err
		}
		out[id] = append(out[id], o)
	}
	return out, rows.Err()
}

func (s *Store) scheduleChangesQ(q Queryer, workspaceID, scheduleID string) ([]models.ScheduleChangeExport, error) {
	query := `SELECT id,item_id,schedule_id,occurrence_key,change_type,role,before_json,after_json,actor,reason,reason_source,root_change_id,parent_change_id,recorded_at FROM schedule_changes WHERE workspace_id=?`
	args := []any{workspaceID}
	if scheduleID != "" {
		query += ` AND schedule_id=?`
		args = append(args, scheduleID)
	} else {
		query += ` AND item_id IN (SELECT id FROM items WHERE workspace_id=? AND deleted_at IS NULL)`
		args = append(args, workspaceID)
	}
	rows, err := q.Query(s.q(query+` ORDER BY recorded_at,id`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.ScheduleChangeExport
	for rows.Next() {
		var c models.ScheduleChangeExport
		if err := rows.Scan(&c.ID, &c.ItemID, &c.ScheduleID, &c.OccurrenceKey, &c.ChangeType, &c.Role, &c.BeforeJSON, &c.AfterJSON, &c.Actor, &c.Reason, &c.ReasonSource, &c.RootChangeID, &c.ParentChangeID, &c.RecordedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// restoreScheduleChangesTx maps only retained rows, so causal references never
// point at a discarded change. Unknown schedules represent deleted schedules.
func (s *Store) restoreScheduleChangesTx(tx *sql.Tx, workspaceID string, changes []models.ScheduleChangeExport, items, schedules map[string]string, sources map[string]models.ScheduleExport) ([]models.ImportScheduleDrop, error) {
	var kept []models.ScheduleChangeExport
	var drops []models.ImportScheduleDrop
	changeMap := map[string]string{}
	for _, c := range changes {
		source, known := sources[c.ScheduleID]
		bad := items[c.ItemID] == "" || c.ScheduleID == "" || (known && (schedules[c.ScheduleID] == "" || source.ItemID != c.ItemID)) || !oneOf(c.ChangeType, scheduleChangeTypes) || !oneOf(c.ReasonSource, []string{"system", "user"}) || (c.Role != nil && !oneOf(*c.Role, commitmentRoles))
		if c.OccurrenceKey != nil && !bad {
			floating := source.Floating
			if !known {
				floating = !strings.Contains(*c.OccurrenceKey, "|")
			}
			key, err := canonicalCarriedKey(*c.OccurrenceKey, floating)
			if err != nil {
				bad = true
			} else {
				c.OccurrenceKey = &key
			}
		}
		if bad {
			drops = append(drops, models.ImportScheduleDrop{Item: items[c.ItemID], Reason: "changes:orphan"})
			continue
		}
		changeMap[c.ID] = newID()
		if schedules[c.ScheduleID] == "" {
			schedules[c.ScheduleID] = newID()
		}
		kept = append(kept, c)
	}
	for _, c := range kept {
		root := changeMap[c.RootChangeID]
		if root == "" {
			root = changeMap[c.ID]
		}
		var parent any
		if c.ParentChangeID != nil && changeMap[*c.ParentChangeID] != "" {
			parent = changeMap[*c.ParentChangeID]
		}
		_, err := tx.Exec(s.q(`INSERT INTO schedule_changes (id,workspace_id,item_id,schedule_id,occurrence_key,change_type,role,before_json,after_json,actor,reason,reason_source,root_change_id,parent_change_id,recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`), changeMap[c.ID], workspaceID, items[c.ItemID], schedules[c.ScheduleID], c.OccurrenceKey, c.ChangeType, c.Role, c.BeforeJSON, c.AfterJSON, c.Actor, c.Reason, c.ReasonSource, root, parent, c.RecordedAt)
		if err != nil {
			return nil, fmt.Errorf("restore schedule change: %w", err)
		}
	}
	return drops, nil
}
