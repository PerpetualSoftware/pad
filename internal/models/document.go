package models

import "time"

// Valid document types
var ValidDocTypes = []string{
	"roadmap", "phase-plan", "architecture", "ideation",
	"feature-spec", "notes", "prompt-library", "reference",
}

// Valid document statuses
var ValidStatuses = []string{
	"draft", "active", "completed", "archived",
}

// Valid actors
var ValidActors = []string{"user", "agent"}

// Valid sources
var ValidSources = []string{"cli", "web", "skill"}

type Document struct {
	ID             string     `json:"id"`
	WorkspaceID    string     `json:"workspace_id"`
	Title          string     `json:"title"`
	Slug           string     `json:"slug"`
	Content        string     `json:"content"`
	DocType        string     `json:"doc_type"`
	Status         string     `json:"status"`
	Tags           string     `json:"tags"` // JSON array
	Pinned         bool       `json:"pinned"`
	SortOrder      int        `json:"sort_order"`
	CreatedBy      string     `json:"created_by"`
	LastModifiedBy string     `json:"last_modified_by"`
	Source         string     `json:"source"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	DeletedAt      *time.Time `json:"deleted_at,omitempty"`
}

type DocumentCreate struct {
	Title          string `json:"title"`
	Content        string `json:"content,omitempty"`
	DocType        string `json:"doc_type,omitempty"`
	Status         string `json:"status,omitempty"`
	Tags           string `json:"tags,omitempty"`
	Pinned         bool   `json:"pinned,omitempty"`
	CreatedBy      string `json:"created_by,omitempty"`
	Source         string `json:"source,omitempty"`
}

type DocumentUpdate struct {
	Title          *string `json:"title,omitempty"`
	Content        *string `json:"content,omitempty"`
	DocType        *string `json:"doc_type,omitempty"`
	Status         *string `json:"status,omitempty"`
	Tags           *string `json:"tags,omitempty"`
	Pinned         *bool   `json:"pinned,omitempty"`
	SortOrder      *int    `json:"sort_order,omitempty"`
	LastModifiedBy string  `json:"last_modified_by,omitempty"`
	Source         string  `json:"source,omitempty"`
	ChangeSummary  string  `json:"change_summary,omitempty"`
}

type QuickSave struct {
	Title          string `json:"title"`
	Content        string `json:"content"`
	DocType        string `json:"doc_type,omitempty"`
	Status         string `json:"status,omitempty"`
	Tags           string `json:"tags,omitempty"`
	CreatedBy      string `json:"created_by,omitempty"`
	Source         string `json:"source,omitempty"`
	ChangeSummary  string `json:"change_summary,omitempty"`
}

type DocumentListParams struct {
	Type   string
	Status string
	Tag    string
	Pinned *bool
	Query  string
	Sort   string
	Order  string
}

func IsValidDocType(t string) bool {
	for _, v := range ValidDocTypes {
		if v == t {
			return true
		}
	}
	return false
}

func IsValidStatus(s string) bool {
	for _, v := range ValidStatuses {
		if v == s {
			return true
		}
	}
	return false
}

func IsValidActor(a string) bool {
	return a == "user" || a == "agent"
}

func IsValidSource(s string) bool {
	return s == "cli" || s == "web" || s == "skill"
}
