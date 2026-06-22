package mcp

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// padItemTool is the v0.2 tool that consolidates the ~20 v0.1 verb
// tools (item_create, item_update, item_block, item_star, ...) into
// one resource × action shape. Largest single ToolDef in the catalog.
//
// Most actions are passThrough — they forward the input verbatim to
// the underlying CLI cmdPath. Two actions are custom:
//
//   - link / unlink — dispatch on link_type to one of several CLI
//     commands (item block / supersedes / implements / split-from /
//     blocked-by, plus their inverses). The custom handler reshapes
//     the uniform schema (`ref`, `target`) into the per-link-type
//     arg names the CLI expects (source_ref/target_ref, new_ref/old_ref,
//     etc.).
//
// Read-modify-write semantics for action=update are handled by the
// HTTPHandlerDispatcher's existing item-update route mapper (TASK-967);
// the catalog just forwards the input.

func init() {
	appendToCatalog(padItemTool)
}

var padItemTool = ToolDef{
	Name:        "pad_item",
	Description: padItemToolDescription,
	Schema: ToolSchema{
		Workspace: true,
		Params:    padItemSchemaParams,
	},
	Actions: map[string]ActionFn{
		// Lifecycle
		"create":  passThrough([]string{"item", "create"}),
		"update":  passThrough([]string{"item", "update"}),
		"delete":  passThrough([]string{"item", "delete"}),
		"get":     passThrough([]string{"item", "show"}),
		"list":    passThrough([]string{"item", "list"}),
		"move":    passThrough([]string{"item", "move"}),
		"restore": passThrough([]string{"item", "restore"}),

		// Relationships — link/unlink fan out to per-type cmdPaths.
		"link":   actionItemLink,
		"unlink": actionItemUnlink,
		"deps":   passThrough([]string{"item", "deps"}),

		// Stars
		"star":    passThrough([]string{"item", "star"}),
		"unstar":  passThrough([]string{"item", "unstar"}),
		"starred": passThrough([]string{"item", "starred"}),

		// Comments. reply-comment isn't a separate CLI verb — the
		// `comment` command takes a --reply-to flag, exposed here as
		// the `reply_to` parameter.
		"comment":       passThrough([]string{"item", "comment"}),
		"list-comments": passThrough([]string{"item", "comments"}),

		// Backlinks ("Mentioned in") — PLAN-1593 / TASK-1596.
		// Returns inbound `[[...]]` references to the given item.
		// Same-workspace + cross-workspace rows are unioned in the
		// response; cross-ws rows carry `source_workspace_slug`.
		// The CLI command takes a ref and optional --limit/--offset
		// flags; passThrough handles both. PLAN-1593 / TASK-1596.
		"backlinks": passThrough([]string{"item", "backlinks"}),

		// Bulk + notes + decisions
		// bulk-update is custom because the CLI takes repeatable
		// positional refs (one or more); our uniform schema exposes
		// scalar `ref` everywhere else, so the action accepts a
		// dedicated `refs: array<string>` and translates to the
		// repeatable form BuildCLIArgs expects.
		"bulk-update": actionItemBulkUpdate,
		"note":        passThrough([]string{"item", "note"}),
		"decide":      passThrough([]string{"item", "decide"}),

		// Artifact export / import (PLAN artifact export/import, Phase 5).
		// Both are CUSTOM: the CLI defaults are filesystem-oriented
		// (export writes a file, import reads a file) which is useless to
		// an MCP caller. export forces `-o -` so the artifact bytes come
		// back as the tool result; import writes the supplied body to a
		// temp file because ExecDispatcher doesn't pipe stdin to the
		// subprocess. See actionItemExport / actionItemImport below.
		"export": actionItemExport,
		"import": actionItemImport,
	},
}

