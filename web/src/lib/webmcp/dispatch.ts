// dispatch.ts — the `(toolName, action, args) → client.ts` router for the
// WebMCP surface (PLAN-1888 / TASK-1892 read dispatch, TASK-1893 write
// dispatch). Pure/injectable: takes the `api` client + the route `wsSlug` as
// arguments so it's unit-testable with a mocked client and no browser.
//
// Correctness constraints, all unit-tested:
//   - DR-4: the workspace is FROZEN to the route `wsSlug`. An agent-supplied
//     `workspace` arg is REJECTED (not silently overridden) so a stray /
//     malicious workspace can't slip through. Every handler receives the route
//     wsSlug, never an arg-derived one.
//   - No silent no-ops: every mutating catalog action either dispatches to a
//     real client.ts method or returns a precise, actionable error. A write
//     action with no browser mapping returns an honest "not available" error.
//
// Consent (DR-2): consent is NOT enforced here. The browser host fires
// per-invocation consent BEFORE `execute` runs, gated by the descriptor's
// `readOnlyHint`. Mutating tools (pad_item, pad_collection, pad_role,
// pad_library) carry NO readOnlyHint (they mix reads + writes — descriptors.ts
// `isAllReadOnly`), so the host prompts on every call. By the time a write
// handler below runs, the user has already consented. This dispatcher is the
// post-consent execution layer, not the gate. (A real Chrome-149 consent check
// can't run in CI — see the PR body's manual-verification note.)

import type { api as ApiClient } from '$lib/api/client';
import type { ItemCreate, ItemUpdate } from '$lib/types';

type Api = typeof ApiClient;

/** Result envelope the WebMCP host expects from `execute`. */
export type DispatchResult = ModelContextToolResult;

/** A handler: pure call into the api client. Reads and writes share the shape;
 *  the route wsSlug is injected, the agent's `workspace` arg never reaches it. */
type Handler = (
	api: Api,
	wsSlug: string,
	args: Record<string, unknown>,
) => Promise<unknown>;

// ── helpers ────────────────────────────────────────────────────────────────

function str(args: Record<string, unknown>, key: string): string | undefined {
	const v = args[key];
	if (typeof v === 'string' && v.length > 0) return v;
	return undefined;
}

function num(args: Record<string, unknown>, key: string): number | undefined {
	const v = args[key];
	if (typeof v === 'number') return v;
	if (typeof v === 'string' && v.trim() !== '' && !Number.isNaN(Number(v))) {
		return Number(v);
	}
	return undefined;
}

function bool(args: Record<string, unknown>, key: string): boolean | undefined {
	const v = args[key];
	if (typeof v === 'boolean') return v;
	if (v === 'true') return true;
	if (v === 'false') return false;
	return undefined;
}

function requireRef(args: Record<string, unknown>): string {
	const ref = str(args, 'ref') ?? str(args, 'slug');
	if (!ref) throw new Error("missing required arg 'ref'");
	return ref;
}

function requireArg(args: Record<string, unknown>, key: string): string {
	const v = str(args, key);
	if (!v) throw new Error(`missing required arg '${key}'`);
	return v;
}

/**
 * Coerce an array<string> param (the MCP transport may also deliver a single
 * string). Returns undefined when absent/empty. Throws on a non-string element
 * rather than filtering it out, so a malformed array (e.g. `refs: ["TASK-1", 123]`,
 * `tags: [123]`) surfaces a precise tool error instead of a partial / empty
 * write — no silent param drop.
 */
function strArray(args: Record<string, unknown>, key: string): string[] | undefined {
	const v = args[key];
	if (v === undefined || v === null) return undefined;
	if (typeof v === 'string') return v.length > 0 ? [v] : undefined;
	if (!Array.isArray(v)) {
		throw new Error(`'${key}' must be an array of strings`);
	}
	const out: string[] = [];
	for (const e of v) {
		if (typeof e !== 'string') {
			throw new Error(`'${key}' must contain only strings (got ${JSON.stringify(e)})`);
		}
		if (e.length > 0) out.push(e);
	}
	return out.length > 0 ? out : undefined;
}

