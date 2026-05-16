package models

import (
	"bytes"
	"encoding/json"
	"errors"
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

	// Assignment: (user, role) pair
	AssignedUserID *string `json:"assigned_user_id,omitempty"`
	AgentRoleID    *string `json:"agent_role_id,omitempty"`
	RoleSortOrder  int     `json:"role_sort_order"`

	// Auto-assigned sequential number within collection
	ItemNumber *int `json:"item_number,omitempty"`

	// Seq is a workspace-scoped monotonically-increasing sequence number
	// stamped on every mutation (create / update / soft-delete /
	// restore). It is the cursor mechanic for the local-first read
	// model's delta sync (PLAN-1343, DOC-1342 design decision #1).
	// Clients track the max seq they have seen and request
	// `?since=<seq>` deltas to resume. Robust against clock-skew /
	// same-millisecond-write / NTP-step correctness holes that an
	// `updated_at` watermark would carry.
	Seq int64 `json:"seq,omitempty"`

	// Populated by joins (not stored)
	AssignedUserName  string `json:"assigned_user_name,omitempty"`
	AssignedUserEmail string `json:"assigned_user_email,omitempty"`
	AgentRoleName     string `json:"agent_role_name,omitempty"`
	AgentRoleSlug     string `json:"agent_role_slug,omitempty"`
	AgentRoleIcon     string `json:"agent_role_icon,omitempty"`
	CollectionSlug    string `json:"collection_slug,omitempty"`
	CollectionName    string `json:"collection_name,omitempty"`
	CollectionIcon    string `json:"collection_icon,omitempty"`
	CollectionPrefix  string `json:"collection_prefix,omitempty"`

	// Parent link (populated by enrichItemForResponse / enrichItemsWithParent)
	ParentLinkID         string `json:"parent_link_id,omitempty"`
	ParentRef            string `json:"parent_ref,omitempty"`
	ParentTitle          string `json:"parent_title,omitempty"`
	ParentSlug           string `json:"parent_slug,omitempty"`
	ParentCollectionSlug string `json:"parent_collection_slug,omitempty"`

	// HasChildren is true if this item has child items linked to it.
	// Populated by enrichment, not stored in the DB.
	HasChildren bool `json:"has_children,omitempty"`

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

	// hasConventionShape tracks whether we've found a Convention-
	// SPECIFIC marker — the structured convention field, or one of
	// trigger / surfaces / scope / commands / direct enforcement.
	// `category` alone is NOT a Convention marker (Ideas, Bugs, Roadmap
	// items also use category). Used to gate the priority→enforcement
	// legacy fallback below; without this gate every Task/Idea with a
	// `priority` field got a phantom `convention.enforcement` surfaced
	// on its response (BUG-987 bug 13).
	hasConventionShape := false

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
				hasConventionShape = true
			}
		}
	}

	if metadata.Category == "" {
		if category, ok := fieldsMap["category"].(string); ok {
			metadata.Category = category
			hasMetadata = true
			// Note: category alone does NOT flip hasConventionShape —
			// many non-Convention collections legitimately use it.
		}
	}
	if metadata.Trigger == "" {
		if trigger, ok := fieldsMap["trigger"].(string); ok {
			metadata.Trigger = trigger
			hasMetadata = true
			hasConventionShape = true
		}
	}
	// Direct enforcement only — the priority fallback runs at the
	// END so surfaces/scope/commands have a chance to flip
	// hasConventionShape first. Without that ordering, a legacy
	// Convention like `{scope:"all", priority:"must"}` (no trigger)
	// would silently drop enforcement because the fallback ran
	// before scope set hasConventionShape.
	if metadata.Enforcement == "" {
		if value, ok := fieldsMap["enforcement"].(string); ok {
			metadata.Enforcement = value
			hasMetadata = true
			hasConventionShape = true
		}
	}
	if len(metadata.Surfaces) == 0 {
		if surfaces := extractStringList(fieldsMap["surfaces"]); len(surfaces) > 0 {
			metadata.Surfaces = surfaces
			hasMetadata = true
			hasConventionShape = true
		} else if scope, ok := fieldsMap["scope"].(string); ok && scope != "" {
			metadata.Surfaces = []string{scope}
			hasMetadata = true
			hasConventionShape = true
		}
	}
	if len(metadata.Commands) == 0 {
		if commands := extractStringList(fieldsMap["commands"]); len(commands) > 0 {
			metadata.Commands = commands
			hasMetadata = true
			hasConventionShape = true
		}
	}

	// Legacy priority→enforcement fallback. Runs AFTER all other
	// markers because hasConventionShape only flips once we've seen
	// a Convention-specific signal. Without this ordering, a legacy
	// Convention with only `{scope, priority}` would lose its
	// enforcement value because scope hadn't been processed yet
	// (Codex review on PR #361 caught this).
	if metadata.Enforcement == "" && hasConventionShape {
		if priority, ok := fieldsMap["priority"].(string); ok {
			metadata.Enforcement = priority
		}
	}

	if !hasMetadata {
		return nil
	}
	// Final guard: if we ONLY matched on `category` (no Convention-
	// specific markers), the item isn't a Convention. Suppress the
	// metadata entirely — surfacing { category } on a non-Convention
	// item just for category alone produced confusing responses.
	if !hasConventionShape {
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
	Title          string  `json:"title"`
	Content        string  `json:"content,omitempty"`
	Fields         string  `json:"fields,omitempty"`
	Tags           string  `json:"tags,omitempty"`
	Pinned         bool    `json:"pinned,omitempty"`
	ParentID       *string `json:"parent_id,omitempty"`
	AssignedUserID *string `json:"assigned_user_id,omitempty"`
	AgentRoleID    *string `json:"agent_role_id,omitempty"`
	CreatedBy      string  `json:"created_by,omitempty"`
	Source         string  `json:"source,omitempty"`
}