// padItemSchemaParams is the union of every parameter any pad_item
// action accepts. Per-action requirements are documented in the
// description and enforced at dispatch time by BuildCLIArgs (which
// errors when a CLI arg's `required` flag isn't satisfied).
//
// We deliberately do NOT use JSON Schema oneOf here. mcp-go's helpers
// don't expose discriminated unions cleanly, and the description-led
// approach gives Claude / Cursor enough signal to call correctly while
// keeping the schema simple to maintain.
var padItemSchemaParams = []ParamDef{
	// ── Targeting ──
	{Name: "ref", Type: "string", Description: "Item reference (e.g. TASK-5, IDEA-12, PLAYB-3, CONVE-7). Required for: update, delete, restore, get, move, link, unlink, deps, star, unstar, comment, list-comments, note, decide, export. NOT used for bulk-update — pass `refs` (array) instead."},
	{Name: "refs", Type: "array<string>", Description: "Item references for batch operations. Required for: bulk-update (one or more refs)."},
	{Name: "target", Type: "string", Description: "The OTHER end of a relationship. Required for: link, unlink (paired with `ref` and `link_type`). For link_type=blocks, target is the item being blocked; for blocked-by it's the blocker; for supersedes it's the superseded item; etc."},
	{Name: "link_type", Type: "string", Description: "Type of relationship for action=link/unlink.", Enum: []string{"blocks", "blocked-by", "supersedes", "implements", "split-from"}},

	// ── Item content ──
	{Name: "collection", Type: "string", Description: "Collection slug (e.g. \"tasks\", \"ideas\"). Required for: create. Optional filter for: list."},
	{Name: "title", Type: "string", Description: "Item title. Required for: create. Optional rename for: update."},
	{Name: "content", Type: "string", Description: "Markdown body. Optional for: create, update."},

	// ── Artifact export / import ── (Phase 5)
	// `artifact`: the full portable artifact text (YAML frontmatter +
	// Markdown body) that `import` ingests as a new draft. Distinct from
	// `content` (which is only the item's Markdown body) because the
	// artifact carries the frontmatter the server needs to reconstruct
	// the item's collection + typed fields. `export` returns this same
	// text as its tool result.
	{Name: "artifact", Type: "string", Description: "Full portable artifact text (YAML frontmatter + Markdown body). Required for: import — this is the artifact a prior `export` produced. NOT the same as `content` (which is just the item's Markdown body)."},

	// ── Status / priority / scheduling ──
	{Name: "status", Type: "string", Description: "Item status (collection-specific enum). Optional for: create, update, list filter, bulk-update."},
	{Name: "priority", Type: "string", Description: "Item priority. Optional for: create, update, list filter, bulk-update."},
	{Name: "category", Type: "string", Description: "Item category. Optional for: create, update."},

	// ── Hierarchy / assignment ──
	{Name: "parent", Type: "string", Description: "Parent item ref (e.g. PLAN-3). Optional for: create, update, list filter."},
	{Name: "role", Type: "string", Description: "Agent role slug to assign (e.g. implementer). Optional for: create, update, list filter."},
	{Name: "assign", Type: "string", Description: "User name or email to assign. Optional for: create, update, list filter."},

	// ── Tagging ──
	// `tags`: agents naturally pass a JSON array of strings (e.g.
	// `["v1","frontend"]`); the create/update handlers also accept the
	// canonical JSON-encoded string form ItemCreate/ItemUpdate store
	// internally. Pre-BUG-1432 the description said "Comma-separated"
	// which mismatched both the CLI flag's help ("JSON array of tags")
	// AND the column shape (JSONB on Postgres) — agents passing
	// "foo,bar" produced corrupt rows on SQLite and HTTP 500s on
	// Postgres (JSONB rejects non-JSON).
	{Name: "tags", Type: "array<string>", Description: "Tags as a JSON array of strings, e.g. [\"v1\",\"frontend\"]. Optional for: create, update."},
	// `field`: the escape hatch for SCHEMA-DECLARED custom fields.
	// Use the dedicated top-level params (`status`, `priority`,
	// `category`, `parent`, `role`, `assign`, `tags`) for the named
	// fields — the dispatcher rolls those into the `fields` JSON
	// automatically and `field` is only needed for fields that
	// don't have a dedicated parameter.
	//
	// Pre-BUG-1431 the description was the bare "Custom field
	// key=value pairs (repeatable)." Agents seeing that often tried
	// `field: {status: ...}` to override status (the wrong placement)
	// or just guessed shapes when other input shapes errored. The
	// expanded description names the dedicated top-level params so
	// agents don't fall back to `field` for them.
	{Name: "field", Type: "array<string>", Description: "Custom field setters for SCHEMA-DECLARED fields without a dedicated top-level param. Array of \"key=value\" strings (e.g. [\"due_date=2026-06-01\",\"effort=l\"]). For status/priority/category/parent/role/assign/tags use the dedicated top-level param instead. Optional for: create, update, list filter, move."},

	// ── List / starred ──
	{Name: "all", Type: "bool", Description: "Include archived/done items in list responses. Optional for: list, starred."},
	{Name: "limit", Type: "number", Description: "Maximum results. Optional for: list, backlinks. Backlinks defaults to 50, max 300."},
	{Name: "offset", Type: "number", Description: "Skip the first N results (paging). Optional for: backlinks."},
	{Name: "sort", Type: "string", Description: "Sort field. Optional for: list."},
	{Name: "group_by", Type: "string", Description: "Group-by field. Optional for: list."},

	// ── Move ──
	{Name: "target_collection", Type: "string", Description: "Destination collection slug for action=move. Required for: move."},

	// ── Comments ──
	{Name: "message", Type: "string", Description: "Comment body. Required for: comment."},
	{Name: "reply_to", Type: "string", Description: "Parent comment ID for threading replies. Optional for: comment."},
	{Name: "comment", Type: "string", Description: "Audit comment explaining the change. Optional for: update."},

	// ── Guard override ── (IDEA-1494)
	// update/bulk-update reject non-terminal → terminal done-field
	// transitions while the item still has open (non-terminal)
	// children, surfacing a 409 with code=open_children plus a
	// machine-readable details.open_children array (one entry per
	// blocking child: {ref, title, status, collection_slug}). Setting
	// force=true skips the guard and still records the transition.
	// The same flag exists on `pad item update --force` so the CLI
	// and MCP escape hatches are identical.
	{Name: "force", Type: "bool", Description: "Override the open-children guard. Optional for: update, bulk-update, move. When the server returns code=open_children, details.open_children lists the blocking child refs so an agent can ship them and retry — or set force=true if the children should be intentionally orphaned."},

	// ── Notes / decisions ──
	{Name: "summary", Type: "string", Description: "Short note headline. Required for: note."},
	{Name: "details", Type: "string", Description: "Long-form note body. Optional for: note."},
	{Name: "decision", Type: "string", Description: "Decision summary text. Required for: decide."},
	{Name: "rationale", Type: "string", Description: "Reasoning behind the decision. Optional for: decide."},
}

