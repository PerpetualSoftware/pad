package models

import "time"

type Workspace struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Slug          string `json:"slug"`
	OwnerID       string `json:"owner_id,omitempty"`       // User ID of workspace owner
	OwnerUsername string `json:"owner_username,omitempty"` // Populated by JOIN (not stored)
	IsGuest       bool   `json:"is_guest,omitempty"`       // True when user has grants but no membership
	Description   string `json:"description"`
	Settings      string `json:"settings"` // JSON
	// Source records how the workspace was created — "web", "cli", or "mcp"
	// (empty on rows predating migration 069). A cli/mcp origin means an
	// agent surface created the workspace, so an agent is already wired up
	// even before it creates its first item; the dashboard's
	// has_agent_activity signal ORs this in (BUG-1557).
	Source    string            `json:"source,omitempty"`
	SortOrder int               `json:"sort_order"`
	Context   *WorkspaceContext `json:"context,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
	DeletedAt *time.Time        `json:"deleted_at,omitempty"`
}

type WorkspaceCreate struct {
	Name        string            `json:"name"`
	Slug        string            `json:"slug,omitempty"`     // auto-generated if empty
	OwnerID     string            `json:"owner_id,omitempty"` // set by server from authenticated user
	Description string            `json:"description,omitempty"`
	Settings    string            `json:"settings,omitempty"`
	Context     *WorkspaceContext `json:"context,omitempty"`
	Template    string            `json:"template,omitempty"` // workspace template name (e.g. "startup", "scrum", "product")
	// Source is the creation surface — "web", "cli", or "mcp". It is set
	// SERVER-SIDE ONLY: the HTTP create handler derives it authoritatively
	// from the request's auth shape (actorFromRequest), so `json:"-"` keeps
	// it out of the request body — a web client must not be able to spoof
	// "cli" to self-suppress the connect-agent/onboarding prompts. Direct
	// store callers (cloud auto-create, import) leave it "" so they aren't
	// treated as agent-created. See BUG-1557.
	Source string `json:"-"`
}

type WorkspaceUpdate struct {
	Name        *string           `json:"name,omitempty"`
	Description *string           `json:"description,omitempty"`
	Settings    *string           `json:"settings,omitempty"`
	Context     *WorkspaceContext `json:"context,omitempty"`
}