type ItemUpdate struct {
	Title          *string `json:"title,omitempty"`
	Content        *string `json:"content,omitempty"`
	Fields         *string `json:"fields,omitempty"`
	Tags           *string `json:"tags,omitempty"`
	Pinned         *bool   `json:"pinned,omitempty"`
	SortOrder      *int    `json:"sort_order,omitempty"`
	ParentID       *string `json:"parent_id,omitempty"`
	AssignedUserID *string `json:"assigned_user_id,omitempty"`
	AgentRoleID    *string `json:"agent_role_id,omitempty"`
	LastModifiedBy string  `json:"last_modified_by,omitempty"`
	Source         string  `json:"source,omitempty"`
	// VersionSource overrides the per-version-row Source attribution
	// without mutating `items.source`. When unset (the common case),
	// the version row inherits the same value as `items.source`
	// (whatever Source ends up being). When set, the version row
	// uses VersionSource and `items.source` is left alone — used by
	// the collab 5s-flush PATCH handler so an auto-flush doesn't
	// re-attribute a CLI/MCP-created item to "collab-snapshot" and
	// silently flip it out of `WorkspaceHasAgentActivity`'s count.
	// Per Codex review round 3 of TASK-1267 [P2].
	VersionSource string  `json:"version_source,omitempty"`
	ChangeSummary string  `json:"change_summary,omitempty"`
	Comment       *string `json:"comment,omitempty"`
	// OpLogCursor is the highest item_yjs_updates.id the calling client
	// has applied into its local Y.Doc (TASK-1319). Used by the
	// collab-snapshot flush PATCH to advance the op-log GC watermark
	// (`items.content_flushed_op_log_id`) when, and only when, the
	// cursor matches the current MAX(item_yjs_updates.id) for the item.
	//
	// **Why a pointer.** A nil cursor means "the caller didn't claim
	// to know"; the watermark stays put. *0 means "I have nothing"
	// (e.g. a fresh editor whose op-log is empty); when MAX is also 0
	// the watermark advances to 0 (a no-op stamp) — but practical
	// flushes always have *some* op-log id, so this branch rarely
	// matters.
	//
	// Only honoured when VersionSource == "collab-snapshot". Other
	// content writes (CLI / MCP / version restore / PruneAndApply)
	// already advance the watermark to MAX(op-log.id) at write time
	// because they reconstruct or replace items.content wholesale.
	// Per TASK-1319.
	OpLogCursor *int64 `json:"op_log_cursor,omitempty"`
	// ClearAssignedUser / ClearAgentRole allow explicitly setting to NULL
	// (since nil pointer means "don't change" in partial updates)
	ClearAssignedUser bool `json:"clear_assigned_user,omitempty"`
	ClearAgentRole    bool `json:"clear_agent_role,omitempty"`
}

