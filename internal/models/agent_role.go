package models

import "time"

// Role park modes stored in agent_roles.park_mode.
const (
	ParkModeNone = "none"
	ParkModeTop  = "top"
	ParkModeLine = "line"
)

// RolePark represents an active per-role park line on an agent role.
// Absent (nil) on AgentRole when park_mode is "none".
type RolePark struct {
	Mode        string     `json:"mode"`
	SortOrder   *int       `json:"sort_order"`
	SetAt       *time.Time `json:"set_at"`
	SetByUserID *string    `json:"set_by_user_id,omitempty"`
	SetBy       *string    `json:"set_by"`
	ParkSeq     int        `json:"park_seq"`
}

// AgentRole represents a workspace-scoped capability role for human-agent assignment.
// Roles describe what kind of work gets done (e.g. "Planner", "Implementer", "Reviewer"),
// not who or what tool does it. Items are assigned to a (user, role) pair.
type AgentRole struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Icon        string    `json:"icon"`
	Tools       string    `json:"tools"`
	SortOrder   int       `json:"sort_order"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`

	// Park is non-nil when the role lane is parked (mode "top" or "line").
	Park *RolePark `json:"park,omitempty"`

	// Computed (not stored)
	ItemCount int `json:"item_count,omitempty"`
}

// AgentRoleCreate is the input for creating a new agent role.
type AgentRoleCreate struct {
	Name        string `json:"name"`
	Slug        string `json:"slug,omitempty"` // auto-generated from name if empty
	Description string `json:"description,omitempty"`
	Icon        string `json:"icon,omitempty"`
	Tools       string `json:"tools,omitempty"`
}

// AgentRoleUpdate is the input for updating an existing agent role.
type AgentRoleUpdate struct {
	Name        *string `json:"name,omitempty"`
	Slug        *string `json:"slug,omitempty"`
	Description *string `json:"description,omitempty"`
	Icon        *string `json:"icon,omitempty"`
	Tools       *string `json:"tools,omitempty"`
	SortOrder   *int    `json:"sort_order,omitempty"`
}

// RoleQueueRole is the lightweight role header inside RoleQueueResponse.
type RoleQueueRole struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
	Icon string `json:"icon"`
}

// RoleQueueLease is the live lease projection on a Now item in a role queue.
type RoleQueueLease struct {
	Holder    string    `json:"holder"`
	ExpiresAt time.Time `json:"expires_at"`
}

// RoleQueueNowItem is an active or leased lane item (not in the queue).
type RoleQueueNowItem struct {
	Ref    string          `json:"ref"`
	Title  string          `json:"title"`
	Status string          `json:"status"`
	Since  time.Time       `json:"since"`
	Lease  *RoleQueueLease `json:"lease"`
	Live   string          `json:"live"`
}

// RoleQueueItem is one queued item in canonical role lane order.
type RoleQueueItem struct {
	ID            string `json:"id"`
	Ref           string `json:"ref"`
	Title         string `json:"title"`
	Status        string `json:"status"`
	Priority      string `json:"priority"`
	RoleSortOrder int    `json:"role_sort_order"`
	Blocked       bool   `json:"blocked"`
	Ready         bool   `json:"ready"`
	CanEdit       bool   `json:"can_edit"`
}

// RoleQueuePark describes the active park line relative to the caller's
// visible queue. Omitted on RoleQueueResponse when the role is unparked.
type RoleQueuePark struct {
	Mode           string     `json:"mode"`
	SortOrder      *int       `json:"sort_order"`
	Index          *int       `json:"index"`
	FirstParkedRef *string    `json:"first_parked_ref"`
	SetBy          *string    `json:"set_by"`
	SetAt          *time.Time `json:"set_at"`
	ParkSeq        int        `json:"park_seq"`
}

// RoleQueueResponse is the response shape for
// GET /api/v1/workspaces/{ws}/agent-roles/{role}/queue.
type RoleQueueResponse struct {
	Workspace   string             `json:"workspace"`
	Role        RoleQueueRole      `json:"role"`
	CanReorder  bool               `json:"can_reorder"`
	CanPark     bool               `json:"can_park"`
	Now         []RoleQueueNowItem `json:"now"`
	Queue       []RoleQueueItem    `json:"queue"`
	Park        *RoleQueuePark     `json:"park,omitempty"`
	QueueTotal  int                `json:"queue_total"`
	ParkedTotal int                `json:"parked_total"`
	QueueMore   bool               `json:"queue_more"`
}

// UnreadMayHoldReady reports whether queue entries past the returned prefix
// could still be ready. Parking is monotone in lane order (every entry at or
// below the line is parked), so once the first parked entry is inside the
// prefix or directly after it, nothing unread can be ready even when
// QueueMore is true.
func (q *RoleQueueResponse) UnreadMayHoldReady() bool {
	if !q.QueueMore {
		return false
	}
	if q.Park != nil && q.Park.Index != nil && *q.Park.Index <= len(q.Queue) {
		return false
	}
	return true
}

// RoleParkSetRequest is the request body for PUT .../agent-roles/{role}/park.
type RoleParkSetRequest struct {
	At              *string `json:"at,omitempty"`
	Before          *string `json:"before,omitempty"`
	After           *string `json:"after,omitempty"`
	ExpectedParkSeq *int    `json:"expected_park_seq,omitempty"`
}

// RoleParkClearRequest is the optional request body for DELETE .../agent-roles/{role}/park.
type RoleParkClearRequest struct {
	ExpectedParkSeq *int `json:"expected_park_seq,omitempty"`
}

// RoleParkDetail is the park object returned by PUT .../agent-roles/{role}/park.
type RoleParkDetail struct {
	Mode           string     `json:"mode"`
	SortOrder      *int       `json:"sort_order"`
	FirstParkedRef *string    `json:"first_parked_ref"`
	SetBy          *string    `json:"set_by"`
	SetAt          *time.Time `json:"set_at"`
	ParkSeq        int        `json:"park_seq"`
}

// RoleParkSetResponse is the response body for PUT .../agent-roles/{role}/park.
type RoleParkSetResponse struct {
	Park RoleParkDetail `json:"park"`
}

// RoleParkClearResponse is the response body for DELETE .../agent-roles/{role}/park.
type RoleParkClearResponse struct {
	Park    *RoleParkDetail `json:"park"`
	ParkSeq int             `json:"park_seq"`
}
