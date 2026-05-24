package models

// Backlink is the wire shape returned by
// `GET /api/v1/workspaces/{ws}/items/{ref}/backlinks` — one inbound
// `[[...]]` reference to the queried item. The store layer populates
// these from `item_wiki_links` joined to `items` + `collections`;
// the HTTP layer applies visibility filtering; the CLI / web UI / MCP
// render them as one-line "Mentioned in" entries.
//
// Lives in models (not store) because both `internal/cli/client.go`
// and `internal/server/handlers_backlinks.go` deserialize/serialize
// it across the wire, and models is the canonical home for shared
// JSON shapes.
//
// PLAN-1593 / TASK-1594.
type Backlink struct {
	// SourceItemID is the UUID of the linking item.
	SourceItemID string `json:"source_item_id"`

	// SourceRef is "PREFIX-NUMBER" (e.g. "TASK-42"). Always set;
	// the renderer uses it as the primary identifier on each row.
	SourceRef string `json:"source_ref"`

	// SourceTitle is the linking item's title at query time.
	SourceTitle string `json:"source_title"`

	// SourceCollectionSlug + SourceCollectionIcon let the renderer
	// pick a route prefix and visual badge without a second
	// round-trip per backlink.
	SourceCollectionSlug string `json:"source_collection_slug"`
	SourceCollectionIcon string `json:"source_collection_icon"`

	// Snippet is ~80 chars of the source item's body centered on
	// the link's position, with internal newlines collapsed to
	// spaces. Empty when the source item has no body.
	Snippet string `json:"snippet"`

	// DisplayText is the [[X|Display]] override the link author
	// supplied, nil when the link was a bare `[[X]]` (no pipe).
	// A non-nil pointer to "" represents the editor-distinct case
	// `[[X|]]` — explicit empty display. Pointer (not omitempty
	// string) so JSON serialization preserves the distinction:
	// nil → field omitted, "" → field present and empty. Codex
	// round-13 P2.
	DisplayText *string `json:"display_text,omitempty"`

	// UpdatedAt is the source item's updated_at, RFC3339 string.
	// Used for relative timestamps ("3h ago"); the list is
	// already ordered most-recent first.
	UpdatedAt string `json:"updated_at"`
}
