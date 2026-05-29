package models

// ReportLayout is a user's personalization of the Insights surface for one
// workspace (PLAN-1628 / TASK-1634). Stored as the JSON `config` of a
// user_report_layouts row, one per (user, workspace).
type ReportLayout struct {
	// HiddenCards lists the metric-card IDs the user has toggled off. Card IDs
	// are the stable section identifiers in ReportCardIDs; unknown IDs are
	// dropped on save. The Totals summary is always shown and not toggleable.
	HiddenCards []string `json:"hidden_cards"`
	// DefaultWindow is the window to restore on load (day|week|2wk|month).
	// Empty means the surface default (week).
	DefaultWindow string `json:"default_window"`
	// DefaultCollections is the collection-slug filter to restore on load.
	// Empty means all collections.
	DefaultCollections []string `json:"default_collections"`
}

// ReportCardIDs is the canonical set of toggleable Insights metric cards. The
// web page gates each section on `!hidden_cards.includes(id)`; the API filters
// HiddenCards to this set on save so stored config can't drift from the UI.
var ReportCardIDs = map[string]bool{
	"throughput":              true,
	"cycle_time":              true,
	"wip":                     true,
	"completed_by_collection": true,
	"status_distribution":     true,
}

// ValidReportWindow reports whether w is an accepted report window (empty is
// allowed and means the surface default).
func ValidReportWindow(w string) bool {
	switch w {
	case "", "day", "week", "2wk", "month":
		return true
	default:
		return false
	}
}
