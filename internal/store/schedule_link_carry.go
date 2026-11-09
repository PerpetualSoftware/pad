package store

import (
	"fmt"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Carrying time links in the workspace bundle (TASK-4353, version 3). Export,
// import and `pad db migrate-to-pg` (which rides them) now keep the links
// between schedules and their per-occurrence overrides. Gates are not carried
// here.
//
// WHO SEES WHAT. The bundle door is owner-only and unrestricted
// (requireUnrestrictedExportAccess), and every other section of the bundle is
// written without a reader filter, so links are too: the exporter is entitled
// to every end. The one filter is structural: a link is written only when BOTH
// of its schedules are in the bundle.

// exportScheduleLinksQ reads a workspace's links and their overrides, for the
// schedules in exported (the schedule ids of the bundle). A link with an end
// outside that set is not returned and is counted in omitted, which names no
// link and no item. Each read finishes before the next starts.
func (s *Store) exportScheduleLinksQ(q Queryer, workspaceID string, exported map[string]bool) ([]models.ScheduleLinkExport, int, error) {
	rows, err := q.Query(s.q(`SELECT `+linkColumns+` FROM schedule_links WHERE workspace_id=? ORDER BY created_at,id`), workspaceID)
	if err != nil {
		return nil, 0, fmt.Errorf("export schedule links: %w", err)
	}
	var stored []linkRow
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			rows.Close()
			return nil, 0, fmt.Errorf("scan schedule link: %w", err)
		}
		stored = append(stored, l)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, 0, err
	}
	rows.Close()

	var out []models.ScheduleLinkExport
	omitted := 0
	at := map[string]int{} // stored link id -> its index in out
	for _, l := range stored {
		if !exported[l.ParentSchedule] || !exported[l.ChildSchedule] {
			omitted++
			continue
		}
		at[l.ID] = len(out)
		out = append(out, models.ScheduleLinkExport{ParentScheduleID: l.ParentSchedule, ParentRole: l.ParentRole,
			ChildScheduleID: l.ChildSchedule, ChildRole: l.ChildRole, Offset: l.Offset, CancelPolicy: l.CancelPolicy,
			CreatedAt: l.CreatedAt, UpdatedAt: l.UpdatedAt})
	}
	if len(out) == 0 {
		return nil, omitted, nil
	}

	orows, err := q.Query(s.q(`SELECT o.link_id,o.occurrence_key,COALESCE(o.offset_mode,''),o.offset_elapsed,o.offset_days,o.offset_clock,o.offset_all_day,o.offset_zone,COALESCE(o.no_longer_needed_at,''),o.created_at,o.updated_at
 FROM schedule_link_overrides o JOIN schedule_links l ON l.id=o.link_id WHERE l.workspace_id=? ORDER BY o.link_id,o.occurrence_key`), workspaceID)
	if err != nil {
		return nil, 0, fmt.Errorf("export schedule link overrides: %w", err)
	}
	defer orows.Close()
	for orows.Next() {
		var linkID string
		var o models.ScheduleLinkOverrideExport
		var off models.ScheduleLinkOffset
		var allDay int
		if err := orows.Scan(&linkID, &o.OccurrenceKey, &off.Mode, &off.ElapsedSecs, &off.Days, &off.ClockSecs, &allDay, &off.Zone, &o.NoLongerNeededAt, &o.CreatedAt, &o.UpdatedAt); err != nil {
			return nil, 0, fmt.Errorf("scan schedule link override: %w", err)
		}
		i, ok := at[linkID]
		if !ok {
			continue
		}
		if off.Mode != "" {
			off.AllDay = allDay != 0
			o.Offset = &off
		}
		out[i].Overrides = append(out[i].Overrides, o)
	}
	return out, omitted, orows.Err()
}