/**
 * Parse the catalog's `field` param (array of "key=value" strings for
 * schema-declared custom fields) into a key→value object. Throws on a
 * malformed entry (missing `=` or empty key) rather than silently dropping it,
 * so an agent that mis-shapes a field setter gets a precise tool error instead
 * of a write that quietly omits the field.
 */
function parseFieldKVP(args: Record<string, unknown>): Record<string, string> {
	const raw = args.field;
	if (raw === undefined || raw === null) return {};
	// Accept the canonical array<string> shape, or a single string (lenient,
	// matching strArray elsewhere). Anything else is malformed — throw rather
	// than coerce/drop, so a mis-shaped `field` never silently no-ops.
	const entries: unknown[] = Array.isArray(raw) ? raw : [raw];
	const out: Record<string, string> = {};
	for (const entry of entries) {
		if (typeof entry !== 'string') {
			throw new Error(
				`malformed 'field' entry ${JSON.stringify(entry)} — expected a "key=value" string`,
			);
		}
		if (entry.length === 0) continue;
		const idx = entry.indexOf('=');
		if (idx <= 0) {
			throw new Error(
				`malformed 'field' entry ${JSON.stringify(entry)} — expected "key=value"`,
			);
		}
		out[entry.slice(0, idx)] = entry.slice(idx + 1);
	}
	return out;
}

/**
 * Roll the catalog's flat item params (status / priority / category / parent
 * plus arbitrary `field` key=value entries) into the `fields` JSON string the
 * create/update endpoints persist — the browser-side mirror of the CLI's
 * field-building in cmd/pad/main.go::item create. The server resolves a
 * `parent` ref inside the fields JSON itself (handlers_items.go ~:580), so no
 * client-side parent lookup is needed.
 *
 * Returns the JSON string, or undefined when no flat fields were supplied (so
 * an update carrying only e.g. `title` doesn't blow away the item's fields).
 */
function buildFieldsJSON(args: Record<string, unknown>): string | undefined {
	const fields: Record<string, unknown> = {};
	for (const key of ['status', 'priority', 'category', 'parent']) {
		const v = str(args, key);
		if (v !== undefined) fields[key] = v;
	}
	Object.assign(fields, parseFieldKVP(args));
	if (Object.keys(fields).length === 0) return undefined;
	return JSON.stringify(fields);
}

/** Tags: the catalog passes a JSON array of strings; ItemCreate/ItemUpdate
 *  store the canonical JSON-encoded string form. */
function buildTagsJSON(args: Record<string, unknown>): string | undefined {
	const tags = strArray(args, 'tags');
	if (!tags) return undefined;
	return JSON.stringify(tags);
}

/**
 * Resolve the catalog's `role` (slug) and `assign` (user name/email) item
 * params to the `agent_role_id` / `assigned_user_id` the create/update
 * endpoints persist — the browser mirror of the CLI's slug→ID / name→userID
 * resolution (cmd/pad/main.go::item create). Throws (surfaced as a tool error,
 * never a silent drop) when the role/user can't be resolved.
 */
async function resolveAssignment(
	api: Api,
	ws: string,
	args: Record<string, unknown>,
): Promise<{ agent_role_id?: string; assigned_user_id?: string }> {
	const out: { agent_role_id?: string; assigned_user_id?: string } = {};
	const role = str(args, 'role');
	if (role !== undefined) {
		const resolved = await api.agentRoles.get(ws, role);
		if (!resolved?.id) throw new Error(`role ${JSON.stringify(role)} not found`);
		out.agent_role_id = resolved.id;
	}
	const assign = str(args, 'assign');
	if (assign !== undefined) {
		const { members } = await api.members.list(ws);
		const match = members.find(
			(m) =>
				m.user_name?.toLowerCase() === assign.toLowerCase() ||
				m.user_email?.toLowerCase() === assign.toLowerCase(),
		);
		if (!match) throw new Error(`user ${JSON.stringify(assign)} not found in workspace members`);
		out.assigned_user_id = match.user_id;
	}
	return out;
}

/**
 * Resolve the collection `schema` param to the JSON string the create/update
 * endpoints persist. The catalog's `schema` is a structured CollectionSchema
 * object, but the MCP transport may deliver it either as a JSON-encoded string
 * or as a nested object — accept both. The `fields` compact DSL has no
 * browser-side parser (it lives only in the Go CLI), so rather than silently
 * dropping it we throw a precise error pointing the caller at `schema`.
 */