const padItemToolDescription = `Item operations — the consolidated CRUD + relationships + comments + notes surface.

Actions:
  create        — Create a new item.
                  Required: collection, title.
                  Optional: status, priority, category, content, parent, role, assign, tags, field.
                  Use the dedicated top-level params (status / priority / category / parent
                  / role / assign / tags) for those named fields — the dispatcher rolls
                  them into the item's fields JSON automatically. The 'field' param is
                  the escape hatch for SCHEMA-DECLARED custom fields without a dedicated
                  param; accepts an array of "key=value" strings.
                  The 'tags' param accepts a JSON array of strings (e.g. ["v1","frontend"])
                  — NOT a comma-separated string.
  update        — Update an item by ref.
                  Required: ref. At least one mutable field.
                  Optional: title, status, priority, content, role, assign, parent, comment, tags.
                  Same placement rules as create.
  delete        — Archive an item.
                  Required: ref.
  restore       — Un-archive (restore) a soft-deleted item by ref.
                  Required: ref.
  get           — Read an item.
                  Required: ref.
  list          — List items, optionally filtered.
                  Optional: collection, status, priority, parent, role, assign, all, limit.
  move          — Move an item to a different collection.
                  Required: ref, target_collection.
  link          — Create a relationship between two items.
                  Required: ref, target, link_type.
                  link_type: blocks | blocked-by | supersedes | implements | split-from.
  unlink        — Remove a relationship.
                  Required: ref, target, link_type.
  deps          — Show all dependencies (incoming + outgoing) for an item.
                  Required: ref.
  star          — Star an item for quick access.
                  Required: ref.
  unstar        — Remove star.
                  Required: ref.
  starred       — List starred items.
                  Optional: all.
  comment       — Add a comment.
                  Required: ref, message.
                  Optional: reply_to (comment ID for threaded reply).
  list-comments — List comments on an item.
                  Required: ref.
  backlinks     — List inbound [[...]] references to an item ("Mentioned in").
                  Required: ref.
                  Optional: limit (default 50, max 300), offset.
                  Returns same-workspace rows first, then cross-workspace
                  rows (each cross-ws row carries source_workspace_slug).
                  Use this when you need to answer "what other items
                  reference TASK-5?" without scanning the full content
                  corpus.
  bulk-update   — Update status/priority across multiple items.
                  Required: refs (array of refs, e.g. ["TASK-5", "TASK-8"]),
                  AND at least one of status / priority.
  note          — Append an implementation note to an item.
                  Required: ref, summary.
                  Optional: details.
  decide        — Record a decision on an item.
                  Required: ref, decision.
                  Optional: rationale.
  export        — Export a playbook or convention as a portable artifact.
                  Required: ref (PLAYB-N / CONVE-N, or slug).
                  Returns the artifact TEXT (YAML frontmatter + Markdown
                  body) as the tool result — ready to hand to import in
                  another workspace. Only playbooks and conventions are
                  exportable; the server rejects any other item type.
                  Read-only / side-effect-free.
  import        — Import a portable artifact as a new DRAFT item.
                  Required: artifact (the full artifact text a prior
                  export produced).
                  Creates a playbook or convention draft (the server
                  gates by the artifact's collection) and returns
                  {ref, slug, warnings}. The item lands as a draft —
                  review and activate it afterward. Mutating but not
                  destructive (creates an item, like create).

ALWAYS prefer issue refs (TASK-5, IDEA-12) over slugs.

When updating status, include comment="why" so the audit trail tells the team
WHY the status changed, not just THAT it did.

For cross-item search use pad_search.action=query — pad_item.list filters within
a single query but doesn't do FTS scoring.`

