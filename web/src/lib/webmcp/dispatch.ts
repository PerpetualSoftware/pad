// dispatch.ts — the `(toolName, action, args) → client.ts` router for the
// WebMCP surface (PLAN-1888 / TASK-1892, piece 4). Pure/injectable: takes the
// `api` client + the route `wsSlug` as arguments so it's unit-testable with a
// mocked client and no browser.
//
// Two correctness constraints, both unit-tested:
//   - DR-4: the workspace is FROZEN to the route `wsSlug`. An agent-supplied
//     `workspace` arg is REJECTED (not silently overridden) so a stray /
//     malicious workspace can't slip through. Every read handler receives the
//     route wsSlug, never an arg-derived one.
//   - This task wires READ actions only. WRITE actions are recognized and
//     return a clear "not yet wired (TASK-3b)" error — never a silent no-op.

import type { api as ApiClient } from '$lib/api/client';

type Api = typeof ApiClient;

/** Result envelope the WebMCP host expects from `execute`. */
export type DispatchResult = ModelContextToolResult;

/** A read-action handler: pure call into the api client. */
type ReadHandler = (
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

function requireRef(args: Record<string, unknown>): string {
	const ref = str(args, 'ref') ?? str(args, 'slug');
	if (!ref) throw new Error("missing required arg 'ref'");
	return ref;
}

function ok(result: unknown): DispatchResult {
	return { content: [{ type: 'text', text: JSON.stringify(result) }] };
}

function errResult(message: string): DispatchResult {
	return { content: [{ type: 'text', text: message }], isError: true };
}

// ── read dispatch table ──────────────────────────────────────────────────
//
// Keyed by `${toolName}:${action}`. Only READ actions appear here (this
// task). Each handler receives the ROUTE wsSlug — never an arg-supplied one.
// Project next/standup/changelog and pad_meta/bootstrap have no clean REST
// read endpoint exposed to the browser today, so they're intentionally
// absent and fall through to the "not available in browser" branch rather
// than being faked.

const READ_HANDLERS: Record<string, ReadHandler> = {
	// pad_search
	'pad_search:query': (api, ws, args) =>
		api.search(str(args, 'query') ?? str(args, 'q') ?? '', {
			workspace: ws,
			collection: str(args, 'collection'),
			status: str(args, 'status'),
			priority: str(args, 'priority'),
			limit: num(args, 'limit'),
		}),

	// pad_item reads
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
	'pad_item:backlinks': (api, ws, args) =>
		api.items.backlinks(ws, requireRef(args), {
			limit: num(args, 'limit'),
			offset: num(args, 'offset'),
		}),

	// pad_project reads — only dashboard has a browser REST endpoint today.
	'pad_project:dashboard': (api, ws) => api.dashboard.get(ws),

	// pad_collection reads
	'pad_collection:list': (api, ws) => api.collections.list(ws),

	// pad_role reads
	'pad_role:list': (api, ws) => api.agentRoles.list(ws),

	// pad_playbook reads
	'pad_playbook:list': (api, ws) => api.playbooks.list(ws),
	'pad_playbook:get': (api, ws, args) =>
		api.playbooks.get(ws, requireRef(args)),

	// pad_library reads
	'pad_library:list': (api) => api.library.get(),
	'pad_library:get': (api) => api.library.get(),

	// pad_workspace reads
	'pad_workspace:list': (api) => api.workspaces.list(),
};

// ── dispatcher ─────────────────────────────────────────────────────────────

/**
 * Route a WebMCP tool invocation to the api client.
 *
 * @param api      the singleton api client (injected for testability)
 * @param wsSlug   the ROUTE workspace slug — the only workspace authority
 * @param isReadOnlyAction  `(toolName, action) → boolean` from the descriptor's
 *                          action map (the Go read set, served over the wire)
 * @param toolName e.g. "pad_item"
 * @param args     the raw args object from the agent (includes `action`)
 *
 * DR-4 rejection: a non-empty agent-supplied `workspace` arg is rejected
 * outright. The route wsSlug is always used.
 */
export async function dispatch(
	api: Api,
	wsSlug: string,
	isReadOnlyAction: (toolName: string, action: string) => boolean | undefined,
	toolName: string,
	args: Record<string, unknown>,
): Promise<DispatchResult> {
	// DR-4: the workspace is route-bound. Reject any attempt to set it.
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

	const readOnly = isReadOnlyAction(toolName, action);

	// Write actions: recognized but not yet wired (this task is read-only).
	// Never a silent no-op — return a precise, actionable error.
	if (readOnly === false) {
		return errResult(
			`${toolName}.${action} is a write action — not yet wired in the ` +
				'browser WebMCP surface (follows in TASK-3b)',
		);
	}

	const key = `${toolName}:${action}`;
	const handler = READ_HANDLERS[key];
	if (!handler) {
		// A read action the catalog exposes but with no browser REST mapping
		// (e.g. pad_project.standup, pad_meta.bootstrap). Honest error, not a
		// fake result.
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
