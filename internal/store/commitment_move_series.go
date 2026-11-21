package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// A series move is a target move that also writes the root's native target
// offset, so the outstanding occurrence and every later one read at the new
// time of day. The offset is measured from the outstanding occurrence's start
// (never from its current target) in the zone the schedule is read in, as
// civil days and seconds, the way every role offset is applied. The hard
// deadline never changes. Resolved history, carried rows and the overrides on
// other occurrences are left alone. Every occurrence's reminder follows its
// target (target minus the lead).

const seriesMoveReason = "Rescheduled this and future occurrences"

// moveScopeIsSeries validates a move scope: "" and occurrence are the default,
// series needs the target role.
func moveScopeIsSeries(scope string, role schedule.Role) (bool, error) {
	switch scope {
	case "", "occurrence":
		return false, nil
	case "series":
	default:
		return false, fieldErr("scope", "enum", "scope must be occurrence or series")
	}
	if role != schedule.RoleTarget {
		return false, fieldErr("scope", "combination", "a series move needs role target")
	}
	return true, nil
}

// seriesTargetPlan is the root's target offset before and after a series move.
type seriesTargetPlan struct {
	key           string
	before, after scheduleRoles
}

func (sp *seriesTargetPlan) changed() bool {
	return sp.before.TargetDays != sp.after.TargetDays || sp.before.TargetSecs != sp.after.TargetSecs
}

func (sp *seriesTargetPlan) view() *models.LinkedMoveSeries {
	return &models.LinkedMoveSeries{AppliesFrom: sp.key,
		CurrentOffset: models.TargetOffset{TargetDays: sp.before.TargetDays, TargetSecs: sp.before.TargetSecs},
		NewOffset:     models.TargetOffset{TargetDays: sp.after.TargetDays, TargetSecs: sp.after.TargetSecs}}
}

// revision binds the move's revision to the scope, every stored role and the
// new offset, so an occurrence preview's token is stale for a series apply and
// a roles write that moves no date still invalidates a series preview.
func (sp *seriesTargetPlan) revision(base string) (string, error) {
	b, err := json.Marshal(struct {
		Base, Scope string
		Stored      scheduleRoles
		New         models.TargetOffset
	}{base, "series", sp.before, models.TargetOffset{TargetDays: sp.after.TargetDays, TargetSecs: sp.after.TargetSecs}})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// planSeriesTargetTx computes the new offset for the plan's root and refuses
// what a series move cannot do: a row whose roles are not native, a target
// role a link uses, and a target after the stored hard deadline offset. It
// writes nothing.
func (s *Store) planSeriesTargetTx(tx *sql.Tx, p *linkedPlan) (*seriesTargetPlan, error) {
	n := p.nodes[p.root]
	stored, err := s.scheduleRolesQ(tx, n.sc.ID)
	if err != nil {
		return nil, err
	}
	if stored.RolesOrigin != "native" {
		return nil, fieldErr("scope", "combination", "a series move needs native commitment roles; set the roles first")
	}
	before := scheduleRolesFromExport(stored)
	wall, err := schedule.ParseWall(*n.sc.NextWall)
	if err != nil {
		return nil, err
	}
	local := p.to.In(n.rec)
	from := time.Date(wall.Year, wall.Month, wall.Day, 0, 0, 0, 0, time.UTC)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	after := before
	after.TargetDays = int(day.Sub(from).Hours() / 24)
	after.TargetSecs = local.Hour()*3600 + local.Minute()*60 + local.Second() - (wall.Hour*3600 + wall.Minute*60 + wall.Second)
	if err := validateRoleValues(after); err != nil {
		return nil, err
	}
	linked, err := s.linkedRolesTx(tx, n.sc.ID)
	if err != nil {
		return nil, err
	}
	if err := guardLinkedRoles(linked, before, after); err != nil {
		return nil, err
	}
	if before.DeadlineDays != nil && before.DeadlineSecs != nil {
		civil := func(days, secs int) time.Time {
			return time.Date(wall.Year, wall.Month, wall.Day, wall.Hour, wall.Minute, wall.Second, 0, time.UTC).AddDate(0, 0, days).Add(time.Duration(secs) * time.Second)
		}
		if civil(after.TargetDays, after.TargetSecs).After(civil(*before.DeadlineDays, *before.DeadlineSecs)) {
			return nil, fieldErr("to", "range", "a series move cannot put the target after the hard deadline")
		}
	}
	return &seriesTargetPlan{key: n.key, before: before, after: after}, nil
}

// applySeriesTargetTx writes the new offset and journals it on the root, under
// the move's root row when the move changed a date, else as its own root.
func (s *Store) applySeriesTargetTx(tx *sql.Tx, p *linkedPlan, sp *seriesTargetPlan, actor, reason string) error {
	n := p.nodes[p.root]
	if err := s.writeNativeRolesTx(tx, n.sc, sp.after, false); err != nil {
		return err
	}
	// The outstanding occurrence's reminder follows the new offset. An
	// overridden one stays where its move armed it (the override replaces the
	// target, and inside the lead the move armed the target itself).
	root, err := s.getScheduleByIDQ(tx, n.sc.ID)
	if err != nil {
		return err
	}
	if root != nil && root.EndedAt == nil && root.NextAt != nil && root.NextWall != nil && root.NextOverrideAt == nil {
		fire, err := storedFireInstant(root, root.LeadMinutes, sp.after.target())
		if err != nil {
			return err
		}
		if err := s.retimeArmedReminderTx(tx, root.ID, NormalizeInstant(fire)); err != nil {
			return err
		}
	}
	snapshot := func(r scheduleRoles) string {
		b, _ := json.Marshal(models.TargetOffset{TargetDays: r.TargetDays, TargetSecs: r.TargetSecs})
		return string(b)
	}
	why, source := seriesMoveReason, "system"
	if reason != "" {
		why, source = reason, "user"
	}
	return s.journalScheduleChangeRowTx(tx, newID(), n.sc.ID, "update", sp.key, "target", snapshot(sp.before), snapshot(sp.after), actor, why, source, p.rootChange, p.rootChange)
}

// isSeriesTargetRow reports whether a journal row is a series move's offset
// change: an update on the target role whose before and after are exactly
// {target_days, target_secs}. A run-once reminder's retarget journals update
// on the target role too, with whole schedule snapshots.
func isSeriesTargetRow(c models.ScheduleChangeExport) bool {
	if c.ChangeType != "update" || c.Role == nil || *c.Role != "target" || c.BeforeJSON == nil || c.AfterJSON == nil {
		return false
	}
	for _, raw := range []string{*c.BeforeJSON, *c.AfterJSON} {
		var m map[string]json.RawMessage
		if json.Unmarshal([]byte(raw), &m) != nil || len(m) != 2 || m["target_days"] == nil || m["target_secs"] == nil {
			return false
		}
	}
	return true
}