// itemLinkOp describes one direction (link or unlink) of a link_type
// — the cmdPath to invoke, the snake_case input keys the CLI's
// positional args expect, and whether to swap the user's
// (ref, target) tuple before mapping.
//
// Per-direction (rather than per-link_type) because some link_types
// have asymmetric directions: `blocked-by` uses different cmdPaths
// AND different positional arg names for create vs delete (its delete
// shares with `blocks`).
type itemLinkOp struct {
	// cmdPath is the cmdhelp command path for this direction. Nil
	// means the direction isn't supported for the link_type.
	cmdPath []string

	// firstArg is the snake_case input key the CLI's first positional
	// expects (e.g. "source_ref" for `item block`).
	firstArg string

	// secondArg is the snake_case input key for the CLI's second
	// positional (e.g. "target_ref" for `item block`).
	secondArg string

	// inverted, when true, swaps the user's (ref, target) tuple
	// before mapping to (firstArg, secondArg). Required when the
	// catalog's user-facing direction (e.g. "A blocked-by B") differs
	// from the underlying graph direction the cmdPath models (e.g.
	// `pad item unblock` removes "B blocks A" → operands swapped).
	inverted bool
}

// itemLinkRoute pairs a link_type's create direction with its delete
// direction. Either op can have a nil cmdPath if the operation isn't
// supported for that type.
type itemLinkRoute struct {
	link   itemLinkOp
	unlink itemLinkOp
}

