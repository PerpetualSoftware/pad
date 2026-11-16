package store

import (
	"fmt"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// convertLegacyReminderBundle normalizes the old input before either schedule
// writer runs. It leaves the supplied bundle untouched, including on refusal.
func convertLegacyReminderBundle(data *models.WorkspaceExport) (*models.WorkspaceExport, error) {
	if len(data.Reminders) == 0 {
		return data, nil
	}
	counts, scheduled := map[string]int{}, map[string]bool{}
	for _, r := range data.Reminders {
		counts[r.ItemID]++
	}
	for _, sc := range data.Schedules {
		scheduled[sc.ItemID] = true
	}
	if err := resolveReminderCollisions(counts, scheduled); err != nil {
		return nil, err
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
	out.Reminders = nil
	for _, rm := range data.Reminders {
		if !items[rm.ItemID] {
			return nil, fmt.Errorf("legacy reminder item %s: missing owner", rm.ItemID)
		}
		r := &models.Reminder{ID: rm.ItemID, ItemID: rm.ItemID, RemindAt: rm.RemindAt, CreatedAt: rm.CreatedAt, UpdatedAt: rm.UpdatedAt}
		if rm.FiredAt != "" {
			r.FiredAt = &rm.FiredAt
		}
		if rm.AckedAt != "" {
			r.AckedAt = &rm.AckedAt
		}
		at, err := validateLegacyReminder(r)
		if err != nil {
			return nil, err
		}
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
	}
	return &out, nil
}
