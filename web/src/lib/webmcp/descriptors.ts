// descriptors.ts — pure tool-surface JSON → ModelContextTool descriptor
// builder (PLAN-1888 / TASK-1892, pieces 3). No DOM, no fetch, no Svelte —
// fully unit-testable. The lifecycle layer (register.ts) feeds it the
// fetched tool-surface payload and registers the result.
//
// Two load-bearing correctness constraints, both unit-tested:
//   - DR-4: STRIP the `workspace` param from every descriptor's inputSchema.
//     The workspace must NOT be agent-settable; dispatch forces the route
//     wsSlug. `workspace` is dropped here so the param never even appears in
//     the schema the agent sees.
//   - DR-2: `annotations.readOnlyHint = true` only when EVERY action the tool
//     exposes is `read_only`. Mixed tools (e.g. pad_item) get no hint, so the
//     host prompts for per-invocation consent on writes.
//   - DR-2 (prompt-injection hardening): `annotations.untrustedContentHint =
//     true` for every tool whose output can echo user-authored workspace
//     content — i.e. every tool except a pure version/meta surface (every
//     action is content-free server introspection). Signals the agent to treat
//     the result as unverified input. NB: `pad_meta` carries the hint, since
//     its `bootstrap` action returns the workspace bootstrap blob.

import type {
	ToolSurfaceTool,
	ToolSurfaceParam,
	ToolSurfaceResponse,
} from '$lib/api/client';

/**
 * The descriptor a WebMCP tool registers with, paired with the metadata the
 * dispatcher needs at call time. `tool` is the spec-shaped object handed to
 * `registerTool`; `actions`/`hasWorkspaceParam` are retained so the
 * dispatcher can validate/route without re-parsing the schema.
 */
export interface BuiltToolDescriptor {
	tool: ModelContextTool;
	/** Action names this tool exposes, with read-only classification. */
	actions: Map<string, boolean>;
	/** True when the source ToolDef declared a `workspace` param (now stripped). */
	hasWorkspaceParam: boolean;
}

/** A single JSON-Schema property, derived from a tool-surface param. */
interface SchemaProperty {
	type: string;
	description?: string;
	enum?: string[];
}

// JSON-Schema `type` values the spec understands. The catalog emits CLI-ish
// type names ("flag", "ref", …); map the ones that aren't already valid
// JSON-Schema types so the descriptor is well-formed.
function jsonSchemaType(catalogType: string): string {
	switch (catalogType) {
		case 'flag':
			return 'boolean';
		case 'number':
			return 'number';
		case 'string':
		case 'ref':
		case 'enum':
		default:
			// `ref`/`enum`/unknown all serialize as strings over the wire.
			return 'string';
	}
}

function paramToProperty(param: ToolSurfaceParam): SchemaProperty {
	const prop: SchemaProperty = { type: jsonSchemaType(param.type) };
	if (param.description) prop.description = param.description;
	if (param.enum && param.enum.length > 0) prop.enum = [...param.enum];
	return prop;
}

/**
 * Derive `readOnlyHint` per DR-2: true only when the tool exposes at least one
 * action AND every action is `read_only`. A tool with zero actions (shouldn't
 * happen for catalog tools) returns false — the conservative direction.
 */
export function isAllReadOnly(tool: ToolSurfaceTool): boolean {
	if (!tool.actions || tool.actions.length === 0) return false;
	return tool.actions.every((a) => a.read_only);
}

// The catalog actions whose output is pure server introspection — version
// metadata, the tool-catalog dump, server-info — read from in-memory server
// state, never from any item/comment/dashboard. A tool is content-free ONLY if
// EVERY action it exposes is one of these. NB: `pad_meta` is NOT content-free
// despite its name — it also exposes `bootstrap`, which returns the workspace
// bootstrap blob (user + workspace content). So `pad_meta` correctly DOES carry
// untrustedContentHint; only a hypothetical pure version/meta surface wouldn't.
const META_INTROSPECTION_ACTIONS: ReadonlySet<string> = new Set([
	'server-info',
	'version',
	'tool-surface',
]);

