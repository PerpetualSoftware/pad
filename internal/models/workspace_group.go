package models

// WorkspaceGroup is a saved per-user workspace filter. Its stored members
// must be intersected with current admission before being sent to a caller.
type WorkspaceGroup struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	WorkspaceIDs []string `json:"workspace_ids"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
}