// itemLinkRoutes is the dispatch table for actionItemLink /
// actionItemUnlink. Keys are link_type values from the catalog schema.
//
// Behavioral notes:
//
//   - blocks / blocked-by: the underlying graph edge is directional
//     ("X blocks Y"). blocked-by is the same edge expressed from the
//     other side ("Y blocked-by X"); link_type encodes the user's
//     mental model. blocked-by's UNLINK reuses the `item unblock`
//     command (the create wasn't a separate edge type) but with
//     operands swapped because unblock takes (source, target) where
//     source is the blocker.
//   - supersedes / implements / split-from: each have their own
//     create + delete commands with type-specific arg names; the
//     positional names are the same in both directions.
//
// Edges that exist in the CLI but aren't exposed here:
//   - decides: standalone action (pad_item.action: decide), not a link
//     because it carries decision text + rationale that don't fit the
//     ref/target shape.
//   - related: read-only listing, no create/delete; consumers use
//     pad_search or pad_item.action: deps for graph traversal.
var itemLinkRoutes = map[string]itemLinkRoute{
	"blocks": {
		link:   itemLinkOp{cmdPath: []string{"item", "block"}, firstArg: "source_ref", secondArg: "target_ref"},
		unlink: itemLinkOp{cmdPath: []string{"item", "unblock"}, firstArg: "source_ref", secondArg: "target_ref"},
	},
	"blocked-by": {
		link: itemLinkOp{cmdPath: []string{"item", "blocked-by"}, firstArg: "source_ref", secondArg: "blocker_ref"},
		// User intent: "ref blocked-by target" = "target blocks ref".
		// Unlink reuses `pad item unblock SOURCE TARGET`. Operands
		// must swap so SOURCE is the blocker (target) and TARGET is
		// the blocked item (ref).
		unlink: itemLinkOp{cmdPath: []string{"item", "unblock"}, firstArg: "source_ref", secondArg: "target_ref", inverted: true},
	},
	"supersedes": {
		link:   itemLinkOp{cmdPath: []string{"item", "supersedes"}, firstArg: "new_ref", secondArg: "old_ref"},
		unlink: itemLinkOp{cmdPath: []string{"item", "unsupersede"}, firstArg: "new_ref", secondArg: "old_ref"},
	},
	"implements": {
		link:   itemLinkOp{cmdPath: []string{"item", "implements"}, firstArg: "implementer_ref", secondArg: "target_ref"},
		unlink: itemLinkOp{cmdPath: []string{"item", "unimplements"}, firstArg: "implementer_ref", secondArg: "target_ref"},
	},
	"split-from": {
		link:   itemLinkOp{cmdPath: []string{"item", "split-from"}, firstArg: "child_ref", secondArg: "parent_ref"},
		unlink: itemLinkOp{cmdPath: []string{"item", "unsplit"}, firstArg: "child_ref", secondArg: "parent_ref"},
	},
}

// actionItemLink dispatches pad_item.action=link to the appropriate
// CLI cmdPath based on link_type. Renames the schema's uniform `ref`
// and `target` to the type-specific positional arg names (source_ref/
// target_ref, new_ref/old_ref, etc.) before calling env.Dispatch.
//
// Errors:
//   - missing or unknown link_type → structured error envelope with
//     the valid options, same shape as makeFanOutHandler's errMissingAction.
//   - missing ref or target → BuildCLIArgs catches it as a missing-
//     positional error.
func actionItemLink(ctx context.Context, input map[string]any, env ActionEnv) (*mcp.CallToolResult, error) {
	cmdPath, dispatchInput, err := resolveItemLink(input, true)
	if err != nil {
		return errStructured("pad_item.link", err), nil
	}
	return env.Dispatch(ctx, cmdPath, dispatchInput)
}

// actionItemUnlink is the symmetric un-create operation. Same routing
// rules; uses route.unlinkCmdPath instead of route.linkCmdPath.
func actionItemUnlink(ctx context.Context, input map[string]any, env ActionEnv) (*mcp.CallToolResult, error) {
	cmdPath, dispatchInput, err := resolveItemLink(input, false)
	if err != nil {
		return errStructured("pad_item.unlink", err), nil
	}
	return env.Dispatch(ctx, cmdPath, dispatchInput)
}

// resolveItemLink reads link_type/ref/target from input and returns
// the dispatch tuple for action=link (creating=true) or action=unlink
// (creating=false). Pure function — no side effects.
//
// Returns reshaped dispatchInput rather than mutating the caller's
// map. The catalog's `link_type`, `ref`, and `target` keys are
// dropped from the dispatch input and replaced with op.firstArg /
// op.secondArg per the route table. When op.inverted is true, the
// user's (ref, target) tuple is swapped before mapping.
func resolveItemLink(input map[string]any, creating bool) ([]string, map[string]any, error) {
	linkType, _ := input["link_type"].(string)
	if linkType == "" {
		return nil, nil, fmt.Errorf("link_type is required (one of: %s)",
			joinSorted(itemLinkRoutesKeys()))
	}
	route, ok := itemLinkRoutes[linkType]
	if !ok {
		return nil, nil, fmt.Errorf("unknown link_type %q (valid: %s)",
			linkType, joinSorted(itemLinkRoutesKeys()))
	}
	op := route.link
	if !creating {
		op = route.unlink
	}
	if op.cmdPath == nil {
		direction := "link"
		if !creating {
			direction = "unlink"
		}
		return nil, nil, fmt.Errorf("link_type %q does not support %s", linkType, direction)
	}
	ref, _ := input["ref"].(string)
	target, _ := input["target"].(string)
	if ref == "" {
		return nil, nil, fmt.Errorf("ref is required for link/unlink")
	}
	if target == "" {
		return nil, nil, fmt.Errorf("target is required for link/unlink")
	}
	first, second := ref, target
	if op.inverted {
		first, second = target, ref
	}

	// Build the dispatch input from scratch — we want to drop the
	// catalog-only keys (ref, target, link_type) and inject the
	// CLI-positional keys (op.firstArg, op.secondArg). Pass through
	// everything else (workspace, format, etc.).
	out := make(map[string]any, len(input))
	for k, v := range input {
		if k == "ref" || k == "target" || k == "link_type" {
			continue
		}
		out[k] = v
	}
	out[op.firstArg] = first
	out[op.secondArg] = second
	return op.cmdPath, out, nil
}