/**
 * Derive `untrustedContentHint` per DR-2 (prompt-injection hardening): true for
 * every tool whose output can surface user-authored workspace content — i.e.
 * every catalog tool except a pure version/meta-only surface (every action is
 * server introspection). The hint tells the browser agent to treat the result
 * as unverified data so a malicious item body can't smuggle instructions back
 * to the agent.
 *
 * Derived from the action set, NOT the tool name: a tool surfaces content
 * unless EVERY action it exposes is a content-free introspection action. This
 * correctly flags `pad_meta` (its `bootstrap` action returns workspace content)
 * while still exempting a purely version/meta tool. A tool with zero actions
 * (shouldn't happen for catalog tools) is treated as content-surfacing — the
 * conservative direction.
 */
export function surfacesUntrustedContent(tool: ToolSurfaceTool): boolean {
	if (!tool.actions || tool.actions.length === 0) return true;
	return !tool.actions.every((a) => META_INTROSPECTION_ACTIONS.has(a.name));
}

/**
 * Build a single ModelContextTool descriptor from a tool-surface tool.
 *
 * - STRIPS the `workspace` param (DR-4) — it is never placed in the schema.
 * - `action` stays required (the catalog dispatch verb).
 * - readOnlyHint applied per DR-2.
 * - untrustedContentHint applied per DR-2 for content-surfacing tools.
 *
 * `execute` is supplied by the caller (the dispatcher closure) so this stays
 * pure and side-effect-free for testing.
 */
export function buildDescriptor(
	tool: ToolSurfaceTool,
	execute: (args: Record<string, unknown>) => Promise<ModelContextToolResult>,
): BuiltToolDescriptor {
	const properties: Record<string, SchemaProperty> = {};
	const required: string[] = [];
	let hasWorkspaceParam = tool.workspace === true;

	for (const param of tool.params ?? []) {
		// DR-4: the workspace is route-bound, never agent-settable. Drop it
		// from the schema entirely so the agent can't even name it. (The
		// dispatcher additionally rejects a supplied `workspace` arg as
		// belt-and-suspenders.)
		if (param.name === 'workspace') {
			hasWorkspaceParam = true;
			continue;
		}
		properties[param.name] = paramToProperty(param);
		// `action` is the only required param the catalog emits; everything
		// else is optional in the fat-tool union.
		if (param.name === 'action') required.push('action');
	}

	const annotations: ModelContextToolAnnotations = {};
	if (isAllReadOnly(tool)) annotations.readOnlyHint = true;
	if (surfacesUntrustedContent(tool)) annotations.untrustedContentHint = true;

	const descriptor: ModelContextTool = {
		name: tool.name,
		description: tool.description,
		inputSchema: {
			type: 'object',
			properties,
			...(required.length > 0 ? { required } : {}),
		},
		...(Object.keys(annotations).length > 0 ? { annotations } : {}),
		execute,
	};

	const actions = new Map<string, boolean>();
	for (const a of tool.actions ?? []) actions.set(a.name, a.read_only);

	return { tool: descriptor, actions, hasWorkspaceParam };
}

/**
 * Build descriptors for every tool in a tool-surface payload. `executeFor`
 * returns the `execute` closure for a given tool name (the dispatcher binds
 * the route wsSlug into it). Pure aside from invoking `executeFor`.
 */
export function buildDescriptors(
	surface: ToolSurfaceResponse,
	executeFor: (
		toolName: string,
	) => (args: Record<string, unknown>) => Promise<ModelContextToolResult>,
): BuiltToolDescriptor[] {
	return (surface.tools ?? []).map((tool) =>
		buildDescriptor(tool, executeFor(tool.name)),
	);
}
