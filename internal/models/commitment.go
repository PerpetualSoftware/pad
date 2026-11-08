package models

// Commitment is the roles half of an item's schedule as the commitment
// contract reads it (TASK-4351): the effective role values of the outstanding
// occurrence, with that occurrence's overrides applied, and the journal rows
// that carry the deadline's history.
type Commitment struct {
	ScheduleID string          `json:"schedule_id"`
	ItemID     string          `json:"item_id"`
	Roles      CommitmentRoles `json:"roles"`
	// OccurrenceKey names the outstanding occurrence; empty once the schedule
	// ended.
	OccurrenceKey string             `json:"occurrence_key,omitempty"`
	Journal       []CommitmentChange `json:"journal"`
}

// CommitmentRoles holds the effective value of each role for the outstanding
// occurrence: a date ("2026-11-02") for an all-day role, an RFC3339 UTC
// instant otherwise. A role the schedule does not carry, and every role of an
// ended schedule, is nil. Legacy rows read through the same derivation as
// native ones.
type CommitmentRoles struct {
	Target           *string `json:"target"`
	Earliest         *string `json:"earliest"`
	Deadline         *string `json:"deadline"`
	DeadlineBehavior *string `json:"deadline_behavior"`
	OccupiedUntil    *string `json:"occupied_until"`
	Outcome          string  `json:"outcome"`
	RolesOrigin      string  `json:"roles_origin"` // legacy_backfill | native
}

// CommitmentChange is one journal row of the deadline's history: a recorded
// miss, its on-time completion correction, a deadline renegotiation, or an
// assignee change.
// A correction names the miss it answers in ParentChangeID and shares its root.
type CommitmentChange struct {
	ID             string  `json:"id"`
	ChangeType     string  `json:"change_type"` // missed_recorded | done | move | update (assignee)
	OccurrenceKey  *string `json:"occurrence_key,omitempty"`
	Role           *string `json:"role,omitempty"`
	AfterJSON      *string `json:"after_json,omitempty"`
	Actor          string  `json:"actor"`
	Reason         string  `json:"reason,omitempty"`
	ReasonSource   string  `json:"reason_source"`
	RootChangeID   string  `json:"root_change_id"`
	ParentChangeID *string `json:"parent_change_id,omitempty"`
	RecordedAt     string  `json:"recorded_at"`
}

// CommitmentRolesInput is the full set of role offsets a native write stores.
// An offset is days plus seconds from the occurrence's seed wall. Deadline
// offsets and behavior are set together or not at all; so are the earliest and
// occupied pairs.
type CommitmentRolesInput struct {
	TargetDays       int     `json:"target_days"`
	TargetSecs       int     `json:"target_secs"`
	EarliestDays     *int    `json:"earliest_days,omitempty"`
	EarliestSecs     *int    `json:"earliest_secs,omitempty"`
	DeadlineDays     *int    `json:"deadline_days,omitempty"`
	DeadlineSecs     *int    `json:"deadline_secs,omitempty"`
	DeadlineBehavior *string `json:"deadline_behavior,omitempty"`
	OccupiedDays     *int    `json:"occupied_days,omitempty"`
	OccupiedSecs     *int    `json:"occupied_secs,omitempty"`
	Outcome          string  `json:"outcome,omitempty"`
}