function collectionSchema(args: Record<string, unknown>): string | undefined {
	if (str(args, 'fields') !== undefined) {
		throw new Error(
			"the 'fields' DSL is not available in the browser WebMCP surface — " +
				"pass 'schema' (structured CollectionSchema JSON) instead",
		);
	}
	const schema = args.schema;
	if (schema === undefined || schema === null || schema === '') return undefined;
	if (typeof schema === 'string') return schema;
	if (typeof schema === 'object') return JSON.stringify(schema);
	throw new Error("'schema' must be a CollectionSchema object or JSON string");
}

/**
 * Roll the collection settings params (layout / default_view / board_group_by)
 * into the JSON `settings` string the endpoint persists — the browser mirror
 * of the CLI's CollectionSettings build. Returns undefined when none were
 * supplied so an update doesn't clobber existing settings.
 */
function collectionSettings(args: Record<string, unknown>): string | undefined {
	const settings: Record<string, unknown> = {};
	const layout = str(args, 'layout');
	if (layout !== undefined) settings.layout = layout;
	const defaultView = str(args, 'default_view');
	if (defaultView !== undefined) settings.default_view = defaultView;
	const boardGroupBy = str(args, 'board_group_by');
	if (boardGroupBy !== undefined) settings.board_group_by = boardGroupBy;
	if (Object.keys(settings).length === 0) return undefined;
	return JSON.stringify(settings);
}

function ok(result: unknown): DispatchResult {
	return { content: [{ type: 'text', text: JSON.stringify(result ?? null) }] };
}

function errResult(message: string): DispatchResult {
	return { content: [{ type: 'text', text: message }], isError: true };
}

// ── link/unlink routing ──────────────────────────────────────────────────
//
// Mirrors internal/mcp/catalog_item.go::itemLinkRoutes. The catalog's uniform
// (ref, target, link_type) shape maps onto the web `links` API, which is a
// lower-level graph surface: POST /items/{source}/links takes a TARGET UUID +
// a canonical graph type, and DELETE /links/{id} removes by edge id.
//
// Two translations vs. the catalog:
//   - link_type → canonical graph type. The catalog's `blocked-by` is the
//     inverse of `blocks` (no separate graph type): source/target swap and the
//     stored type becomes `blocks`. `split-from` stores as `split_from`.
//   - ref/target → ids. The path source resolves a ref/slug server-side, but
//     the target must be a UUID, so we resolve the target ref via items.get.
//     For inverted types the roles swap before resolution.

interface LinkRoute {
	/** Canonical graph link_type stored server-side. */
	graphType: string;
	/** When true, swap (ref, target) so the stored edge points the right way. */
	inverted: boolean;
}

const LINK_ROUTES: Record<string, LinkRoute> = {
	blocks: { graphType: 'blocks', inverted: false },
	// "ref blocked-by target" == "target blocks ref": swap, store as blocks.
	'blocked-by': { graphType: 'blocks', inverted: true },
	supersedes: { graphType: 'supersedes', inverted: false },
	implements: { graphType: 'implements', inverted: false },
	'split-from': { graphType: 'split_from', inverted: false },
};

function resolveLinkRoute(args: Record<string, unknown>): {
	sourceRef: string;
	targetRef: string;
	route: LinkRoute;
} {
	const ref = requireRef(args);
	const target = requireArg(args, 'target');
	const linkType = str(args, 'link_type');
	const route = linkType ? LINK_ROUTES[linkType] : undefined;
	if (!route) {
		throw new Error(
			`link_type is required (one of: ${Object.keys(LINK_ROUTES).sort().join(', ')})`,
		);
	}
	const [sourceRef, targetRef] = route.inverted ? [target, ref] : [ref, target];
	return { sourceRef, targetRef, route };
}

async function dispatchItemLink(
	api: Api,
	ws: string,
	args: Record<string, unknown>,
): Promise<unknown> {
	const { sourceRef, targetRef, route } = resolveLinkRoute(args);
	// The target must be a UUID for the web links API; resolve the ref.
	const targetItem = await api.items.get(ws, targetRef);
	return api.links.create(ws, sourceRef, {
		target_id: targetItem.id,
		link_type: route.graphType,
	});
}

