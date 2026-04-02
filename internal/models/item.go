package models

import (
	"encoding/json"
	"fmt"
	"time"
)

type Item struct {
	ID             string     `json:"id"`
	WorkspaceID    string     `json:"workspace_id"`
	CollectionID   string     `json:"collection_id"`
	Title          string     `json:"title"`
	Slug           string     `json:"slug"`
	Ref            string     `json:"ref,omitempty"` // computed: e.g. "TASK-5", "BUG-8"
	Content        string     `json:"content"`
	Fields         string     `json:"fields"` // JSON string
	Tags           string     `json:"tags"`   // JSON array string
	Pinned         bool       `json:"pinned"`
	SortOrder      int        `json:"sort_order"`
	ParentID       *string    `json:"parent_id,omitempty"`
	CreatedBy      string     `json:"created_by"`
	LastModifiedBy string     `json:"last_modified_by"`
	Source         string     `json:"source"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	DeletedAt      *time.Time `json:"deleted_at,omitempty"`

	// Auto-assigned sequential number within collection
	ItemNumber *int `json:"item_number,omitempty"`

	// Populated by joins (not stored)
	CollectionSlug   string              `json:"collection_slug,omitempty"`
	CollectionName   string              `json:"collection_name,omitempty"`
	CollectionIcon   string              `json:"collection_icon,omitempty"`
	CollectionPrefix string              `json:"collection_prefix,omitempty"`
	DerivedClosure   *ItemDerivedClosure `json:"derived_closure,omitempty"`
	CodeContext      *ItemCodeContext    `json:"code_context,omitempty"`
}

// ComputeRef sets the Ref field from CollectionPrefix and ItemNumber.
// Call this after populating the item from a database query.
func (item *Item) ComputeRef() {
	if item.CollectionPrefix != "" && item.ItemNumber != nil {
		item.Ref = fmt.Sprintf("%s-%d", item.CollectionPrefix, *item.ItemNumber)
	}
}

type ItemRelationRef struct {
	ID             string `json:"id"`
	Slug           string `json:"slug,omitempty"`
	Ref            string `json:"ref,omitempty"`
	Title          string `json:"title"`
	CollectionSlug string `json:"collection_slug,omitempty"`
	Status         string `json:"status,omitempty"`
}

type ItemDerivedClosure struct {
	IsClosed     bool              `json:"is_closed"`
	Kind         string            `json:"kind"`
	Summary      string            `json:"summary"`
	RelatedItems []ItemRelationRef `json:"related_items,omitempty"`
}

type ItemCodeContext struct {
	Provider    string                   `json:"provider"`
	Repo        string                   `json:"repo,omitempty"`
	Branch      string                   `json:"branch,omitempty"`
	PullRequest *ItemPullRequestMetadata `json:"pull_request,omitempty"`
}

type ItemPullRequestMetadata struct {
	Number    int    `json:"number"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	State     string `json:"state"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

type githubPRFields struct {
	Number    int    `json:"number"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	State     string `json:"state"`
	Branch    string `json:"branch"`
	Repo      string `json:"repo"`
	UpdatedAt string `json:"updated_at"`
}

func ExtractItemCodeContext(fieldsJSON string) *ItemCodeContext {
	if fieldsJSON == "" || fieldsJSON == "{}" {
		return nil
	}

	var fieldsMap map[string]any
	if err := json.Unmarshal([]byte(fieldsJSON), &fieldsMap); err != nil {
		return nil
	}

	raw, ok := fieldsMap["github_pr"]
	if !ok {
		return nil
	}

	payload, err := json.Marshal(raw)
	if err != nil {
		return nil
	}

	var githubPR githubPRFields
	if err := json.Unmarshal(payload, &githubPR); err != nil {
		return nil
	}
	if githubPR.Number == 0 && githubPR.URL == "" && githubPR.Branch == "" && githubPR.Repo == "" {
		return nil
	}

	context := &ItemCodeContext{
		Provider: "github",
		Repo:     githubPR.Repo,
		Branch:   githubPR.Branch,
	}
	if githubPR.Number != 0 || githubPR.URL != "" || githubPR.Title != "" || githubPR.State != "" {
		context.PullRequest = &ItemPullRequestMetadata{
			Number:    githubPR.Number,
			URL:       githubPR.URL,
			Title:     githubPR.Title,
			State:     githubPR.State,
			UpdatedAt: githubPR.UpdatedAt,
		}
	}

	return context
}

type ItemCreate struct {
	Title     string  `json:"title"`
	Content   string  `json:"content,omitempty"`
	Fields    string  `json:"fields,omitempty"`
	Tags      string  `json:"tags,omitempty"`
	Pinned    bool    `json:"pinned,omitempty"`
	ParentID  *string `json:"parent_id,omitempty"`
	CreatedBy string  `json:"created_by,omitempty"`
	Source    string  `json:"source,omitempty"`
}

type ItemUpdate struct {
	Title          *string `json:"title,omitempty"`
	Content        *string `json:"content,omitempty"`
	Fields         *string `json:"fields,omitempty"`
	Tags           *string `json:"tags,omitempty"`
	Pinned         *bool   `json:"pinned,omitempty"`
	SortOrder      *int    `json:"sort_order,omitempty"`
	ParentID       *string `json:"parent_id,omitempty"`
	LastModifiedBy string  `json:"last_modified_by,omitempty"`
	Source         string  `json:"source,omitempty"`
	ChangeSummary  string  `json:"change_summary,omitempty"`
}

type ItemListParams struct {
	CollectionSlug  string
	Fields          map[string]string // field filters: key=value
	Sort            string            // e.g. "priority:desc,created_at:asc"
	GroupBy         string
	Search          string // FTS query
	ParentID        string
	Tag             string
	IncludeArchived bool
	Limit           int
	Offset          int
}

type ItemLink struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	SourceID    string    `json:"source_id"`
	TargetID    string    `json:"target_id"`
	LinkType    string    `json:"link_type"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`

	// Populated by joins
	SourceTitle          string `json:"source_title,omitempty"`
	TargetTitle          string `json:"target_title,omitempty"`
	SourceSlug           string `json:"source_slug,omitempty"`
	TargetSlug           string `json:"target_slug,omitempty"`
	SourceRef            string `json:"source_ref,omitempty"`
	TargetRef            string `json:"target_ref,omitempty"`
	SourceCollectionSlug string `json:"source_collection_slug,omitempty"`
	TargetCollectionSlug string `json:"target_collection_slug,omitempty"`
	SourceStatus         string `json:"source_status,omitempty"`
	TargetStatus         string `json:"target_status,omitempty"`
}

type ItemLinkCreate struct {
	TargetID  string `json:"target_id"`
	LinkType  string `json:"link_type,omitempty"`
	CreatedBy string `json:"created_by,omitempty"`
}
