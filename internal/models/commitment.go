package models

import "encoding/json"

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
	OccurrenceKey string                   `json:"occurrence_key,omitempty"`
	Journal       []CommitmentChange       `json:"journal"`
	Conflicts     []CommitmentDateConflict `json:"conflicts,omitempty"`
	// Forecast is the hand-set estimate, null when none. It is not a role: it
	// arms nothing and passing it is never a missed deadline.
	Forecast *CommitmentForecast `json:"forecast"`
	// Milestone is the outcome derived from the selected children, read for
	// the one reader asking.
	Milestone CommitmentMilestone `json:"milestone"`
}

// CommitmentDateConflict reports only this commitment own dates.
type CommitmentDateConflict struct {
	Role       string `json:"role"`
	Bound      string `json:"bound"`
	Value      string `json:"value"`
	BoundValue string `json:"bound_value"`
}

// ForecastSummary is display-only decoration for an admitted schedule.
type ForecastSummary struct {
	Value     string `json:"value"`
	SetBy     string `json:"set_by"`
	SetByName string `json:"set_by_name,omitempty"`
	SetByKind string `json:"set_by_kind"`
	SetAt     string `json:"set_at"`
	Source    string `json:"source"`
}

// CommitmentForecast includes read-time passing state and stored history.
type CommitmentForecast struct {
	Value     string          `json:"value"`
	SetBy     string          `json:"set_by"`
	SetByName string          `json:"set_by_name,omitempty"`
	SetByKind string          `json:"set_by_kind"`
	SetAt     string          `json:"set_at"`
	Source    string          `json:"source"`
	Passed    bool            `json:"passed"`
	History   []ForecastEvent `json:"history"`
}

// ForecastEvent is one append-only row of a forecast's history.
type ForecastEvent struct {
	ID         string `json:"id,omitempty"`
	Action     string `json:"action"` // set | clear
	Value      string `json:"value"`
	PrevValue  string `json:"prev_value"`
	SetBy      string `json:"set_by"`
	SetByKind  string `json:"set_by_kind"`
	Source     string `json:"source"`
	RecordedAt string `json:"recorded_at"`
}

// MilestoneChild is one selected child as a reader that may see it reads it.
type MilestoneChild struct {
	ItemRef    string `json:"item_ref"`
	Kind       string `json:"kind"`        // child | waiver
	CloseState string `json:"close_state"` // open | done | abandoned | missing
	DoneValue  string `json:"done_value,omitempty"`
	Counts     bool   `json:"counts"`
	Waived     bool   `json:"waived"`
	Reason     string `json:"reason,omitempty"`
	AddedBy    string `json:"added_by"`
	AddedAt    string `json:"added_at"`
}

// MilestoneHistoryRow is a selection that was later removed.
type MilestoneHistoryRow struct {
	ItemRef   string `json:"item_ref"`
	Kind      string `json:"kind"`
	Reason    string `json:"reason,omitempty"`
	AddedBy   string `json:"added_by"`
	AddedAt   string `json:"added_at"`
	RemovedBy string `json:"removed_by"`
	RemovedAt string `json:"removed_at"`
}

// MilestoneEvidence is one child as it stood when the milestone was resolved.
type MilestoneEvidence struct {
	ItemRef    string `json:"item_ref"`
	Kind       string `json:"kind"`
	CloseState string `json:"close_state"`
	DoneValue  string `json:"done_value,omitempty"`
	RecordedAt string `json:"recorded_at"`
}

// MilestoneChange is a visible child whose live state differs from the
// evidence recorded at completion.
type MilestoneChange struct {
	ItemRef  string `json:"item_ref"`
	Was      string `json:"was"` // close state at completion, or "unselected"
	Now      string `json:"now"` // live close state, or "unselected"
	Reopened bool   `json:"reopened,omitempty"`
}