async function dispatchItemUnlink(
	api: Api,
	ws: string,
	args: Record<string, unknown>,
): Promise<unknown> {
	const { sourceRef, targetRef, route } = resolveLinkRoute(args);
	const targetItem = await api.items.get(ws, targetRef);
	// The web links API deletes by edge id, so find the matching edge from the
	// source item's link list.
	const links = await api.links.list(ws, sourceRef);
	const match = links.find(
		(l) => l.link_type === route.graphType && l.target_id === targetItem.id,
	);
	if (!match) {
		throw new Error(`no ${route.graphType} link to ${targetRef} to remove`);
	}
	return api.links.delete(ws, match.id);
}

// ── dispatch table ─────────────────────────────────────────────────────────
//
// Keyed by `${toolName}:${action}`. Holds BOTH reads and writes — the read/
// write split is enforced by the served `read_only` flag + the per-invocation
// consent gate (DR-2), not by which table an action lives in. Each handler
// receives the ROUTE wsSlug — never an arg-supplied one (DR-4).
//
// pad_project next/standup/changelog and any other catalog read with no
// browser REST mapping are intentionally absent and fall through to the "not
// available in browser" branch rather than being faked (TASK-1894 wires
// next/standup/changelog once their backend endpoints exist).

const HANDLERS: Record<string, Handler> = {
	// ── pad_search ──
	'pad_search:query': (api, ws, args) =>
		api.search(str(args, 'query') ?? str(args, 'q') ?? '', {
			workspace: ws,
			collection: str(args, 'collection'),
			status: str(args, 'status'),
			priority: str(args, 'priority'),
			limit: num(args, 'limit'),
		}),

	// ── pad_item reads ──
	'pad_item:list': (api, ws, args) =>
		api.items.list(ws, {
			collection: str(args, 'collection'),
			status: str(args, 'status'),
			priority: str(args, 'priority'),
			parent: str(args, 'parent'),
			tag: str(args, 'tag'),
			limit: num(args, 'limit'),
		}),
	'pad_item:get': (api, ws, args) => api.items.get(ws, requireRef(args)),
	'pad_item:deps': (api, ws, args) => api.links.list(ws, requireRef(args)),
	'pad_item:list-comments': (api, ws, args) =>
		api.comments.list(ws, requireRef(args)),
	'pad_item:starred': (api, ws, args) =>
		api.items.starred(ws, {
			include_terminal: bool(args, 'all') === true ? true : undefined,
		}),
	'pad_item:backlinks': (api, ws, args) =>
		api.items.backlinks(ws, requireRef(args), {
			limit: num(args, 'limit'),
			offset: num(args, 'offset'),
		}),
	// export is read-only (side-effect-free): returns the artifact text.
	'pad_item:export': (api, ws, args) =>
		api.exportItemArtifact(ws, requireRef(args)),

	// ── pad_item writes ──
	'pad_item:create': async (api, ws, args) => {
		const collection = requireArg(args, 'collection');
		const title = requireArg(args, 'title');
		const data: ItemCreate = { title, source: 'web' };
		const content = str(args, 'content');
		if (content !== undefined) data.content = content;
		const fields = buildFieldsJSON(args);
		if (fields !== undefined) data.fields = fields;
		const tags = buildTagsJSON(args);
		if (tags !== undefined) data.tags = tags;
		// role / assign resolve to ids; throws (not a silent drop) on miss.
		const { agent_role_id, assigned_user_id } = await resolveAssignment(api, ws, args);
		if (agent_role_id !== undefined) data.agent_role_id = agent_role_id;
		if (assigned_user_id !== undefined) data.assigned_user_id = assigned_user_id;
		return api.items.create(ws, collection, data);
	},
	'pad_item:update': async (api, ws, args) => {
		const ref = requireRef(args);
		const data: ItemUpdate = { source: 'web' };
		const title = str(args, 'title');
		if (title !== undefined) data.title = title;
		const content = str(args, 'content');
		if (content !== undefined) data.content = content;
		const fields = buildFieldsJSON(args);
		if (fields !== undefined) data.fields = fields;
		const tags = buildTagsJSON(args);
		if (tags !== undefined) data.tags = tags;
		const comment = str(args, 'comment');
		if (comment !== undefined) data.comment = comment;
		if (bool(args, 'force') === true) data.force = true;
		const { agent_role_id, assigned_user_id } = await resolveAssignment(api, ws, args);
		if (agent_role_id !== undefined) data.agent_role_id = agent_role_id;
		if (assigned_user_id !== undefined) data.assigned_user_id = assigned_user_id;
		return api.items.update(ws, ref, data);
	},
	'pad_item:delete': (api, ws, args) => api.items.delete(ws, requireRef(args)),
	'pad_item:restore': (api, ws, args) => api.items.restore(ws, requireRef(args)),
	'pad_item:move': (api, ws, args) => {
		// The catalog's `field` param maps to the move endpoint's
		// field_overrides (mirrors the CLI's `pad item move --field`).
		const overrides = parseFieldKVP(args);
		return api.items.move(
			ws,
			requireRef(args),
			requireArg(args, 'target_collection'),
			Object.keys(overrides).length > 0 ? overrides : undefined,
			{ force: bool(args, 'force') === true ? true : undefined },
		);
	},
	'pad_item:link': (api, ws, args) => dispatchItemLink(api, ws, args),
	'pad_item:unlink': (api, ws, args) => dispatchItemUnlink(api, ws, args),
	'pad_item:star': (api, ws, args) => api.items.star(ws, requireRef(args)),
	'pad_item:unstar': (api, ws, args) => api.items.unstar(ws, requireRef(args)),
	'pad_item:comment': (api, ws, args) =>
		api.comments.create(ws, requireRef(args), {
			body: requireArg(args, 'message'),
			parent_id: str(args, 'reply_to'),
			source: 'web',
		}),
	'pad_item:bulk-update': (api, ws, args) => {
		const ids = strArray(args, 'refs');
		if (!ids || ids.length === 0) {
			throw new Error('refs is required (array of item references)');
		}
		const status = str(args, 'status');
		const priority = str(args, 'priority');
		if (status === undefined && priority === undefined) {
			throw new Error('bulk-update requires at least one of status / priority');
		}
		// The web bulk endpoint applies ONE verb per call (handlers_items_bulk.go):
		// status rides the `move` verb, priority the `set-priority` verb. Mirror
		// that one-verb-per-request shape (the MCP/CLI bulk-update sets both in a
		// single command, but the web endpoint is split — surface the limit
		// honestly rather than silently dropping one).
		if (status !== undefined && priority !== undefined) {
			throw new Error(
				'bulk-update accepts status OR priority per call in the browser, not both',
			);
		}
		const force = bool(args, 'force') === true ? true : undefined;
		if (status !== undefined) {
			return api.items.bulk(ws, { op: 'move', ids, status, force });
		}
		return api.items.bulk(ws, { op: 'set-priority', ids, priority: priority!, force });
	},
	// import ingests a portable artifact (YAML frontmatter + body). The
	// catalog passes the full artifact text in `artifact` (NOT `content`).
	'pad_item:import': (api, ws, args) =>
		api.importArtifact(ws, requireArg(args, 'artifact')),

	// ── pad_project reads — only dashboard has a browser REST endpoint today.
	'pad_project:dashboard': (api, ws) => api.dashboard.get(ws),

	// ── pad_collection ──
	'pad_collection:list': (api, ws) => api.collections.list(ws),
	'pad_collection:create': (api, ws, args) =>
		api.collections.create(ws, {
			name: requireArg(args, 'name'),
			slug: str(args, 'slug'),
			prefix: str(args, 'prefix'),
			icon: str(args, 'icon'),
			description: str(args, 'description'),
			schema: collectionSchema(args),
			settings: collectionSettings(args),
		}),
	'pad_collection:update': (api, ws, args) =>
		api.collections.update(ws, requireArg(args, 'slug'), {
			name: str(args, 'name'),
			prefix: str(args, 'prefix'),
			icon: str(args, 'icon'),
			description: str(args, 'description'),
			schema: collectionSchema(args),
			settings: collectionSettings(args),
			sort_order: num(args, 'sort_order'),
		}),
	'pad_collection:delete': (api, ws, args) =>
		api.collections.delete(ws, requireArg(args, 'slug')),

	// ── pad_role ──
	'pad_role:list': (api, ws) => api.agentRoles.list(ws),
	'pad_role:create': (api, ws, args) =>
		api.agentRoles.create(ws, {
			name: requireArg(args, 'name'),
			slug: str(args, 'slug'),
			description: str(args, 'description'),
			icon: str(args, 'icon'),
			tools: str(args, 'tools'),
		}),
	'pad_role:update': (api, ws, args) =>
		api.agentRoles.update(ws, requireArg(args, 'slug'), {
			name: str(args, 'name'),
			// catalog `new_slug` is the rename target; the web update body's
			// `slug` field IS the new slug (the URL path carries the current one).
			slug: str(args, 'new_slug'),
			description: str(args, 'description'),
			icon: str(args, 'icon'),
			tools: str(args, 'tools'),
			sort_order: num(args, 'sort_order'),
		}),
	'pad_role:delete': (api, ws, args) =>
		api.agentRoles.delete(ws, requireArg(args, 'slug')),

	// ── pad_playbook ──
	'pad_playbook:list': (api, ws) => api.playbooks.list(ws),
	'pad_playbook:get': (api, ws, args) => api.playbooks.get(ws, requireRef(args)),
	// run is side-effect-free server-side (parses + binds, the agent executes),
	// classified read_only in the catalog.
	'pad_playbook:run': (api, ws, args) =>
		api.playbooks.run(ws, requireRef(args), {
			args: (args.args as Record<string, unknown> | undefined) ?? undefined,
			raw_args: strArray(args, 'raw_args'),
		}),

	// ── pad_library ──
	'pad_library:list': (api) => api.library.get(),
	'pad_library:get': (api) => api.library.get(),
	'pad_library:activate': (api, ws, args) =>
		api.library.activateByTitle(ws, requireArg(args, 'title')),

	// ── pad_workspace reads ──
	'pad_workspace:list': (api) => api.workspaces.list(),

	// ── pad_meta ──
	// bootstrap → GET /workspaces/{ws}/agent/bootstrap (scope addition,
	// TASK-1893 comment). Read-only one-shot workspace context.
	'pad_meta:bootstrap': (api, ws) => api.agentBootstrap(ws),
};

