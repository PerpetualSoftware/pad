package models

import (
	"encoding/json"
	"fmt"
	"time"
)

const (
	ItemFieldGitHubPR            = "github_pr"
	ItemFieldImplementationNotes = "implementation_notes"
	ItemFieldDecisionLog         = "decision_log"
	ItemFieldConvention          = "convention"
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
	CollectionSlug      string                   `json:"collection_slug,omitempty"`
	CollectionName      string                   `json:"collection_name,omitempty"`
	CollectionIcon      string                   `json:"collection_icon,omitempty"`
	CollectionPrefix    string                   `json:"collection_prefix,omitempty"`
	DerivedClosure      *ItemDerivedClosure      `json:"derived_closure,omitempty"`
	CodeContext         *ItemCodeContext         `json:"code_context,omitempty"`
	Convention          *ItemConventionMetadata  `json:"convention,omitempty"`
	ImplementationNotes []ItemImplementationNote `json:"implementation_notes,omitempty"`
	DecisionLog         []ItemDecisionLogEntry   `json:"decision_log,omitempty"`
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

type ItemConventionMetadata struct {
	Category    string   `json:"category,omitempty"`
	Trigger     string   `json:"trigger,omitempty"`
	Surfaces    []string `json:"surfaces,omitempty"`
	Enforcement string   `json:"enforcement,omitempty"`
	Commands    []string `json:"commands,omitempty"`
}

type ItemPullRequestMetadata struct {
	Number    int    `json:"number"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	State     string `json:"state"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

type ItemImplementationNote struct {
	ID        string `json:"id,omitempty"`
	Summary   string `json:"summary"`
	Details   string `json:"details,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	CreatedBy string `json:"created_by,omitempty"`
}

type ItemDecisionLogEntry struct {
	ID        string `json:"id,omitempty"`
	Decision  string `json:"decision"`
	Rationale string `json:"rationale,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	CreatedBy string `json:"created_by,omitempty"`
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

type conventionFields struct {
	Category    string   `json:"category"`
	Trigger     string   `json:"trigger"`
	Surfaces    []string `json:"surfaces"`
	Enforcement string   `json:"enforcement"`
	Commands    []string `json:"commands"`
}

func ExtractItemCodeContext(fieldsJSON string) *ItemCodeContext {
	fieldsMap, ok := parseItemFields(fieldsJSON)
	if !ok {
		return nil
	}

	raw, ok := fieldsMap[ItemFieldGitHubPR]
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

func ExtractItemConventionMetadata(fieldsJSON string) *ItemConventionMetadata {
	fieldsMap, ok := parseItemFields(fieldsJSON)
	if !ok {
		return nil
	}

	var metadata ItemConventionMetadata
	hasMetadata := false

	if raw, ok := fieldsMap[ItemFieldConvention]; ok {
		payload, err := json.Marshal(raw)
		if err == nil {
			var structured conventionFields
			if err := json.Unmarshal(payload, &structured); err == nil {
				metadata = ItemConventionMetadata{
					Category:    structured.Category,
					Trigger:     structured.Trigger,
					Surfaces:    append([]string(nil), structured.Surfaces...),
					Enforcement: structured.Enforcement,
					Commands:    append([]string(nil), structured.Commands...),
				}
				hasMetadata = true
			}
		}
	}

	if metadata.Category == "" {
		if category, ok := fieldsMap["category"].(string); ok {
			metadata.Category = category
			hasMetadata = true
		}
	}
	if metadata.Trigger == "" {
		if trigger, ok := fieldsMap["trigger"].(string); ok {
			metadata.Trigger = trigger
			hasMetadata = true
		}
	}
	if metadata.Enforcement == "" {
		switch value := fieldsMap["enforcement"].(type) {
		case string:
			metadata.Enforcement = value
			hasMetadata = true
		default:
			if priority, ok := fieldsMap["priority"].(string); ok {
				metadata.Enforcement = priority
				hasMetadata = true
			}
		}
	}
	if len(metadata.Surfaces) == 0 {
		if surfaces := extractStringList(fieldsMap["surfaces"]); len(surfaces) > 0 {
			metadata.Surfaces = surfaces
			hasMetadata = true
		} else if scope, ok := fieldsMap["scope"].(string); ok && scope != "" {
			metadata.Surfaces = []string{scope}
			hasMetadata = true
		}
	}
	if len(metadata.Commands) == 0 {
		if commands := extractStringList(fieldsMap["commands"]); len(commands) > 0 {
			metadata.Commands = commands
			hasMetadata = true
		}
	}

	if !hasMetadata {
		return nil
	}
	return normalizeItemConventionMetadata(&metadata)
}

func ExtractItemImplementationNotes(fieldsJSON string) []ItemImplementationNote {
	fieldsMap, ok := parseItemFields(fieldsJSON)
	if !ok {
		return nil
	}
	raw, ok := fieldsMap[ItemFieldImplementationNotes]
	if !ok {
		return nil
	}

	payload, err := json.Marshal(raw)
	if err != nil {
		return nil
	}

	var notes []ItemImplementationNote
	if err := json.Unmarshal(payload, &notes); err != nil {
		return nil
	}
	if len(notes) == 0 {
		return nil
	}
	return notes
}

func ExtractItemDecisionLog(fieldsJSON string) []ItemDecisionLogEntry {
	fieldsMap, ok := parseItemFields(fieldsJSON)
	if !ok {
		return nil
	}
	raw, ok := fieldsMap[ItemFieldDecisionLog]
	if !ok {
		return nil
	}

	payload, err := json.Marshal(raw)
	if err != nil {
		return nil
	}

	var entries []ItemDecisionLogEntry
	if err := json.Unmarshal(payload, &entries); err != nil {
		return nil
	}
	if len(entries) == 0 {
		return nil
	}
	return entries
}

func AppendImplementationNote(fieldsJSON string, note ItemImplementationNote) (string, error) {
	fieldsMap, err := parseMutableItemFields(fieldsJSON)
	if err != nil {
		return "", err
	}

	notes := ExtractItemImplementationNotes(fieldsJSON)
	notes = append(notes, note)
	fieldsMap[ItemFieldImplementationNotes] = notes
	return marshalItemFields(fieldsMap)
}

func AppendDecisionLogEntry(fieldsJSON string, entry ItemDecisionLogEntry) (string, error) {
	fieldsMap, err := parseMutableItemFields(fieldsJSON)
	if err != nil {
		return "", err
	}

	entries := ExtractItemDecisionLog(fieldsJSON)
	entries = append(entries, entry)
	fieldsMap[ItemFieldDecisionLog] = entries
	return marshalItemFields(fieldsMap)
}

func ApplyItemConventionMetadata(fieldsJSON string, metadata *ItemConventionMetadata) (string, error) {
	fieldsMap, err := parseMutableItemFields(fieldsJSON)
	if err != nil {
		return "", err
	}

	normalized := normalizeItemConventionMetadata(metadata)
	if normalized == nil {
		delete(fieldsMap, ItemFieldConvention)
		delete(fieldsMap, "category")
		delete(fieldsMap, "trigger")
		delete(fieldsMap, "scope")
		delete(fieldsMap, "priority")
		delete(fieldsMap, "enforcement")
		delete(fieldsMap, "surfaces")
		delete(fieldsMap, "commands")
		return marshalItemFields(fieldsMap)
	}

	fieldsMap[ItemFieldConvention] = normalized
	fieldsMap["category"] = normalized.Category
	fieldsMap["trigger"] = normalized.Trigger
	fieldsMap["enforcement"] = normalized.Enforcement
	fieldsMap["priority"] = normalized.Enforcement
	fieldsMap["surfaces"] = normalized.Surfaces
	fieldsMap["commands"] = normalized.Commands
	if len(normalized.Surfaces) > 0 {
		fieldsMap["scope"] = normalized.Surfaces[0]
	}

	return marshalItemFields(fieldsMap)
}

func BuildConventionItemFields(status string, metadata *ItemConventionMetadata) (string, error) {
	fieldsJSON, err := ApplyItemConventionMetadata("{}", metadata)
	if err != nil {
		return "", err
	}
	fieldsMap, err := parseMutableItemFields(fieldsJSON)
	if err != nil {
		return "", err
	}
	if status != "" {
		fieldsMap["status"] = status
	}
	return marshalItemFields(fieldsMap)
}

func parseItemFields(fieldsJSON string) (map[string]any, bool) {
	if fieldsJSON == "" || fieldsJSON == "{}" {
		return nil, false
	}
	var fieldsMap map[string]any
	if err := json.Unmarshal([]byte(fieldsJSON), &fieldsMap); err != nil {
		return nil, false
	}
	return fieldsMap, true
}

func parseMutableItemFields(fieldsJSON string) (map[string]any, error) {
	if fieldsJSON == "" || fieldsJSON == "{}" {
		return map[string]any{}, nil
	}
	var fieldsMap map[string]any
	if err := json.Unmarshal([]byte(fieldsJSON), &fieldsMap); err != nil {
		return nil, fmt.Errorf("parse item fields: %w", err)
	}
	return fieldsMap, nil
}

func marshalItemFields(fieldsMap map[string]any) (string, error) {
	payload, err := json.Marshal(fieldsMap)
	if err != nil {
		return "", fmt.Errorf("marshal item fields: %w", err)
	}
	return string(payload), nil
}

func normalizeItemConventionMetadata(metadata *ItemConventionMetadata) *ItemConventionMetadata {
	if metadata == nil {
		return nil
	}
	normalized := &ItemConventionMetadata{
		Category:    metadata.Category,
		Trigger:     metadata.Trigger,
		Enforcement: metadata.Enforcement,
		Surfaces:    uniqueStrings(metadata.Surfaces),
		Commands:    uniqueStrings(metadata.Commands),
	}
	if normalized.Category == "" && normalized.Trigger == "" && normalized.Enforcement == "" && len(normalized.Surfaces) == 0 && len(normalized.Commands) == 0 {
		return nil
	}
	return normalized
}

func extractStringList(raw any) []string {
	switch value := raw.(type) {
	case []string:
		return uniqueStrings(value)
	case []any:
		var out []string
		for _, entry := range value {
			if str, ok := entry.(string); ok && str != "" {
				out = append(out, str)
			}
		}
		return uniqueStrings(out)
	default:
		return nil
	}
}

func uniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
