package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/PerpetualSoftware/pad/internal/bodypatch"
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
	// The `fields` OBJECT param is a create/update writer (see
	// catalog_item_fields.go). Every other action wraps in a loud
	// refusal so a declared-but-meaningless `fields` can't flow to
	// dispatch and be silently dropped by BuildCLIArgs — the exact
	// failure mode the #1066 contract removes.
	for name, fn := range padItemTool.Actions {
		if name == "create" || name == "update" {
			continue
		}
		padItemTool.Actions[name] = rejectFieldsParam("pad_item."+name, fn)
	}
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
		// Lifecycle. create/update are CUSTOM (catalog_item_fields.go):
		// they fold an optional `fields` OBJECT into the dedicated-param
		// + `field`-array paths before dispatching (#1066).
		"create": actionItemCreate,
		"update": actionItemUpdate,
		"delete": passThrough([]string{"item", "delete"}),
		"get":    actionItemGet,

		// PLAN-3578: content-anchored body edits, answered with the diff.
		// CUSTOM (BUG-4082) so each edit's line_hint gets remote's rule.
		"patch": actionItemPatch,
		// list is CUSTOM (not passThrough) so a bare agent list stays
		// bounded: it injects a default limit and clamps an oversized one
		// before dispatch, mirroring the backlinks default/max. See
		// actionItemList. TASK-2000.
		"list":    actionItemList,
		"move":    passThrough([]string{"item", "move"}),
		"restore": passThrough([]string{"item", "restore"}),

		// Relationships — link/unlink fan out to per-type cmdPaths.
		"link":   actionItemLink,
		"unlink": actionItemUnlink,
		"deps":   passThrough([]string{"item", "deps"}),
		// PLAN-3598: one brief for working an item (item, parents with
		// acceptance criteria, matching conventions, deps, decisions, links).
		"context": passThrough([]string{"item", "context"}),

		// Reminders (IDEA-2641 / GitHub #1010). An agent that can RECEIVE a
		// reminder but not set one has half the primitive: deferring a piece
		// of work is exactly the moment an agent knows when it wants to be
		// asked again. `remind` arms; `ack-reminder` acknowledges a fired one
		// so it leaves pad_project's next/ready surface.
		//
		// `reminders` lists an item's reminders with their ids, and
		// `unremind` disarms one by id (TASK-3611): an agent that defers work
		// can now see and cancel what it armed without a terminal. Re-arm
		// stays CLI-only (`pad item remind --rearm`).
		// PR linking (TASK-3611): the agent supplies the PR's metadata and
		// nothing is fetched from GitHub, so it works with no checkout and
		// no `gh`. The server validates it as it does a fetched PR.
		"link-pr":   refAsItemRef([]string{"github", "link"}),
		"unlink-pr": refAsItemRef([]string{"github", "unlink"}),

		// `remind` is also the door to schedules (TASK-4105): any schedule
		// parameter makes it one. The rest of the schedule surface rides
		// `pad item remind <subcommand>`; see catalog_item_schedule.go.
		"remind":       actionItemRemind,
		"ack-reminder": actionItemAckReminder,
		"reminders":    passThrough([]string{"item", "reminders"}),
		"unremind":     passThrough([]string{"item", "unremind"}),

		"schedule":             passThrough([]string{"item", "remind", "show"}),
		"schedule-edit":        actionItemScheduleEdit,
		"schedule-forecast":    actionItemScheduleForecast,
		"schedule-delete":      actionItemScheduleDelete,
		"schedule-dismiss":     passThrough([]string{"item", "remind", "dismiss"}),
		"schedule-resolve":     actionItemScheduleResolve,
		"schedule-renegotiate": actionItemScheduleRenegotiate,
		"schedule-link":        actionItemScheduleGraph("schedule-link"),
		"schedule-unlink":      actionItemScheduleGraph("schedule-unlink"),
		"schedule-move":        actionItemScheduleGraph("schedule-move"),
		"schedule-gate":        actionItemScheduleGraph("schedule-gate"),
		"agenda":               actionItemAgenda,

		// Execution lease (#1221 / PR #1238). Agents are the motivating
		// users of the claim primitive — two pollers that both read
		// "unclaimed" get one winner and one structured 409 naming the
		// live holder — so the catalog carries it from the start
		// (maintainer's call on the PR thread). Additive, same
		// disposition as remind/ack-reminder above — and, like 0.28,
		// it OWNS a ToolSurfaceVersion bump (0.38, #1405): DR-7 is
		// about unknown keys in the serialized surface, not the action
		// enum, so adding an action is the contract change.
		"claim":   passThrough([]string{"item", "claim"}),
		"release": passThrough([]string{"item", "release"}),

		// Stars
		"star":    passThrough([]string{"item", "star"}),
		"unstar":  passThrough([]string{"item", "unstar"}),
		"starred": actionItemStarred,

		// Comments. reply-comment isn't a separate CLI verb — the
		// `comment` command takes a --reply-to flag, exposed here as
		// the `reply_to` parameter.
		"comment":       passThrough([]string{"item", "comment"}),
		"list-comments": passThrough([]string{"item", "comments"}),
		// TASK-2695: edit/delete ride the item-scoped comment routes, which
		// refuse a comment_id that is not on `ref` on every transport.
		"edit-comment":   passThrough([]string{"item", "comment-edit"}),
		"delete-comment": passThrough([]string{"item", "comment-delete"}),

		// Backlinks ("Mentioned in") — PLAN-1593 / TASK-1596.
		// Returns inbound `[[...]]` references to the given item.
		// Same-workspace + cross-workspace rows are unioned in the
		// response; cross-ws rows carry `source_workspace_slug`.
		// The CLI command takes a ref and optional --limit/--offset
		// flags; passThrough handles both. PLAN-1593 / TASK-1596.
		"backlinks": passThrough([]string{"item", "backlinks"}),

		// Version history ("what changed, when, by whom") — TASK-2022.
		// Read-only: returns the item's recorded content-version metadata
		// (newest-first), a token-light summary shape (id, created_at,
		// created_by, source, change_summary) with the resolved content
		// body omitted. Restoring a version stays a web-UI action.
		"history": actionItemHistory,

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
	{Name: "ref", Type: "string", Description: "Item reference (e.g. TASK-5). Required for actions naming one item, except create, list, starred, import, agenda, bulk-update (use refs) and unremind. ack-reminder: ref OR reminder_id; multiple fired reminders require an id."},
	{Name: "refs", Type: "array<string>", Description: "Item references for batch operations. Required: bulk-update (one or more refs)."},
	{Name: "target", Type: "string", Description: "The OTHER end of a relationship. Required: link, unlink. blocks: the item being blocked; blocked-by: the blocker; supersedes: the superseded item."},
	{Name: "link_type", Type: "string", Description: "Type of relationship for action=link/unlink.", Enum: []string{"blocks", "blocked-by", "supersedes", "implements", "split-from"}},

	// ── Item content ──
	{Name: "collection", Type: "string", Description: "Collection slug (e.g. \"tasks\", \"ideas\"). Required: create. Optional filter for: list."},
	{Name: "title", Type: "string", Description: "Item title. Required: create. Optional rename for: update."},
	{Name: "content", Type: "string", Description: "Markdown body. For: create, update."},
	{Name: "agent", Type: "bool", Description: "For action=get, return the full item body and meaningful work metadata as compact JSON while omitting internal UUIDs and duplicate join fields. Prefer this for Cursor and Codex reads."},

	// ── Artifact export / import ── (Phase 5)
	// `artifact`: the full portable artifact text (YAML frontmatter +
	// Markdown body) that `import` ingests as a new draft. Distinct from
	// `content` (which is only the item's Markdown body) because the
	// artifact carries the frontmatter the server needs to reconstruct
	// the item's collection + typed fields. `export` returns this same
	// text as its tool result.
	// ── Reminders ── (IDEA-2641)
	{Name: "remind_at", Type: "string", Description: "RFC3339 INSTANT a reminder fires (2026-08-01T09:00:00Z or with an offset). Required: remind as a one-shot (no schedule parameters). A bare date is REFUSED, not assumed to be midnight."},
	// PR metadata for action=link-pr (TASK-3611), sent as given.
	{Name: "pr_number", Type: "number", Description: "The pull request's number (> 0). Required: link-pr."},
	{Name: "pr_url", Type: "string", Description: "The pull request's absolute http(s) URL. Required: link-pr."},
	{Name: "pr_title", Type: "string", Description: "The pull request's title. For: link-pr."},
	{Name: "pr_state", Type: "string", Description: "The pull request's state as you know it (e.g. open, merged, closed). For: link-pr. Pad does not check it against GitHub."},
	{Name: "pr_branch", Type: "string", Description: "The pull request's head branch. For: link-pr."},
	{Name: "pr_repo", Type: "string", Description: "The pull request's repository, owner/name. For: link-pr."},
	{Name: "reminder_id", Type: "string", Description: "Reminder id from arming or action=reminders. Required for unremind. ack-reminder accepts reminder_id OR ref, never both."},

	// ── Schedules ── (TASK-4105)
	{Name: "every", Type: "string", Description: "Repeat rule: daily, weekdays, weekly, biweekly, monthly, monthly-last, quarterly, yearly, or an RRULE. For remind (schedule) and schedule-edit."},
	{Name: "start", Type: "string", Description: "Wall time, no Z or offset: 2026-11-09T09:00, or 2026-11-09 all-day. Required: remind as a schedule. For: schedule-edit."},
	{Name: "forecast", Type: "string", Description: "schedule-forecast estimate: YYYY-MM-DD or RFC3339; excludes clear_forecast."},
	{Name: "clear_forecast", Type: "bool", Description: "schedule-forecast: true clears; excludes forecast."},
	{Name: "forecast_source", Type: "string", Description: "With forecast only; max 280 characters."},
	{Name: "deadline", Type: "string", Description: "hard makes a deadline; soft or omit for no deadline. schedule-edit: soft and none clear it and keep any window. Not with window. schedule-renegotiate: the new deadline, an RFC3339 instant."},
	{Name: "window", Type: "string", Description: "Span from start: a duration (90m, 2h, 3d) or an end wall (2026-11-09T17:00). Not with deadline."},
	{Name: "anchor", Type: "string", Description: "calendar (default) | completion: the next one falls an interval after you mark it done."},
	{Name: "audience", Type: "string", Description: "human | agent | both (default)."},
	{Name: "tz", Type: "string", Description: "IANA zone the wall is read in (default: your account zone); for agenda, the zone shown. Not with floating."},
	{Name: "floating", Type: "bool", Description: "Read the wall in the zone of whoever it fires for. Not with tz."},
	{Name: "state", Type: "string", Description: "done | skip | snooze | move | ack (fired only; still owed, no comment). Required: schedule-resolve."},
	{Name: "until", Type: "string", Description: "RFC3339 instant to re-fire (snooze) or become due (move). Required: snooze, move."},
	{Name: "occurrence_at", Type: "string", Description: "RFC3339 instant of the outstanding occurrence, from action=schedule (next_occurrence.at). Required: schedule-resolve."},
	{Name: "actual_done_at", Type: "string", Description: "RFC3339 instant the work really finished. For: schedule-resolve, done only."},
	{Name: "end_schedule", Type: "bool", Description: "done/skip: end an open item's schedule (item_still_open)."},
	{Name: "reason", Type: "string", Description: "Why dates change; required for schedule-renegotiate and graph changes moving hard deadlines."},
	{Name: "scope", Type: "string", Description: "occurrence (default) | series. For: schedule-renegotiate."},
	{Name: "deadline_days", Type: "number", Description: "Series offset in days. For: schedule-renegotiate."},
	{Name: "deadline_secs", Type: "number", Description: "Series offset in seconds. For: schedule-renegotiate."},
	{Name: "deadline_behavior", Type: "string", Description: "lapses | stays_owed. For: schedule-renegotiate."},
	{Name: "remove_deadline", Type: "bool", Description: "Drop the series deadline. For: schedule-renegotiate."},
	{Name: "revision", Type: "string", Description: "Opaque preview token; return unchanged with graph applies. Required: schedule-renegotiate preview=false."},
	{Name: "preview", Type: "bool", Description: "Preview by default. schedule-renegotiate: false applies with revision. schedule-link: apply=true commits."},
	{Name: "from", Type: "string", Description: "Window start, RFC3339 instant. Required: agenda; with stale, resumes the list."},
	{Name: "to", Type: "string", Description: "RFC3339 instant: agenda end (<=92 days from from; not with stale), or schedule-move destination. Required for both."},
	{Name: "stale", Type: "bool", Description: "agenda: yours unresolved over 14 days, not a window."},

	{Name: "child", Type: "string", Description: "Child ref for schedule-link creation; ref is the parent."},
	{Name: "parent_role", Type: "string", Description: "schedule-link: parent's target | earliest | deadline | occupied_until."},
	{Name: "child_role", Type: "string", Description: "schedule-link: child's target | earliest | deadline | occupied_until."},
	{Name: "link_offset", Type: "object", Description: "schedule-link API offset: {mode:elapsed,elapsed_secs} or {mode:civil,days,clock_secs,all_day,zone}. All chosen members required, including 0/false; no other-mode members. zone: pinned IANA, never Local."},
	{Name: "cancel_policy", Type: "string", Description: "schedule-link: waiting_on (server default) | cancel_child_occurrence."},
	{Name: "link_id", Type: "string", Description: "schedule-link: edit this link; schedule-unlink: remove it."},
	{Name: "gate_id", Type: "string", Description: "schedule-gate: remove this gate; omit blocker."},
	{Name: "blocker", Type: "string", Description: "schedule-gate: blocker ref; waits for success (fixed occurrence if scheduled)."},
	{Name: "apply", Type: "bool", Description: "schedule-link/move: true commits; default previews without writes. Repeat preview inputs with revision; stale revision refuses."},
	{Name: "acknowledge_violations", Type: "bool", Description: "schedule-link/move: accept previewed bound violations with its fresh revision; bounds and links remain."},

	// ── Execution lease ── (#1221)
	{Name: "holder", Type: "string", Description: "Lease holder label for action=claim/release. Optional; defaults to your agent name. Display only with a worker_id."},
	{Name: "worker_id", Type: "string", Description: "Your lease worker id, as a claim answers it. For: claim, release; pass it to resume your leases from a new connection."},
	{Name: "ttl", Type: "string", Description: "Lease duration for action=claim, as a Go duration (e.g. \"15m\", \"1h\"): default 15m, max 24h, minimum 1s (fractions truncate)."},

	{Name: "artifact", Type: "string", Description: "Full portable artifact text (YAML frontmatter + Markdown body). Required: import. NOT the same as `content` (which is just the item's Markdown body)."},

	// ── Status / priority / scheduling ──
	{Name: "status", Type: "string", Description: "Item status (collection-specific enum). For: create, update, list filter, bulk-update."},
	{Name: "priority", Type: "string", Description: "Item priority. For: create, update, list filter, bulk-update."},
	{Name: "category", Type: "string", Description: "Item category. For: create, update."},

	// ── Hierarchy / assignment ──
	{Name: "parent", Type: "string", Description: "Parent item ref (e.g. PLAN-3). For: create, update, list filter."},
	{Name: "unparented", Type: "bool", Description: "For action=list, keep only items with no parent or implements relationship. Mutually exclusive with parent."},
	// BUG-2305: list is summary-shaped by default on BOTH transports;
	// this is the discoverable opt-in for complete bodies. Reaches
	// LOCAL STDIO as the CLI's --full flag via BuildCLIArgs and the
	// remote path via dispatchItemList reading the dispatch input —
	// declared only because both transports genuinely honor it (the
	// v0.18 lesson: never advertise a param one transport drops).
	{Name: "full", Type: "bool", Description: "list: complete content bodies instead of summaries. history: each version's content body. Token-expensive; prefer get for one item's body."},
	{Name: "role", Type: "string", Description: "Agent role slug for create/update/list/context. schedule-move: target | earliest | deadline | occupied_until."},
	// PLAN-3598 action=context.
	{Name: "intent", Type: "string", Description: "For: context. What you are about to do (implement, review, ...): picks the conventions whose trigger matches; without it, only always-on ones."},
	{Name: "budget", Type: "number", Description: "For: context. Size cap in characters (default 40000); whole parts past it are listed as omitted."},
	{Name: "assign", Type: "string", Description: "User name or email to assign. For: create, update, list filter. To UNASSIGN, use clear_assigned_user=true — an empty `assign` does NOT clear."},
	// The canonical clear form (IDEA-2584). Booleans rather than an empty
	// string on assign/role, for two reasons that both matter:
	//
	//  1. An empty DECLARED string is inert everywhere else on this tool
	//     (title, content, comment, tags), so a client that fills optional
	//     params with "" instead of omitting them is harmless today. Giving
	//     `assign: ""` a destructive meaning would turn that same client into
	//     one that silently unassigns every item it touches. A boolean
	//     carries its destructive meaning in its name and can't be tripped.
	//  2. Only a boolean reaches LOCAL STDIO. BuildCLIArgs emits the CLI's
	//     real flags, so a declared param with no flag behind it is dropped;
	//     these map to `--clear-assigned-user` / `--clear-agent-role`.
	//
	// The empty-string `assigned_user_id: ""` form still works over the
	// remote transport and is not deprecated — it's just not discoverable
	// from this schema, which is what these params fix.
	{Name: "clear_assigned_user", Type: "bool", Description: "update: unassign (clear assigned_user_id). Refuses assignment in the same call via assign or assigned_user_id in field."},
	{Name: "clear_agent_role", Type: "bool", Description: "update: clear agent_role_id. Refuses setting a role in the same call."},
	// BUG-2078: same shape as clear_assigned_user/clear_agent_role above, for
	// the same reason — an empty `parent` is inert everywhere else this
	// mapper treats empty strings as "not provided", so it can't double as a
	// clear signal without becoming a silent trap for a client that pads
	// unused params with "". The server has supported clearing since BUG-2013
	// (extractParentLink treats a PRESENT-but-empty "parent" key in
	// fields_patch as "detach"); this is the first client-visible way to
	// reach it.
	{Name: "clear_parent", Type: "bool", Description: "update: detach parent. Empty parent is inert. Refuses setting a parent in the same call."},

	// ── Tagging ──
	// `tags`: agents naturally pass a JSON array of strings (e.g.
	// `["v1","frontend"]`); the create/update handlers also accept the
	// canonical JSON-encoded string form ItemCreate/ItemUpdate store
	// internally. Pre-BUG-1432 the description said "Comma-separated"
	// which mismatched both the CLI flag's help ("JSON array of tags")
	// AND the column shape (JSONB on Postgres) — agents passing
	// "foo,bar" produced corrupt rows on SQLite and HTTP 500s on
	// Postgres (JSONB rejects non-JSON).
	{Name: "tags", Type: "array<string>", Description: "Tags as a JSON array of strings, e.g. [\"v1\",\"frontend\"]. For: create, update."},
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
	{Name: "field", Type: "array<string>", Description: "SCHEMA-DECLARED custom fields as [\"key=value\"] (e.g. [\"due_date=2026-06-01\",\"effort=l\"]). Use dedicated status/priority/category/parent/role/assign/tags params. For create/update/list filter/move. create/update also accept fields OBJECT. create/update/move refuse implementation_notes (use note), decision_log (decide), github_pr (link-pr/unlink-pr), convention (library activation only)."},
	// `fields`: the OBJECT alias for the write path (#1066). Reads
	// return `fields` as a native object, so writing that same shape
	// back is what agents naturally do — it used to be silently
	// dropped. See catalog_item_fields.go for the merge contract.
	{Name: "fields", Type: "object", Description: "Field OBJECT matching reads, e.g. {\"status\":\"done\",\"effort\":\"l\"}. create/update only. Merges like field/dedicated params; conflicting values for a key across placements refuse. JSON types stay intact; objects/arrays write as-is to multi_select/json. null refuses (omit unchanged keys); parent/plan must be string refs."},

	// ── List / starred ──
	{Name: "all", Type: "bool", Description: "Include archived/done items in list responses. For: list, starred."},
	{Name: "limit", Type: "number", Description: "Maximum results. For: list, backlinks, history; default 50, max 300. history returns the NEWEST N versions. starred takes no limit: at most 300, with has_more."},
	{Name: "offset", Type: "number", Description: "Skip the first N results (paging). For: backlinks."},
	{Name: "sort", Type: "string", Description: "Sort field. For: list."},
	{Name: "group_by", Type: "string", Description: "Group-by field. For: list."},

	// ── Move ──
	{Name: "target_collection", Type: "string", Description: "Destination collection slug for action=move. Required: move."},

	// ── Comments ──
	{Name: "message", Type: "string", Description: "Comment body. Required: comment, edit-comment."},
	{Name: "comment_id", Type: "string", Description: "ID of a comment on `ref` (from list-comments). Required: edit-comment, delete-comment."},
	{Name: "reply_to", Type: "string", Description: "Parent comment ID for threading replies. For: comment."},
	{Name: "comment", Type: "string", Description: "Audit comment explaining the change. Optional for: update, schedule-resolve."},

	// ── Guard override ── (IDEA-1494)
	// update/bulk-update reject non-terminal → terminal done-field
	// transitions while the item still has open (non-terminal)
	// children, surfacing a 409 with code=open_children plus a
	// machine-readable details.open_children array (one entry per
	// blocking child: {ref, title, status, collection_slug}). Setting
	// force=true skips the guard and still records the transition.
	// The same flag exists on `pad item update --force` so the CLI
	// and MCP escape hatches are identical.
	{Name: "force", Type: "bool", Description: "Override the open-children guard. For: update, bulk-update, move. On code=open_children, details.open_children lists the blocking child refs: ship them and retry, or set force=true to orphan them on purpose."},

	// ── Optimistic concurrency ── (TASK-2022)
	// update only. Round-trip the `updated_at` a prior get returned; the
	// server rejects the update with code=update_conflict (409) if the item
	// changed since, so a coordinating agent can detect a lost-update race
	// and re-read instead of silently clobbering another writer.
	{Name: "expected_updated_at", Type: "string", Description: "Weaker guard for action=update than expected_seq: the RFC3339 updated_at you last read; rejected with code=update_conflict if the item changed since. Optional."},
	// BUG-3037: the STRONG token, and the one an agent should reach for.
	// `updated_at` has one-second resolution, so two writes inside one second
	// both match it and NEITHER conflicts — an agent that reads, decides and
	// writes quickly (which is what agents do) loses the race the token exists
	// to catch. `seq` is bumped on every mutation of the row, is returned on
	// every item AND every item summary, and is an integer.
	{Name: "overwrite_pending_edits", Type: "bool", Description: "update: true replaces a tab's unsaved edits instead of refusing token-guarded content (content_pending_flush)."},
	// ── Patch (PLAN-3578) ──
	{Name: "edits", Type: "array<object>", ItemSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"old":       map[string]any{"type": "string", "description": "Exact text that must occur EXACTLY ONCE in the current body. Never empty."},
			"new":       map[string]any{"type": "string", "description": "Replacement text; empty deletes."},
			"line_hint": map[string]any{"type": "number", "description": "1-based line on which `old` starts; only chooses among several exact matches."},
		},
		"required": []any{"old", "new"},
	}, Description: "patch: the edits, applied in order, all or nothing. Required: patch."},
	{Name: "op_key", Type: "string", Description: "patch/create/comment/note/decide: caller-chosen idempotency key (e.g. UUID). Retry SAME request/key for its first answer without another write after unclear outcomes. Different request with reused key refuses: idempotency_key_reused."},
	{Name: "undo", Type: "string", Description: "patch: invert an earlier patch by its patch_id (instead of edits). Refused, writing nothing (patch_anchor_missing), when the text it changed has changed since."},
	{Name: "lines", Type: "string", Description: "get: return only these body lines, numbered (\"A-B\", \"A-\" or \"A\"; 1-based), with seq and content_state, for targeting a patch."},
	// PLAN-3555: the deliberate-overwrite opt-out. Phase 1 changes no outcome;
	// phase 2 refuses a body write that carries neither this nor a token.
	{Name: "replace", Type: "bool", Description: "update: true overwrites the content on purpose, whatever changed since you read it (no version token). Conflicts with expected_seq."},
	{Name: "expected_seq", Type: "number", Description: "update: the `seq` you last read, PREFERRED over expected_updated_at; rejected with update_conflict if the item changed since."},

	// ── Notes / decisions ──
	{Name: "summary", Type: "string", Description: "Short note headline. Required: note."},
	{Name: "details", Type: "string", Description: "Long-form note body. For: note."},
	{Name: "decision", Type: "string", Description: "Decision summary text. Required: decide."},
	{Name: "rationale", Type: "string", Description: "Reasoning behind the decision. For: decide."},
}

