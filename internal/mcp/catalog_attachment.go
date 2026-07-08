package mcp

// padAttachmentTool exposes READ-ONLY attachment metadata — list the
// attachments in a workspace and inspect a single attachment's headers
// (MIME, size, filename, ETag). Both actions are pure reads.
//
// Why a dedicated tool rather than actions on pad_item: an attachment
// is its own resource (workspace-scoped, optionally item-linked), not a
// property of an item — folding list/show into pad_item would force yet
// more entries onto an already-large action enum and conflate item CRUD
// with attachment queries. The HTTP dispatcher already routes the
// `attachment list` / `attachment show` cmdPaths as first-class commands
// (dispatch_http_attachments.go); this tool just surfaces them.
//
// Deliberately read-only: upload / download / view are CLI-only
// (filesystem-bound) and excluded from the MCP surface per the catalog's
// exclusion rules — same rationale HTTPHandlerDispatcher's
// noRemoteEquivalent table encodes. The base64 image RESOURCE for
// multimodal agents is tracked separately (TASK-2076).

func init() {
	appendToCatalog(padAttachmentTool)
}

var padAttachmentTool = ToolDef{
	Name:        "pad_attachment",
	Description: padAttachmentToolDescription,
	Schema: ToolSchema{
		Workspace: true,
		Params: []ParamDef{
			// list filters
			{
				Name:        "item",
				Type:        "string",
				Description: "Filter to attachments on this item ref (e.g. TASK-5) or slug. Optional for action=list. Resolved to a UUID before querying.",
			},
			{
				Name:        "category",
				Type:        "string",
				Description: "Filter by attachment category (e.g. \"image\"). Optional for action=list.",
			},
			{
				Name:        "collection",
				Type:        "string",
				Description: "Filter to attachments on items in this collection, by collection UUID (NOT slug — the value is matched against the raw collection_id). Optional for action=list.",
			},
			{
				Name:        "attached",
				Type:        "bool",
				Description: "Only attachments linked to some item. Optional for action=list. Mutually exclusive with `unattached`.",
			},
			{
				Name:        "unattached",
				Type:        "bool",
				Description: "Only orphan attachments not linked to any item. Optional for action=list. Mutually exclusive with `attached` and `item`.",
			},
			{
				Name:        "sort",
				Type:        "string",
				Description: "Sort order for action=list (e.g. created_at_desc). Optional.",
			},
			{
				Name:        "limit",
				Type:        "number",
				Description: "Maximum results for action=list. Optional.",
			},
			{
				Name:        "offset",
				Type:        "number",
				Description: "Pagination offset for action=list. Optional.",
			},
			// show
			{
				Name:        "attachment_id",
				Type:        "string",
				Description: "The attachment's ID. Required for action=show.",
			},
			{
				Name:        "variant",
				Type:        "string",
				Description: "Request a derived variant (thumb-sm | thumb-md) for action=show. Optional.",
			},
		},
	},
	Actions: map[string]ActionFn{
		"list": passThrough([]string{"attachment", "list"}),
		"show": passThrough([]string{"attachment", "show"}),
	},
}

const padAttachmentToolDescription = `Read-only attachment metadata.

Actions:
  list  — List attachments in the workspace with optional filters.
          Required: workspace.
          Optional: item (ref/slug), category, collection (collection
          UUID, not slug), attached, unattached, sort, limit, offset.
          (attached / unattached are mutually exclusive; item and
          unattached are mutually exclusive.)
  show  — Inspect one attachment's metadata (MIME, size, filename, ETag,
          last-modified) via a HEAD request — no bytes transferred.
          Required: workspace, attachment_id.
          Optional: variant (thumb-sm | thumb-md).

Use pad_attachment to discover what files are attached to items and to
size / confirm an attachment before deciding what to do with it. This
surface is read-only: uploading, downloading, and viewing raw bytes are
CLI-only (filesystem-bound); an agent that needs the bytes fetches them
from the attachment URL directly.`
