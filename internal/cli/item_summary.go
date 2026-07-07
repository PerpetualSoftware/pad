package cli

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// ItemSummary is the agent-friendly, token-light projection of
// models.Item used by `pad item list` JSON output by default (TASK-2000).
//
// It drops the single biggest cost in a list response — the full rich-text
// `content` body, which is ~half the bytes of a typical `item list --format
// json` — replacing it with a short `content_preview`. It also drops the
// redundant UUID plumbing (id / workspace_id / collection_id / *_user_id /
// parent_id / agent_role_id) and the duplicate collection/parent join fields
// that only repeat data already addressable via slugs/refs. `fields` and
// `tags` are emitted as nested JSON (not escaped strings) so agents can read
// them without a second parse.
//
// The full shape (raw models.Item) is still available via `item list --full`.
type ItemSummary struct {
	Ref            string          `json:"ref,omitempty"`
	Title          string          `json:"title"`
	Slug           string          `json:"slug"`
	CollectionSlug string          `json:"collection_slug,omitempty"`
	ItemNumber     *int            `json:"item_number,omitempty"`
	Fields         json.RawMessage `json:"fields,omitempty"`
	Tags           json.RawMessage `json:"tags,omitempty"`
	Pinned         bool            `json:"pinned,omitempty"`
	ContentPreview string          `json:"content_preview,omitempty"`
	ParentRef      string          `json:"parent_ref,omitempty"`
	AssignedUser   string          `json:"assigned_user,omitempty"`
	AgentRole      string          `json:"agent_role,omitempty"`
	HasChildren    bool            `json:"has_children,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// contentPreviewLimit caps the content_preview at a small, agent-friendly
// length. A preview exists so an agent can recognize an item without pulling
// the whole body; the full body is one `pad item show <ref>` away.
const contentPreviewLimit = 200

// contentPreview returns the first non-empty line of the body (markdown
// stripped of a leading heading marker), truncated to contentPreviewLimit
// runes with an ellipsis when longer. Empty content yields "".
func contentPreview(content string) string {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return ""
	}
	// Take the first non-blank line so the preview is a coherent snippet
	// rather than a mid-sentence cut across a blank line.
	line := trimmed
	for _, l := range strings.Split(trimmed, "\n") {
		if s := strings.TrimSpace(l); s != "" {
			line = s
			break
		}
	}
	// Drop a leading markdown heading marker for readability.
	line = strings.TrimLeft(line, "#")
	line = strings.TrimSpace(line)

	runes := []rune(line)
	if len(runes) > contentPreviewLimit {
		return strings.TrimSpace(string(runes[:contentPreviewLimit])) + "…"
	}
	return line
}

// rawJSONOrNil returns s as json.RawMessage when it is a non-empty, non-empty-
// container JSON value, else nil (so the field is omitted). Guards against
// json.RawMessage("") — which is invalid JSON — and drops the noise of empty
// "{}" / "[]" so the summary stays lean.
func rawJSONOrNil(s string) json.RawMessage {
	t := strings.TrimSpace(s)
	if t == "" || t == "{}" || t == "[]" || t == "null" {
		return nil
	}
	// Defensive: item.Fields/Tags are validated JSON on write, but a
	// malformed stored value must never break the whole list marshal
	// (json.Marshal of an invalid RawMessage errors). Fall back to
	// emitting the raw value as a JSON string so output stays valid.
	if !json.Valid([]byte(t)) {
		if b, err := json.Marshal(t); err == nil {
			return json.RawMessage(b)
		}
		return nil
	}
	return json.RawMessage(t)
}

// ToItemSummary projects a single models.Item into the summary shape.
func ToItemSummary(item models.Item) ItemSummary {
	return ItemSummary{
		Ref:            item.Ref,
		Title:          item.Title,
		Slug:           item.Slug,
		CollectionSlug: item.CollectionSlug,
		ItemNumber:     item.ItemNumber,
		Fields:         rawJSONOrNil(item.Fields),
		Tags:           rawJSONOrNil(item.Tags),
		Pinned:         item.Pinned,
		ContentPreview: contentPreview(item.Content),
		ParentRef:      item.ParentRef,
		AssignedUser:   item.AssignedUserName,
		AgentRole:      item.AgentRoleSlug,
		HasChildren:    item.HasChildren,
		CreatedAt:      item.CreatedAt,
		UpdatedAt:      item.UpdatedAt,
	}
}

// ToItemSummaries projects a slice of items into summary shape, preserving
// order. A nil input yields a non-nil empty slice so the JSON encodes as `[]`.
func ToItemSummaries(items []models.Item) []ItemSummary {
	out := make([]ItemSummary, 0, len(items))
	for _, it := range items {
		out = append(out, ToItemSummary(it))
	}
	return out
}