// ── dispatcher ─────────────────────────────────────────────────────────────

/**
 * Route a WebMCP tool invocation to the api client.
 *
 * @param api      the singleton api client (injected for testability)
 * @param wsSlug   the ROUTE workspace slug — the only workspace authority
 * @param isReadOnlyAction  `(toolName, action) → boolean` from the descriptor's
 *                          action map (the Go read set, served over the wire).
 *                          Retained for parity / future per-read-write
 *                          branching; the route table itself is the source of
 *                          truth for what's wired.
 * @param toolName e.g. "pad_item"
 * @param args     the raw args object from the agent (includes `action`)
 *
 * DR-4 rejection: a non-empty agent-supplied `workspace` arg is rejected
 * outright. The route wsSlug is always used.
 */
export async function dispatch(
	api: Api,
	wsSlug: string,
	_isReadOnlyAction: (toolName: string, action: string) => boolean | undefined,
	toolName: string,
	args: Record<string, unknown>,
): Promise<DispatchResult> {
	// DR-4: the workspace is route-bound. Reject any attempt to set it — even
	// when it equals the route slug, so the contract is unambiguous and a
	// future route mismatch can't slip through.
	if ('workspace' in args && args.workspace !== undefined && args.workspace !== '') {
		return errResult(
			"the 'workspace' argument is not accepted — WebMCP tools always " +
				`operate on the current workspace (${wsSlug})`,
		);
	}

	if (!wsSlug) {
		return errResult('no active workspace');
	}

	const action = str(args, 'action');
	if (!action) {
		return errResult(`missing required arg 'action' for ${toolName}`);
	}

	const key = `${toolName}:${action}`;
	const handler = HANDLERS[key];
	if (!handler) {
		// A catalog action with no browser mapping (e.g. pad_project.standup,
		// pending TASK-1894). Honest error, never a fake result or silent
		// no-op.
		return errResult(
			`${toolName}.${action} is not available in the browser WebMCP surface`,
		);
	}

	try {
		const result = await handler(api, wsSlug, args);
		return ok(result);
	} catch (e) {
		const message = e instanceof Error ? e.message : String(e);
		return errResult(`${toolName}.${action} failed: ${message}`);
	}
}
