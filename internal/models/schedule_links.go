package models

// Time links (TASK-4353, DOC-4348 unit 5). These are store
// DTOs: nothing here is a wire shape yet (the routes land in 5c), and none of
// them may carry data the reader could not read on its own.

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
	// RenegotiatesDeadline is whether the move changes a hard deadline the
	// caller can see; it needs a reason and the revision.
	RenegotiatesDeadline bool `json:"renegotiates_deadline,omitempty"`
}

// ScheduleLinkUpdate changes a link's offset (for later moves) or cancel policy.
type ScheduleLinkUpdate struct {
	Offset       *ScheduleLinkOffset
	CancelPolicy *string
}