const padItemToolDescription = `Item operations.

Actions:
  create — Create an item.
    Required: collection, title.
    Optional: status, priority, category, content, parent, role, assign, tags, field, fields.
    Use dedicated params; field=["key=value"] handles other SCHEMA-DECLARED fields.
    fields OBJECT matches reads; conflicting values across placements refuse.
    tags: JSON string array, never comma-separated.
  update — Update an item; fields MERGE server-side, only supplied keys change.
    Required: ref, at least one mutable field.
    Optional: title, status, priority, content, role, assign, parent, comment, tags,
    field, fields, expected_seq, expected_updated_at, overwrite_pending_edits, replace.
    Placement as create. Prefer expected_seq (last read seq) over expected_updated_at;
    stale returns update_conflict, details.actual_seq. content REPLACES the body: get,
    edit, send with read seq. On details.content_write conflict, NEVER resend stale
    content: re-read, edit CURRENT content, send new seq. replace=true skips seq checks.
    Neither: warnings.unguarded_content_write now, refusal in a later release.
    content_outcome="applied_pending_flush": a browser holds the item; read lags,
    write succeeded. Re-read later, never re-send.
  delete — Archive. Required: ref.
  restore — Restore an archived item. Required: ref.
  get — Read. Required: ref. Optional: lines ("40-80": numbered body lines + seq for anchors).
  patch — Exact body edits.
    Required: ref, edits ([{old,new,line_hint?}]) OR undo (invert a prior patch_id).
    Optional: expected_seq. Each old must occur EXACTLY ONCE; quote context or use
    line_hint to choose exact matches. Ordered, atomic edits. Read the diff: anchors
    fix position, not meaning; rewritten anchors still land. A moved expected_seq
    applies with rebased=true and intervening writes (complete=false if records missing).
    Refusals write nothing: patch_anchor_missing/patch_anchor_ambiguous (re-read/quote
    current text); patch_unstable_roundtrip (unrelated editor changes: whole-body
    update); patch_room_open (older editor cannot patch).
  list — List/filter. Optional: collection, status, priority, parent, unparented,
    role, assign, all, limit, full. parent/unparented are mutually exclusive.
    Summary-shaped, no content bodies unless full=true.
  move — Change collection. Required: ref, target_collection.
  link — Add relationship. Required: ref, target, link_type.
    link_type: blocks | blocked-by | supersedes | implements | split-from.
  unlink — Remove relationship. Required: ref, target, link_type.
  deps — Incoming/outgoing dependencies. Required: ref.
  context — One brief before starting work: item, parents with acceptance criteria,
    matching conventions, deps, links. Required: ref. Optional: intent, role, budget.
  link-pr — Link supplied GitHub metadata; nothing is fetched.
    Required: ref, pr_number, pr_url. Optional: pr_title, pr_state, pr_branch, pr_repo.
    Replaces any previous link; returns the item.
  unlink-pr — Remove PR link. Required: ref.
  remind — Arm a one-shot reminder. Required: ref, remind_at (RFC3339 INSTANT; bare dates refused).
    Use when deferring work. Fired items carry reminder_id in pad_project next/ready
    until acknowledged. start replaces remind_at for a once/repeating check, deadline
    or window; optional every, deadline, window, anchor, audience, tz, floating.
  schedule — Read the item's schedule. Required: ref.
  schedule-edit — Patch only given schedule parameters; ref required. deadline soft/none clears it, keeping any window.
  schedule-resolve — Resolve the outstanding occurrence. Required: ref, state, occurrence_at.
    snooze and move also need until. Optional: comment, actual_done_at (done only), end_schedule.
  schedule-forecast — Hand-set estimate: ref, forecast OR clear_forecast:true; forecast_source optional on set. Arms nothing; never a missed deadline.
  schedule-renegotiate — Change a deadline. Required: ref, reason. scope occurrence: deadline (RFC3339);
    series: deadline_days/deadline_secs, deadline_behavior or remove_deadline. Previews, returning a
    revision; preview=false with it applies. Hard deadline: assignee or owner only.
  schedule-link — Preview a time link; apply=true commits. Create: ref (parent), child,
    parent_role, child_role, link_offset. With link_id: edit link_offset and/or cancel_policy.
    Optional: reason, revision, acknowledge_violations, cancel_policy. Read links/gates with schedule.
  schedule-unlink — Remove a time link. Required: link_id. Keeps both commitments.
  schedule-move — Preview linked date cascade; apply=true commits. Required: ref, role, to.
    Optional: reason, revision, acknowledge_violations. Returns changes, violations, left_in_place,
    renegotiates_deadline and revision. Deadline changes need reason and fresh revision;
    bound crossings also need acknowledge_violations. Both ends must be readable and editable.
  schedule-gate — Wait for blocker success (fixed occurrence if scheduled): ref, blocker.
    With gate_id and no blocker: remove gate. Cancellation/lapse/skip requires a decision.
  schedule-dismiss — Clear an ended schedule from Outstanding. ref required.
  schedule-delete — Irreversibly delete schedule/rule/history/managed reminder; stops pending firing. ref required. Keeps item/comments/one-shot reminders.
  agenda — Occurrences/dated items: from, to (RFC3339, <=92 days); tz optional.
    no_date_scope:personal_only means undated work is not listed, never empty.
  ack-reminder — Acknowledge by reminder_id OR ref, never both; leaves next/ready.
    ref requires one fired, unacknowledged unmanaged reminder; multiple refuse, ack by id.
    Only this acknowledges; completing the item does not. Reminders may fire after work finishes.
  reminders — Armed/fired/acknowledged reminders; ref required. Row id is reminder_id for unremind/ack-reminder (fired only).
  unremind — Disarm/delete by reminder_id (from reminders or arming response).
  claim — Atomic execution lease. Required: ref. Optional: holder, worker_id, ttl
    (Go duration; default 15m, max 24h). Returns worker_id. One claimer wins; loser's
    409 names live holder/expiry: log and skip, never retry in a loop. Re-claim extends expiry.
  release — Release when you finish. Required: ref. Optional: holder, worker_id.
    Another holder's live lease is refused. Absent/expired leases return released:false, no error.
  star — Star. Required: ref.
  unstar — Unstar. Required: ref.
  starred — List starred. Optional: all.
  comment — Add comment. Required: ref, message. Optional: reply_to (parent comment ID).
  list-comments — List with id and edited flag. Required: ref.
  edit-comment — Replace body (author only). Required: ref, comment_id, message.
  delete-comment — Delete (item editors may). Required: ref, comment_id.
    Replies retain a deleted:true, empty-body placeholder until the last reply is deleted.
  backlinks — Inbound [[...]] references. Required: ref.
    Optional: limit (default 50, max 300), offset.
    Same-workspace rows first, then cross-workspace rows carrying source_workspace_slug.
  history — Read-only newest-first history.
    Required: ref. Optional: limit (NEWEST 50 by default, max 300), full.
    Summaries: id, created_at, created_by, source, change_summary.
    full includes resolved content; restoring is web-only.
  bulk-update — Set status/priority on several items.
    Required: refs (array of refs), at least one of status / priority.
  note — Append implementation note. Required: ref, summary. Optional: details.
  decide — Record decision. Required: ref, decision. Optional: rationale.
  export — Read-only, side-effect-free artifact TEXT (YAML frontmatter + Markdown),
    for import into another workspace. Required: ref (PLAYB-N/CONVE-N or slug).
    Only playbooks/conventions; other types refuse.
  import — Create playbook/convention DRAFT; artifact's collection gates type.
    Required: artifact (full export text). Returns {ref,slug,warnings}; review/activate
    afterward. Mutating, not destructive (creates item).

ALWAYS prefer issue refs (TASK-5, IDEA-12) over slugs.
When updating status, include comment="why" so the audit trail records WHY, not just THAT it changed.
Cross-item FTS: pad_search.action=query; pad_item.list only filters.`

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