// itemLinkRoutesKeys returns the registered link_type values in
// stable sort order, used in error messages so users see the same
// listing every time.
func itemLinkRoutesKeys() []string {
	out := make([]string, 0, len(itemLinkRoutes))
	for k := range itemLinkRoutes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// joinSorted concatenates ss with ", " between values. Caller is
// expected to pass a pre-sorted slice (the only call site uses
// itemLinkRoutesKeys which sorts on the way out).
func joinSorted(ss []string) string {
	return strings.Join(ss, ", ")
}

// errStructured wraps a Go error into the standard MCP tool-error
// envelope (TASK-1077) with a stable identifier prefix (e.g.
// "pad_item.link"). Local helper — avoids leaking actionFn-specific
// formatting up to every caller.
//
// All catalog-level validation errors that flow through here go to
// ErrValidationFailed because every current call site is "the agent
// passed bad input" (missing link_type, missing refs, unknown
// link_type, etc.). If a future call site needs a different code,
// give it its own helper rather than overloading this one — keeps
// the code-per-call-site mapping explicit.
func errStructured(prefix string, err error) *mcp.CallToolResult {
	return NewErrorResult(ErrorPayload{
		Code:    ErrValidationFailed,
		Message: fmt.Sprintf("%s: %s", prefix, err.Error()),
		Hint:    "Check the input shape against the tool's schema.",
	})
}

// actionItemBulkUpdate translates the catalog's `refs: array<string>`
// param into the repeatable positional `ref` that BuildCLIArgs feeds
// to `pad item bulk-update REF [REF ...]`. The CLI's cmdhelp entry
// declares ref as Repeatable=true, so passing a []string under the
// `ref` key produces multiple positionals.
//
// We expose `refs` (plural) on the catalog rather than overloading
// `ref` (which is scalar across every other action) so the schema
// stays consistent — agents see a single shape per param name. The
// rename happens here at dispatch time.
func actionItemBulkUpdate(ctx context.Context, input map[string]any, env ActionEnv) (*mcp.CallToolResult, error) {
	rawRefs, ok := input["refs"]
	if !ok || rawRefs == nil {
		return errStructured("pad_item.bulk-update",
			fmt.Errorf("refs is required (array of item references)")), nil
	}
	refs, err := normalizeBulkUpdateRefs(rawRefs)
	if err != nil {
		return errStructured("pad_item.bulk-update", err), nil
	}
	if len(refs) == 0 {
		return errStructured("pad_item.bulk-update",
			fmt.Errorf("refs cannot be empty")), nil
	}
	out := make(map[string]any, len(input))
	for k, v := range input {
		if k == "refs" {
			continue
		}
		out[k] = v
	}
	// BuildCLIArgs expects []string under the cmd's positional name
	// (`ref`) when arg.Repeatable=true.
	out["ref"] = refs
	return env.Dispatch(ctx, []string{"item", "bulk-update"}, out)
}

// normalizeBulkUpdateRefs accepts the JSON shapes the MCP transport
// can deliver for an array<string> param: []string (canonical),
// []any (mcp-go's typical decoded form), or a single string (lenient
// fallback so an agent that passes one ref unwrapped still works).
func normalizeBulkUpdateRefs(raw any) ([]string, error) {
	switch v := raw.(type) {
	case []string:
		out := make([]string, 0, len(v))
		for _, s := range v {
			if s = trimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out, nil
	case []any:
		out := make([]string, 0, len(v))
		for i, e := range v {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("refs[%d] is %T, want string", i, e)
			}
			if s = trimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out, nil
	case string:
		// Lenient fallback: a single ref passed unwrapped. Logically
		// equivalent to a 1-element array.
		if s := trimSpace(v); s != "" {
			return []string{s}, nil
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("refs is %T, want array of strings", raw)
	}
}

// trimSpace is strings.TrimSpace inlined to avoid pulling strings
// through this file just for one call. catalog_item.go already
// imports strings for joinSorted, so the inline isn't strictly
// necessary — kept anyway because the call site is in a hot path
// (every bulk-update entry).
func trimSpace(s string) string {
	return strings.TrimSpace(s)
}

// actionItemExport handles pad_item.action=export. The CLI's
// `pad item export <ref>` defaults to WRITING A FILE (<slug>.pad.md),
// which is useless to an MCP caller — the bytes have to come back as
// the tool result. So the action forces the CLI's stdout sink
// (`-o -`): the CLI streams the artifact to stdout, ExecDispatcher
// captures stdout, and packageJSONResult surfaces it as the result.
//
// We inject `output: "-"` rather than asking agents to pass it, so the
// MCP surface stays "give me a ref, get the artifact back" with no
// filesystem semantics leaking through. An agent-supplied `output`
// would be a local-filesystem path the MCP host can't see, so we
// always override it.
//
// Read-only / side-effect-free: the server's export endpoint only
// reads the item.
func actionItemExport(ctx context.Context, input map[string]any, env ActionEnv) (*mcp.CallToolResult, error) {
	if ref, _ := input["ref"].(string); strings.TrimSpace(ref) == "" {
		return errStructured("pad_item.export",
			fmt.Errorf("ref is required (PLAYB-N, CONVE-N, or slug)")), nil
	}
	out := make(map[string]any, len(input)+1)
	for k, v := range input {
		out[k] = v
	}
	// Force stdout. cmdhelp's `item export` flag is `output` (shorthand
	// -o); BuildCLIArgs emits the long form, so this becomes
	// `--output -` and the CLI streams the artifact bytes to stdout.
	out["output"] = "-"
	return env.Dispatch(ctx, []string{"item", "export"}, out)
}

// actionItemImport handles pad_item.action=import. The CLI's
// `pad item import <file>` reads the artifact from a file (or stdin
// when the positional is `-`). ExecDispatcher does NOT pipe the MCP
// transport's data to the subprocess's stdin — cmd.Stdin is never set
// — so the stdin route isn't available here. Instead the action writes
// the supplied artifact body to a temp file, dispatches
// `item import <tmpfile>`, and removes the temp file afterward.
//
// The artifact body arrives in the `artifact` param (NOT `content`):
// `content` is just the item's Markdown body, whereas the artifact
// carries the YAML frontmatter the server needs to reconstruct the
// item's collection + typed fields.
//
// Mutating but not destructive — the server imports the artifact as a
// draft item (same risk profile as action=create), returning
// {ref, slug, warnings}.
func actionItemImport(ctx context.Context, input map[string]any, env ActionEnv) (*mcp.CallToolResult, error) {
	artifact, _ := input["artifact"].(string)
	if strings.TrimSpace(artifact) == "" {
		return errStructured("pad_item.import",
			fmt.Errorf("artifact is required (the full artifact text from a prior export)")), nil
	}

	// ExecDispatcher can't pipe stdin, so spill the artifact to a temp
	// file and hand the CLI a real path. Cleaned up unconditionally
	// after dispatch returns.
	tmp, err := os.CreateTemp("", "pad-artifact-*.pad.md")
	if err != nil {
		return errStructured("pad_item.import",
			fmt.Errorf("create temp artifact file: %w", err)), nil
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.WriteString(artifact); err != nil {
		tmp.Close()
		return errStructured("pad_item.import",
			fmt.Errorf("write temp artifact file: %w", err)), nil
	}
	if err := tmp.Close(); err != nil {
		return errStructured("pad_item.import",
			fmt.Errorf("close temp artifact file: %w", err)), nil
	}

	// Build the dispatch input from scratch: drop the catalog-only
	// `artifact` key and inject the CLI's positional `file`, which
	// BuildCLIArgs emits as the lone positional for `item import`.
	out := make(map[string]any, len(input))
	for k, v := range input {
		if k == "artifact" {
			continue
		}
		out[k] = v
	}
	out["file"] = tmpPath
	return env.Dispatch(ctx, []string{"item", "import"}, out)
}