// ErrInvalidFieldsType / ErrInvalidTagsType are returned by
// ItemUpdate.UnmarshalJSON AND ItemCreate.UnmarshalJSON when the
// inbound `fields` / `tags` value is neither a JSON-encoded string
// nor the natural object/array shape. Wire handlers surface the
// sentinel's message verbatim (without the "invalid JSON: ..." wrapper
// from decodeJSON) so callers see a clean domain-level error instead
// of leaked Go internals. See BUG-1144 (Update) and BUG-1432 (Create).
var (
	ErrInvalidFieldsType = errors.New(`"fields" must be a JSON object or a JSON-encoded string`)
	ErrInvalidTagsType   = errors.New(`"tags" must be a JSON array or a JSON-encoded string`)
)

// UnmarshalJSON for ItemCreate mirrors the flexible-shape behaviour
// ItemUpdate gained under BUG-1144: accept `fields` / `tags` either as
// the canonical JSON-encoded string shape (matches models.Item storage)
// OR as the natural nested object/array shape any reasonable HTTP
// client would send.
//
// Pre-BUG-1432 the Create path was brittle in two specific ways agents
// hit on Pad Cloud:
//
//   - `tags: ["foo","bar"]` (natural JSON-array shape) was rejected by
//     the default unmarshaler because the struct field is `string`,
//     yielding `"cannot unmarshal array into Go struct field
//     ItemCreate.tags of type string"` — an HTTP 400 the MCP
//     dispatcher then surfaced as a validation_failed envelope.
//
//   - `fields: {status: "open"}` (natural nested-object shape) hit the
//     same brittleness with `cannot unmarshal object into ... fields of
//     type string`.
//
// The asymmetry with ItemUpdate (which already handled both shapes
// cleanly per BUG-1144) was Codex's tip in the BUG-1432 investigation
// — fixing it here closes the create/update gap and gives agents a
// uniform shape contract across both verbs.
//
// In-process callers that construct ItemCreate{} literals never hit
// this path; only the JSON decode boundary changes.
func (c *ItemCreate) UnmarshalJSON(data []byte) error {
	// Use an alias to inherit every other field's default unmarshal
	// behaviour, while shadowing fields/tags with json.RawMessage so we
	// can inspect the raw shape. The outer fields shadow the embedded
	// alias's same-named fields because they are less deeply nested.
	type alias ItemCreate
	aux := struct {
		Fields json.RawMessage `json:"fields,omitempty"`
		Tags   json.RawMessage `json:"tags,omitempty"`
		*alias
	}{alias: (*alias)(c)}

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	// flexJSONToString returns *string (nil when absent / null / empty).
	// ItemCreate.Fields/Tags are plain strings, not pointers, so deref
	// when present and leave at zero value otherwise.
	if fieldsStr, err := flexJSONToString(aux.Fields, '{', ErrInvalidFieldsType); err != nil {
		return err
	} else if fieldsStr != nil {
		c.Fields = *fieldsStr
	}

	if tagsStr, err := flexJSONToString(aux.Tags, '[', ErrInvalidTagsType); err != nil {
		return err
	} else if tagsStr != nil {
		c.Tags = *tagsStr
	}

	return nil
}

