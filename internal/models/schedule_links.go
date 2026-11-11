package models

import "database/sql"

// CommitmentQueryer is the read surface shared by authorization callbacks and
// store transactions. Store.Queryer aliases it to keep models independent of stores.
type CommitmentQueryer interface {
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

// CommitmentAccessLoader refreshes the request's permissions under the write lock.
type CommitmentAccessLoader func(CommitmentQueryer) (CommitmentAccess, error)

// Time links and commitment gates (TASK-4353, DOC-4348 unit 5). These are store
// DTOs; the commitment view also exposes the filtered link and gate shapes.
// None may carry data the reader could not read on its own.

// LinkCancelPolicy values.
const (
	// LinkWaitingOn leaves a child waiting on a cancelled parent occurrence (the default).
	LinkWaitingOn = "waiting_on"
	// LinkCancelChildOccurrence marks the child occurrence paired with a cancelled
	// parent occurrence as no longer needed. It never ends the series and never
	// touches completed work.
	LinkCancelChildOccurrence = "cancel_child_occurrence"
)

// ScheduleLinkOffset is a link's offset in storable form. ElapsedSecs applies to
// mode elapsed; Days, ClockSecs (seconds past midnight), AllDay and Zone to civil.
type ScheduleLinkOffset struct {
	Mode        string `json:"mode"` // elapsed | civil
	ElapsedSecs int64  `json:"elapsed_secs,omitempty"`
	Days        int    `json:"days,omitempty"`
	ClockSecs   int    `json:"clock_secs,omitempty"`
	AllDay      bool   `json:"all_day,omitempty"`
	Zone        string `json:"zone,omitempty"`
}

// ScheduleLinkCreate links ChildItemID's ChildRole date to ParentItemID's
// ParentRole date. Roles are target, earliest, deadline or occupied_until.
type ScheduleLinkCreate struct {
	ParentItemID, ParentRole string
	ChildItemID, ChildRole   string
	Offset                   ScheduleLinkOffset
	CancelPolicy             string // empty means waiting_on
	Move                     LinkWriteOptions
}

// LinkWriteOptions governs the move a link write makes: writing a link, or
// changing its offset, moves the child's outstanding occurrence to the parent
// plus the offset through the same plan as a cascade, so the cascade's rules
// apply to it: a preview, the one reason and revision of a deadline
// renegotiation, and the acknowledgement of a date outside its own bounds.
type LinkWriteOptions struct {
	Actor string
	// ActorUserID is the acting user's id. Actor is the journal's actor, which
	// may be a kind; this is what excludes a person from their own move notice.
	ActorUserID           string
	Reason                string
	Preview               bool // compute the move and write nothing, the link included
	Revision              string
	AcknowledgeViolations bool
}

// ScheduleLinkResult is the answer to a link write. Link is nil for a preview.
// Moved holds only what the caller can read, as for a linked move.
type ScheduleLinkResult struct {
	Link  *ScheduleLink
	Moved LinkedMoveResult
}

// ScheduleLink is a stored time link, addressed by item.
type ScheduleLink struct {
	ID           string             `json:"id"`
	ParentItemID string             `json:"parent_item_id"`
	ParentRole   string             `json:"parent_role"`
	ChildItemID  string             `json:"child_item_id"`
	ChildRole    string             `json:"child_role"`
	Offset       ScheduleLinkOffset `json:"offset"`
	CancelPolicy string             `json:"cancel_policy"`
	CreatedAt    string             `json:"created_at"`
	UpdatedAt    string             `json:"updated_at"`
}

// CommitmentAccess is the caller's decision about the items a store call may
// touch. An item absent from Readable is treated as not existing; an item
// absent from Editable is read-only. Editable implies Readable.
type CommitmentAccess struct {
	Readable map[string]bool
	Editable map[string]bool
}

// LinkedMoveInput moves one date of ItemID's outstanding occurrence, and every
// date linked below it.
type LinkedMoveInput struct {
	ItemID string
	Role   string // target | earliest | deadline | occupied_until
	To     string // RFC3339 instant
	Actor  string
	// ActorUserID is the acting user's id. Actor is the journal's actor, which
	// may be a kind; this is what excludes a person from their own move notice
	// and what a refused move's approval notice is recomputed for.
	ActorUserID string
	// Reason is the one reason of a renegotiation: required when the move changes any hard deadline.
	Reason string
	// Preview computes the result and the revision and writes nothing.
	Preview bool
	// Revision is the token a preview returned; an apply that renegotiates a
	// deadline, or crosses a bound, must carry it.
	Revision string
	// AcknowledgeViolations accepts that a dependent date lands outside one of
	// its own bounds. The link and the bound both stay.
	AcknowledgeViolations bool
	Access                CommitmentAccess
}

// LinkedDateChange is one date a move changed, with the item it belongs to.
type LinkedDateChange struct {
	ItemID string `json:"item_id"`
	Role   string `json:"role"`
	From   string `json:"from,omitempty"`
	To     string `json:"to"`
	Root   bool   `json:"root,omitempty"`
}

// LinkedMoveResult holds only what the caller can read. It carries no total,
// count or flag that would show a hidden dependent.
type LinkedMoveResult struct {
	Revision string             `json:"revision"` // opaque, fixed length
	Changes  []LinkedDateChange `json:"changes"`
	// Violations names dates outside their own bounds, readable items only.
	Violations []LinkedDateChange `json:"violations,omitempty"`
	// LeftInPlace names finished (done or abandoned) dependents the link would
	// have moved and the cascade left where they are: To is the date the link
	// would have given. Readable items only, no count of the rest.
	LeftInPlace []LinkedDateChange `json:"left_in_place,omitempty"`
	// RenegotiatesDeadline is whether the move changes a hard deadline the
	// caller can see; it needs a reason and the revision.
	RenegotiatesDeadline bool `json:"renegotiates_deadline,omitempty"`
}

// ScheduleLinkUpdate changes a link's offset or cancel policy. A new offset moves
// the child like a new link does.
type ScheduleLinkUpdate struct {
	Offset       *ScheduleLinkOffset
	CancelPolicy *string
	Move         LinkWriteOptions
}

// Gate outcomes, as ScheduleGate.Outcome names them: what became of the blocker
// occurrence the gate is held against.
const (
	// GateOutcomePending: the blocker has not ended, or is still owed.
	GateOutcomePending = "pending"
	// GateOutcomeSucceeded: the blocker was done. It releases the gate for good.
	GateOutcomeSucceeded = "succeeded"
	// GateOutcomeCancelled: the blocker ended without delivering (an abandoned
	// status, or an item removed). The child stays blocked for a decision.
	GateOutcomeCancelled = "cancelled"
	// GateOutcomeLapsed: the blocker's deadline passed and it lapsed. The child
	// stays blocked for a decision.
	GateOutcomeLapsed = "lapsed"
	// GateOutcomeSkipped: the blocker occurrence was skipped. The child stays
	// blocked for a decision.
	GateOutcomeSkipped = "skipped"
)

// Gate states, as ScheduleGateView.State names them.
const (
	GateStateReleased             = "released"
	GateStateWaiting              = "waiting"
	GateStateBlockedNeedsDecision = "blocked_needs_decision"
)

// ScheduleGate is a commitment gate as one reader sees it: BlockedItemID cannot
// start until the blocker occurrence fixed at creation succeeds. OpenedAt is set
// once, when it did, and the gate never closes again.
type ScheduleGate struct {
	ID            string  `json:"id"`
	BlockedItemID string  `json:"blocked_item_id"`
	BlockerItemID string  `json:"blocker_item_id"`
	Outcome       string  `json:"outcome"`
	OpenedAt      *string `json:"opened_at,omitempty"`
	CreatedAt     string  `json:"created_at"`
}

// ScheduleGateView is an item's gate state for one reader. State covers EVERY
// gate of the item, so a child held by a blocker the reader cannot read is shown
// held, never released. Gates and Unresolved name only the blockers the reader
// can read, so a hidden blocker's item and gate are not disclosed.
type ScheduleGateView struct {
	State      string         `json:"state"` // released | waiting | blocked_needs_decision
	Gates      []ScheduleGate `json:"gates"`
	Unresolved []string       `json:"unresolved,omitempty"` // blocker item ids, readable only
}