// CommitmentMilestone is the derived outcome. Result is completed, pending,
// unknown (a selected child the reader cannot see) or null (nothing selected).
// Totals and every list cover the children the reader can see; EvidenceChanged
// is null whenever Result is unknown. Nothing here resolves an occurrence.
type CommitmentMilestone struct {
	Children        []MilestoneChild      `json:"children"`
	Counted         int                   `json:"counted"`
	Total           int                   `json:"total"`
	Waivers         int                   `json:"waivers"`
	Result          *string               `json:"result"`
	CompletedAt     *string               `json:"completed_at,omitempty"`
	EvidenceChanged *bool                 `json:"evidence_changed"`
	Changes         []MilestoneChange     `json:"changes"`
	Evidence        []MilestoneEvidence   `json:"evidence"`
	History         []MilestoneHistoryRow `json:"history"`
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
	ChangeType     string  `json:"change_type"` // missed_recorded | done | move | update (deadline or assignee)
	OccurrenceKey  *string `json:"occurrence_key,omitempty"`
	Role           *string `json:"role,omitempty"`
	BeforeJSON     *string `json:"before_json,omitempty"`
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

// CommitmentRolePatchValue preserves absent, null and value on a PATCH member.
// A nil Value means clear only when Present is true.
type CommitmentRolePatchValue[T any] struct {
	Present bool
	Value   *T
}

func (p *CommitmentRolePatchValue[T]) UnmarshalJSON(raw []byte) error {
	p.Present = true
	return json.Unmarshal(raw, &p.Value)
}

// CommitmentRolesPatch edits only named members. Pairing is checked against
// the stored roles under the schedule transaction lock, never during decoding.
// POST continues to use CommitmentRolesInput's full-object semantics.
type CommitmentRolesPatch struct {
	TargetDays       CommitmentRolePatchValue[int]    `json:"target_days"`
	TargetSecs       CommitmentRolePatchValue[int]    `json:"target_secs"`
	EarliestDays     CommitmentRolePatchValue[int]    `json:"earliest_days"`
	EarliestSecs     CommitmentRolePatchValue[int]    `json:"earliest_secs"`
	DeadlineDays     CommitmentRolePatchValue[int]    `json:"deadline_days"`
	DeadlineSecs     CommitmentRolePatchValue[int]    `json:"deadline_secs"`
	DeadlineBehavior CommitmentRolePatchValue[string] `json:"deadline_behavior"`
	OccupiedDays     CommitmentRolePatchValue[int]    `json:"occupied_days"`
	OccupiedSecs     CommitmentRolePatchValue[int]    `json:"occupied_secs"`
	Outcome          CommitmentRolePatchValue[string] `json:"outcome"`
}

// CommitmentDeadlineOffset uses the deadline fields of CommitmentRolesInput.
// Pointers preserve its paired-field validation, including an omitted member.
type CommitmentDeadlineOffset struct {
	DeadlineDays *int `json:"deadline_days"`
	DeadlineSecs *int `json:"deadline_secs"`
}

type CommitmentRenegotiateInput struct {
	CommitmentAccessLoader CommitmentAccessLoader `json:"-"`
	ActorUserID            string                 `json:"-"`
	Access                 CommitmentAccess       `json:"-"`
	// AcknowledgeViolations accepts that a dependent date lands outside one of
	// its own bounds. The link and the bound both stay.
	AcknowledgeViolations bool `json:"acknowledge_violations"`

	Reason           string                    `json:"reason"`
	Scope            string                    `json:"scope"`
	Deadline         *string                   `json:"deadline"`
	DeadlineOffset   *CommitmentDeadlineOffset `json:"deadline_offset"`
	DeadlineBehavior *string                   `json:"deadline_behavior"`
	RemoveDeadline   bool                      `json:"remove_deadline"`
	Revision         string                    `json:"revision"`
	Preview          bool                      `json:"preview"`
}

type CommitmentRenegotiationPreview struct {
	Scope                   string                    `json:"scope"`
	OccurrenceKey           string                    `json:"occurrence_key"`
	AppliesFrom             string                    `json:"applies_from"`
	CurrentDeadline         *string                   `json:"current_deadline"`
	NewDeadline             *string                   `json:"new_deadline"`
	CurrentDeadlineBehavior *string                   `json:"current_deadline_behavior"`
	NewDeadlineBehavior     *string                   `json:"new_deadline_behavior"`
	CurrentOffset           *CommitmentDeadlineOffset `json:"current_offset"`
	NewOffset               *CommitmentDeadlineOffset `json:"new_offset"`
	MissRecordedFirst       bool                      `json:"miss_recorded_first"`
	MissOccurrenceKey       *string                   `json:"miss_occurrence_key"`
	KeepsDeadlineOverride   bool                      `json:"keeps_deadline_override"`
	ClearsDeadlineOverrides bool                      `json:"clears_deadline_overrides"`
}

type CommitmentRenegotiation struct {
	Cascade  *LinkedMoveResult              `json:"cascade,omitempty"`
	Preview  CommitmentRenegotiationPreview `json:"preview"`
	Revision string                         `json:"revision"`
}
