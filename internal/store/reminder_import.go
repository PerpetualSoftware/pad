package store

import (
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// convertLegacyReminderBundle normalizes the old input before either schedule
// writer runs. It leaves the supplied bundle untouched, including on refusal.
func convertLegacyReminderBundle(data *models.WorkspaceExport) (*models.WorkspaceExport, error) {
	if len(data.Reminders) == 0 {
		return data, nil
	}
	items, collections := map[string]bool{}, map[string]bool{}
	for _, c := range data.Collections {
		collections[c.ID] = true
	}
	for _, item := range data.Items {
		items[item.ID] = collections[item.CollectionID]
	}
	out := *data
	out.Schedules = append([]models.ScheduleExport(nil), data.Schedules...)
	out.ScheduleChanges = append([]models.ScheduleChangeExport(nil), data.ScheduleChanges...)
	out.Reminders = nil
	scheduled := map[string]string{}
	for i := range out.Schedules {
		sc := &out.Schedules[i]
		// Older schedule bundles predate journal identities. Supply an import
		// identity so any new history can follow the usual schedule remapping.
		if sc.ID == "" {
			sc.ID = newID()
		}
		scheduled[sc.ItemID] = sc.ID
	}
	reminders := make([]*models.Reminder, 0, len(data.Reminders))
	targets := make([]string, 0, len(data.Reminders))
	unreadable := make([]string, 0, len(data.Reminders))
	// Keep original bundle positions for history even when entries are skipped.
	indices := make([]int, 0, len(data.Reminders))
	skipped := 0
	migrationTime := now()
	for i, rm := range data.Reminders {
		if !items[rm.ItemID] {
			// Import skips items whose collection is absent too. Neither kind
			// of orphan has an item to carry a schedule or reminder history.
			skipped++
			continue
		}
		indices = append(indices, i)
		r := &models.Reminder{ID: rm.ItemID, ItemID: rm.ItemID, RemindAt: rm.RemindAt, CreatedAt: rm.CreatedAt, UpdatedAt: rm.UpdatedAt}
		if rm.FiredAt != "" {
			r.FiredAt = &rm.FiredAt
		}
		if rm.AckedAt != "" {
			r.AckedAt = &rm.AckedAt
		}
		at, field := validateLegacyReminder(r)
		unreadable = append(unreadable, field)
		reminders = append(reminders, r)
		targets = append(targets, at)
	}
	winners := resolveReminderCollisions(reminders, scheduled, unreadable, true)
	for i, index := range indices {
		rm := data.Reminders[index]
		if winner, ok := winners[rm.ItemID]; !ok || winner != i {
			continue
		}
		at := targets[i]
		wall := strings.TrimSuffix(at, "Z")
		sx := models.ScheduleExport{ID: newID(), ItemID: rm.ItemID, Kind: "check", Dtstart: wall, Tz: "UTC", Anchor: "calendar", Audience: "both", NextAt: at, NextWall: wall, CreatedAt: rm.CreatedAt, UpdatedAt: rm.UpdatedAt, Roles: &models.ScheduleRolesExport{RolesOrigin: "native"}, Reminder: &models.ScheduleReminderExport{RemindAt: at, FiredAt: rm.FiredAt, AckedAt: rm.AckedAt}}
		in, err := carriedFromExport(sx, nil)
		if err != nil {
			return nil, err
		}
		if _, err := prepareCarriedSchedule(in); err != nil {
			return nil, err
		}
		out.Schedules = append(out.Schedules, sx)
		scheduled[rm.ItemID] = sx.ID
	}
	for i, index := range indices {
		rm := data.Reminders[index]
		if winner, ok := winners[rm.ItemID]; ok && winner == i {
			continue
		}
		if scheduled[rm.ItemID] == "" {
			sx := models.ScheduleExport{ID: newID(), ItemID: rm.ItemID, Kind: "check", Dtstart: strings.TrimSuffix(migrationTime, "Z"), Tz: "UTC", Anchor: "calendar", Audience: "both", EndedAt: migrationTime, CreatedAt: migrationTime, UpdatedAt: migrationTime, Roles: &models.ScheduleRolesExport{RolesOrigin: "native"}}
			out.Schedules = append(out.Schedules, sx)
			scheduled[rm.ItemID] = sx.ID
			after, err := json.Marshal(sx)
			if err != nil {
				return nil, err
			}
			id := newID()
			out.ScheduleChanges = append(out.ScheduleChanges, models.ScheduleChangeExport{ID: id, ItemID: rm.ItemID, ScheduleID: sx.ID, ChangeType: "import", AfterJSON: ptr(string(after)), Reason: legacyReminderCarrierReason, ReasonSource: "system", RootChangeID: id, RecordedAt: migrationTime})
		}
		reason := legacyReminderHistoryReason
		if unreadable[i] != "" {
			reason = "reminder kept as history: unreadable " + unreadable[i]
		}
		before, err := json.Marshal(map[string]any{
			"item_id": rm.ItemID, "remind_at": rm.RemindAt,
			"fired_at": rm.FiredAt, "acked_at": rm.AckedAt,
			"created_at": rm.CreatedAt, "updated_at": rm.UpdatedAt, "bundle_index": index,
		})
		if err != nil {
			return nil, err
		}
		id := newID()
		out.ScheduleChanges = append(out.ScheduleChanges, models.ScheduleChangeExport{
			ID: id, ItemID: rm.ItemID, ScheduleID: scheduled[rm.ItemID], ChangeType: "import",
			BeforeJSON: ptr(string(before)), Reason: reason, ReasonSource: "system",
			RootChangeID: id, RecordedAt: nowNano(),
		})
	}

	if skipped > 0 {
		slog.Warn("workspace import: skipped orphaned reminders", "skipped", skipped)
	}
	return &out, nil
}