// UnmarshalJSON accepts `fields` / `tags` either as the canonical
// JSON-encoded string shape (matches models.Item.Fields/Tags storage)
// OR as the natural nested object/array shape any reasonable HTTP
// client would send. The struct fields stay `*string` and the rest
// of the pipeline (validation, store writes, web/CLI consumers) is
// unchanged — we just normalize the wire input here.
//
// In-process callers that construct ItemUpdate{} literals never hit
// this path, so no internal call site needs to change.
//
// See BUG-1144 (input side) and BUG-991 (the symmetric response-side
// dual-emit, fixed at the MCP boundary in PR #364).
func (u *ItemUpdate) UnmarshalJSON(data []byte) error {
	// Use an alias to inherit every other field's default unmarshal
	// behaviour, while shadowing fields/tags with json.RawMessage so we
	// can inspect the raw shape. The outer fields shadow the embedded
	// alias's same-named fields because they are less deeply nested.
	type alias ItemUpdate
	aux := struct {
		Fields json.RawMessage `json:"fields,omitempty"`
		Tags   json.RawMessage `json:"tags,omitempty"`
		*alias
	}{alias: (*alias)(u)}

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	// The alias decode leaves u.Fields / u.Tags nil (the raw bytes were
	// captured at the outer level). Re-populate them from the flex parser.
	fieldsStr, err := flexJSONToString(aux.Fields, '{', ErrInvalidFieldsType)
	if err != nil {
		return err
	}
	u.Fields = fieldsStr

	tagsStr, err := flexJSONToString(aux.Tags, '[', ErrInvalidTagsType)
	if err != nil {
		return err
	}
	u.Tags = tagsStr

	return nil
}

// flexJSONToString accepts a raw JSON value and returns it as a
// canonical JSON-encoded string. Acceptable inbound shapes:
//
//   - absent / empty / null → nil (caller leaves the field unchanged)
//   - JSON-encoded string whose INNER content is either empty (the
//     legacy empty-string sentinel handled downstream by store-layer
//     coercion) OR a JSON value whose shape matches expectedStart
//     ('{' or '[')
//   - JSON object or array (matching expectedStart '{' or '[') → re-
//     marshal to string
//
// Any other shape returns errInvalid so the handler surfaces a clean
// domain-level error instead of a leaked Go unmarshal message.
//
// IDEA-1488 R1 codex hardening: the `case '"'` branch validates the
// INNER content's shape (not just the JSON-encoded-string envelope).
// Without this, `{"config": "[]"}` or `{"settings": "not json"}` would
// slip past — the outer string-shape check accepted any inner content
// verbatim, which defeated the shape-validation ceiling that IDEA-1488
// is supposed to add. The pre-existing ItemUpdate fields/tags path
// inherits the same tightening because it routes through this helper.
func flexJSONToString(raw json.RawMessage, expectedStart byte, errInvalid error) (*string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	switch trimmed[0] {
	case '"':
		// JSON-encoded string envelope — unmarshal to the underlying Go
		// string and then validate the inner content's shape matches
		// expectedStart. Subsequent code that does
		// json.Unmarshal([]byte(*s), ...) sees the inner JSON, not a
		// re-quoted string.
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, errInvalid
		}
		// Empty inner string is the empty-string sentinel; store-layer
		// coercion handles it (IDEA-1486). Don't reject here so callers
		// retain the legacy "" → default normalization shape.
		innerTrimmed := bytes.TrimSpace([]byte(s))
		if len(innerTrimmed) == 0 {
			return &s, nil
		}
		if innerTrimmed[0] != expectedStart {
			return nil, errInvalid
		}
		// Confirm the inner content actually parses as JSON of the
		// expected shape. Catches strings that start with the right
		// brace but are otherwise garbage (e.g. `"{not valid"`).
		var inner any
		if err := json.Unmarshal(innerTrimmed, &inner); err != nil {
			return nil, errInvalid
		}
		return &s, nil
	case expectedStart:
		// Object or array — re-marshal to a canonical JSON string so
		// the downstream string-typed pipeline can json.Unmarshal it
		// back to a map/slice exactly as if the caller had stringified.
		var v any
		if err := json.Unmarshal(trimmed, &v); err != nil {
			return nil, errInvalid
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, errInvalid
		}
		s := string(b)
		return &s, nil
	default:
		return nil, errInvalid
	}
}

type ItemListParams struct {
	CollectionSlug  string
	CollectionIDs   []string          // permission filter: restrict to these collection IDs (nil = no filter)
	ItemIDs         []string          // permission filter: additionally restrict to these item IDs (for item-level grants)
	Fields          map[string]string // field filters: key=value
	Sort            string            // e.g. "priority:desc,created_at:asc"
	GroupBy         string
	Search          string // FTS query
	ParentID        string
	Tag             string
	AssignedUserID  string // filter by assigned user
	AgentRoleID     string // filter by agent role (ID or slug)
	ParentLinkID    string // filter by parent link (item ID of the parent)
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