func actionItemAckReminder(ctx context.Context, input map[string]any, env ActionEnv) (*CallToolResult, error) {
	ref, _ := input["ref"].(string)
	id, _ := input["reminder_id"].(string)
	if (ref == "") == (id == "") {
		return errStructured("pad_item.ack-reminder", fmt.Errorf("provide either reminder_id or ref, never both")), nil
	}
	out := make(map[string]any, len(input)+1)
	for k, v := range input {
		out[k] = v
	}
	// The CLI positional is <reminder-id-or-ref>. Keep the original parameter
	// too, so HTTP dispatch routes on which one the caller gave.
	out["reminder_id_or_ref"] = id
	if ref != "" {
		out["reminder_id_or_ref"] = ref
	}
	return env.Dispatch(ctx, []string{"item", "ack"}, out)
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
func actionItemLink(ctx context.Context, input map[string]any, env ActionEnv) (*CallToolResult, error) {
	cmdPath, dispatchInput, err := resolveItemLink(input, true)
	if err != nil {
		return errStructured("pad_item.link", err), nil
	}
	return env.Dispatch(ctx, cmdPath, dispatchInput)
}

// actionItemUnlink is the symmetric un-create operation. Same routing
// rules; uses route.unlinkCmdPath instead of route.linkCmdPath.
func actionItemUnlink(ctx context.Context, input map[string]any, env ActionEnv) (*CallToolResult, error) {
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
func errStructured(prefix string, err error) *CallToolResult {
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
func actionItemBulkUpdate(ctx context.Context, input map[string]any, env ActionEnv) (*CallToolResult, error) {
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

// MCP list limits (TASK-2000). A bare `pad_item.list` from an agent
// otherwise dumps the whole workspace into context (the CLI's own default
// only kicks in via the exec path; the HTTP dispatch path bypasses it). We
// inject a tight default and clamp an oversized one here so both dispatchers
// stay bounded. Mirrors the backlinks default/max (50 / 300).
const (
	mcpItemListDefaultLimit = 50
	mcpItemListMaxLimit     = 300
)

// actionItemPatch handles pad_item.action=patch on stdio. It gives each edit's
// line_hint remote's rule (editsWithLineHints, shared with mapItemPatch) and
// then forwards to `item patch` as passThrough did (BUG-4082). edits used to
// reach the CLI unread: its JSON decode into an int refused a string hint that
// remote coerces, and took a JSON number of 2^53 or more, which mcp-go's
// float64 decode of the request may already have rounded, as a line number. A
// refusal is validation_failed with remote's message. The caller's map is not
// modified.
func actionItemPatch(ctx context.Context, input map[string]any, env ActionEnv) (*CallToolResult, error) {
	edits, isList := input["edits"].([]any)
	if !isList {
		return env.Dispatch(ctx, []string{"item", "patch"}, input)
	}
	out, err := editsWithLineHints(edits)
	if err != nil {
		return validationFailedResult("item patch", err.Error(),
			"Pass each line_hint as a whole JSON number below 2^53, or a string of decimal digits with no leading zero."), nil
	}
	in := make(map[string]any, len(input))
	for k, v := range input {
		in[k] = v
	}
	in["edits"] = out
	return env.Dispatch(ctx, []string{"item", "patch"}, in)
}

// actionItemList handles pad_item.action=list. It forwards to `item list`
// but first applies a default limit (when the agent didn't ask for one) and
// clamps an oversized one, so a bare agent list can't dump the whole
// workspace into context. Everything else about the input passes through
// unchanged — collection/status/priority/parent filters, all, etc.
//
// Result shape: SUMMARY (no content bodies) by default on BOTH
// transports — the exec path via the CLI's own default projection
// (cli.ToItemSummaries, lifted by --full), the HTTP path via the
// hand-written dispatchItemList (BUG-2305; a RouteMapper cannot
// transform responses, and the server's list endpoint has no
// projection parameter). `full: true` opts into complete bodies on
// both. MCP callers that need one item's body should prefer
// action=get.
func actionItemList(ctx context.Context, input map[string]any, env ActionEnv) (*CallToolResult, error) {
	// Early MCP-side feedback for the parent/unparented conflict; canonical
	// enforcement lives in validateUnparentedListRequest
	// (internal/server/handlers_items.go). Keep the two in sync.
	if unparented, _ := input["unparented"].(bool); unparented {
		if parent, _ := input["parent"].(string); strings.TrimSpace(parent) != "" {
			return errStructured("pad_item.list", fmt.Errorf("parent and unparented are mutually exclusive")), nil
		}
	}
	out := make(map[string]any, len(input)+1)
	for k, v := range input {
		out[k] = v
	}

	// A limit of zero or less takes the default. A fractional one is refused,
	// not truncated (BUG-4061): 0.5 used to become 0, and so the default.
	limit := mcpItemListDefaultLimit
	n, ok, err := intParam(input, "limit")
	if err != nil {
		return errStructured("pad_item.list", err), nil
	}
	if ok && int(n) > 0 {
		limit = int(n)
	}
	if limit > mcpItemListMaxLimit {
		limit = mcpItemListMaxLimit
	}
	// One row past the limit tells whether the list was cut short (BUG-3704):
	// the result then says so. It never pages for the agent, which is what
	// the limit is for.
	out["limit"] = limit + 1

	res, err := env.Dispatch(ctx, []string{"item", "list"}, out)
	if err != nil || res == nil || res.IsError {
		return res, err
	}
	return markItemListTruncation(res, limit), nil
}

// actionItemStarred handles pad_item.action=starred. Like list it never pages
// (BUG-3790, as BUG-3704 did for list): it asks for one row past
// the list cap, on both transports (the HTTP mapper sends it as ?limit=, stdio
// as pad item starred --limit), and a result that had the extra row says
// has_more with the truncation note.
func actionItemStarred(ctx context.Context, input map[string]any, env ActionEnv) (*CallToolResult, error) {
	out := make(map[string]any, len(input)+1)
	for k, v := range input {
		out[k] = v
	}
	out["limit"] = mcpItemListMaxLimit + 1
	res, err := env.Dispatch(ctx, []string{"item", "starred"}, out)
	if err != nil || res == nil || res.IsError {
		return res, err
	}
	return markItemListTruncation(res, mcpItemListMaxLimit), nil
}

// itemListTruncatedNote is the text an agent sees when its list was cut short.
const itemListTruncatedNote = "More items match than this list shows (has_more). Narrow the filters, or raise limit (max %d)."

// markItemListTruncation trims a list result fetched with limit+1 back to
// limit and, when it had the extra row, sets has_more on the structured
// result and adds a note. A list that fit is returned unchanged.
func markItemListTruncation(res *CallToolResult, limit int) *CallToolResult {
	obj, ok := res.StructuredContent.(map[string]any)
	if !ok {
		return res
	}
	rows, ok := obj["items"].([]any)
	if !ok || len(rows) <= limit {
		return res
	}
	marked := make(map[string]any, len(obj)+1)
	for k, v := range obj {
		marked[k] = v
	}
	marked["items"] = rows[:limit]
	marked["has_more"] = true
	text, err := json.Marshal(rows[:limit])
	if err != nil {
		return res
	}
	out := structuredResult(marked, string(text))
	out.Content = append(out.Content, mcp.NewTextContent(fmt.Sprintf(itemListTruncatedNote, mcpItemListMaxLimit)))
	return out
}

// mcpItemHistoryDefaultLimit / MaxLimit bound pad_item.action=history the same
// way list and backlinks are bounded, and for a sharper reason: a
// collab-edited item records a version every few seconds while someone types,
// so "the whole history" is routinely hundreds of rows nobody asked for
// (BUG-2608). Same numbers as list, so an agent does not have to remember a
// third pair.
const (
	mcpItemHistoryDefaultLimit = 50
	mcpItemHistoryMaxLimit     = 300
)

// actionItemHistory handles pad_item.action=history. It injects a default
// limit when the agent did not ask for one and clamps an oversized one, so
// this lands on BOTH transports: the HTTP dispatcher reads it off the input,
// and the exec path gets it as the CLI's --limit through BuildCLIArgs.
//
// The catalog is the right home for the default (rather than either
// dispatcher) for the reason actionItemList already documents — it is the one
// place both transports pass through.
func actionItemHistory(ctx context.Context, input map[string]any, env ActionEnv) (*CallToolResult, error) {
	out := make(map[string]any, len(input)+1)
	for k, v := range input {
		out[k] = v
	}

	limit := mcpItemHistoryDefaultLimit
	n, ok, err := intParam(input, "limit")
	if err != nil {
		return errStructured("pad_item.history", err), nil
	}
	if ok && n > 0 {
		limit = int(n)
	}
	if limit > mcpItemHistoryMaxLimit {
		limit = mcpItemHistoryMaxLimit
	}
	out["limit"] = limit

	return env.Dispatch(ctx, []string{"item", "history"}, out)
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
func actionItemExport(ctx context.Context, input map[string]any, env ActionEnv) (*CallToolResult, error) {
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
func actionItemImport(ctx context.Context, input map[string]any, env ActionEnv) (*CallToolResult, error) {
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

	// Add the CLI's positional `file`, which BuildCLIArgs emits as the lone
	// positional for `item import`. `artifact` STAYS in the input: the CLI
	// never sees it (BuildCLIArgs emits only declared args and flags), but
	// the remote transport's HTTPHandlerDispatcher reads the artifact text
	// from it, and dropping it made every remote import answer "artifact
	// is required" (BUG-3533).
	out := make(map[string]any, len(input)+1)
	for k, v := range input {
		out[k] = v
	}
	out["file"] = tmpPath
	return env.Dispatch(ctx, []string{"item", "import"}, out)
}

// actionItemGet handles pad_item.action=get. With `lines` it slices the body
// into numbered lines HERE, the one place both transports pass through, so
// the two answer identically (PLAN-3578).
func actionItemGet(ctx context.Context, input map[string]any, env ActionEnv) (*CallToolResult, error) {
	spec, has := input["lines"]
	if !has {
		return env.Dispatch(ctx, []string{"item", "show"}, input)
	}
	s, _ := spec.(string)
	start, end, err := bodypatch.ParseLineRange(s)
	if err != nil {
		return errStructured("pad_item.get", err), nil
	}
	out := make(map[string]any, len(input))
	for k, v := range input {
		if k != "lines" {
			out[k] = v
		}
	}
	res, err := env.Dispatch(ctx, []string{"item", "show"}, out)
	if err != nil || res == nil || res.IsError {
		return res, err
	}
	m, ok := res.StructuredContent.(map[string]any)
	if !ok {
		return errStructured("pad_item.get", fmt.Errorf("lines: the item read returned no structured body")), nil
	}
	content, _ := m["content"].(string)
	lines, from, to, total := bodypatch.SliceBodyLines(content, start, end)
	view := bodypatch.BodyLines{Start: from, End: to, TotalLines: total, Lines: lines}
	view.Ref, _ = m["ref"].(string)
	// The server's own seq, not caller input: read leniently, so an absent
	// or malformed one leaves Seq zero (BUG-4061 ruled it out of scope).
	if n, ok, _ := numericInput(m["seq"]); ok {
		view.Seq = n
	}
	view.ContentState, _ = m["content_state"].(string)
	text, _ := json.Marshal(view)
	return structuredResult(view, string(text)), nil
}
