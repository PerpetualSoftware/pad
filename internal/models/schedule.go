package models

// ItemSchedule is the time trigger attached to an item (TASK-4102, DOC-4098):
// a one-off deadline or a recurring rule. At most one per item.
//
// The schedule is the RULE. The one outstanding occurrence is carried by a
// managed row in item_reminders (ReminderID), so the reminder tick fires it.
//
// Every wall (Dtstart, Dtend, RDates, ExDates, NextWall, AnchorWall) is a
// zone-less reading in the schedule.Wall text form, "2026-11-02" for an
// all-day date or "2026-11-02T09:00:00". Every instant (NextAt,
// NextOverrideAt, EndedAt, ...) is RFC3339 UTC.
type ItemSchedule struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	ItemID      string `json:"item_id"`

	Kind    string  `json:"kind"` // deadline_hard | deadline_soft | check | window
	Dtstart string  `json:"dtstart"`
	Dtend   *string `json:"dtend,omitempty"` // windows only
	// Tz is the pinned IANA zone; nil exactly when Floating.
	Tz       *string `json:"tz,omitempty"`
	Floating bool    `json:"floating"`
	// Rrule is the canonical schedule.NormalizeRule text; nil = one occurrence
	// at Dtstart.
	Rrule       *string  `json:"rrule,omitempty"`
	RDates      []string `json:"rdates"`
	ExDates     []string `json:"exdates"`
	Anchor      string   `json:"anchor"`   // calendar | completion
	Audience    string   `json:"audience"` // human | agent | both
	LeadMinutes int      `json:"lead_minutes"`

	// NextAt is the outstanding occurrence's exact instant once it is fired or
	// overridden; while a floating schedule is armed with no override, it is the
	// early bound (the wall read at UTC+14), an internal value that surfaces must
	// not show as the fire time. A pinned schedule's is always the occurrence
	// itself. The fire transaction overwrites a floating bound with the exact
	// instant it fired for (TASK-4103). Readers go through store.OutstandingInstant.
	NextAt         *string `json:"next_at,omitempty"`
	NextWall       *string `json:"next_wall,omitempty"`
	NextOverrideAt *string `json:"next_override_at,omitempty"`
	AnchorWall     *string `json:"anchor_wall,omitempty"`
	EndedAt        *string `json:"ended_at,omitempty"`
	// ErrorCode is nil while healthy. Otherwise it says why the stored rule
	// could not be advanced, never third-party text: a schedule.Code (syntax,
	// unknown_part, duplicate_part, missing_part, range, combination,
	// unsupported, unsatisfiable, zone, wall) or a cap code (cap_scan,
	// cap_stall, cap_time, cap_window, cap_instances). A timing edit that
	// compiles clears it.
	ErrorCode *string `json:"error_code,omitempty"`

	// ReminderID is the managed item_reminders row, nil once the schedule ended.
	ReminderID *string `json:"reminder_id,omitempty"`
	// ReminderFired is whether that row is fired (read, never stored on the
	// schedule). It decides how the outstanding instant is read.
	ReminderFired bool `json:"-"`

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// ScheduleCreate is the input to Store.CreateSchedule. Dtstart, Dtend, RDates
// and ExDates are walls; a zone, offset or Z is refused. Rrule may be any RRULE
// text or a shorthand. Empty Anchor and Audience take their defaults
// (calendar, both).
type ScheduleCreate struct {
	Kind        string   `json:"kind"`
	Dtstart     string   `json:"dtstart"`
	Dtend       string   `json:"dtend,omitempty"`
	Tz          string   `json:"tz,omitempty"`
	Floating    bool     `json:"floating,omitempty"`
	Rrule       string   `json:"rrule,omitempty"`
	RDates      []string `json:"rdates,omitempty"`
	ExDates     []string `json:"exdates,omitempty"`
	Anchor      string   `json:"anchor,omitempty"`
	Audience    string   `json:"audience,omitempty"`
	LeadMinutes int      `json:"lead_minutes,omitempty"`
}

// ScheduleUpdate is a partial update: nil leaves a field alone. For Dtend,
// Rrule, RDates and ExDates a pointer to the empty value clears the field.
// Setting Floating true clears the pinned zone; setting a Tz on a floating
// schedule needs Floating=false in the same update.
type ScheduleUpdate struct {
	Kind        *string   `json:"kind,omitempty"`
	Dtstart     *string   `json:"dtstart,omitempty"`
	Dtend       *string   `json:"dtend,omitempty"`
	Tz          *string   `json:"tz,omitempty"`
	Floating    *bool     `json:"floating,omitempty"`
	Rrule       *string   `json:"rrule,omitempty"`
	RDates      *[]string `json:"rdates,omitempty"`
	ExDates     *[]string `json:"exdates,omitempty"`
	Anchor      *string   `json:"anchor,omitempty"`
	Audience    *string   `json:"audience,omitempty"`
	LeadMinutes *int      `json:"lead_minutes,omitempty"`
}
