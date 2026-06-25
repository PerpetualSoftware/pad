import { describe, it, expect, vi } from 'vitest';
import { dispatch } from './dispatch';
import type { api as ApiClient } from '$lib/api/client';

// Build a minimal mock of the api client. Only the methods the dispatch
// table touches need to exist; cast through unknown to the full type.
function mockApi() {
	return {
		search: vi.fn(async () => ({ results: [] })),
		items: {
			list: vi.fn(async () => []),
			get: vi.fn(async () => ({ id: 'uuid-target', ref: 'TASK-1' })),
			backlinks: vi.fn(async () => []),
			create: vi.fn(async () => ({ ref: 'TASK-9' })),
			update: vi.fn(async () => ({ ref: 'TASK-1' })),
			delete: vi.fn(async () => undefined),
			restore: vi.fn(async () => ({ ref: 'TASK-1' })),
			move: vi.fn(async () => ({ ref: 'TASK-1' })),
			star: vi.fn(async () => undefined),
			unstar: vi.fn(async () => undefined),
			starred: vi.fn(async () => []),
			bulk: vi.fn(async () => ({ op: 'move', updated: [], failed: [] })),
		},
		links: {
			list: vi.fn(async () => [
				{ id: 'link-1', link_type: 'blocks', target_id: 'uuid-target' },
			]),
			create: vi.fn(async () => ({ id: 'link-2' })),
			delete: vi.fn(async () => undefined),
		},
		comments: {
			list: vi.fn(async () => []),
			create: vi.fn(async () => ({ id: 'c-1' })),
		},
		dashboard: { get: vi.fn(async () => ({ ok: true })) },
		collections: {
			list: vi.fn(async () => []),
			create: vi.fn(async () => ({ slug: 'risks' })),
			update: vi.fn(async () => ({ slug: 'risks' })),
			delete: vi.fn(async () => undefined),
		},
		agentRoles: {
			list: vi.fn(async () => []),
			get: vi.fn(async () => ({ id: 'role-uuid', slug: 'reviewer' })),
			create: vi.fn(async () => ({ slug: 'reviewer' })),
			update: vi.fn(async () => ({ slug: 'reviewer' })),
			delete: vi.fn(async () => undefined),
		},
		members: {
			list: vi.fn(async () => ({
				members: [
					{ user_id: 'user-uuid', user_name: 'Dave', user_email: 'dave@example.com' },
				],
				invitations: [],
			})),
		},
		playbooks: {
			list: vi.fn(async () => []),
			get: vi.fn(async () => ({})),
			run: vi.fn(async () => ({ body: 'do the thing' })),
		},
		library: {
			get: vi.fn(async () => ({})),
			activateByTitle: vi.fn(async () => ({ ref: 'CONVE-3' })),
		},
		workspaces: { list: vi.fn(async () => []) },
		exportItemArtifact: vi.fn(async () => ({ filename: 'x.pad.md', text: 'ARTIFACT' })),
		importArtifact: vi.fn(async () => ({ ref: 'PLAYB-7', slug: 'ship', warnings: [] })),
		agentBootstrap: vi.fn(async () => ({ needs_onboarding: false })),
	};
}

type Api = typeof ApiClient;

// Mirror the served read set: known reads → true, known writes → false.
// (The dispatcher no longer branches on this — the route table is the source
// of truth — but register.ts still supplies it, so we keep it realistic.)
const READ = new Set([
	'pad_search:query',
	'pad_item:list',
	'pad_item:get',
	'pad_item:deps',
	'pad_item:list-comments',
	'pad_item:starred',
	'pad_item:backlinks',
	'pad_item:export',
	'pad_project:dashboard',
	'pad_collection:list',
	'pad_role:list',
	'pad_playbook:list',
	'pad_playbook:get',
	'pad_playbook:run',
	'pad_library:list',
	'pad_library:get',
	'pad_workspace:list',
	'pad_meta:bootstrap',
]);
const isReadOnly = (tool: string, action: string): boolean | undefined => {
	if (READ.has(`${tool}:${action}`)) return true;
	if (
		['create', 'update', 'delete', 'import', 'comment', 'link', 'unlink', 'move',
			'restore', 'star', 'unstar', 'bulk-update', 'activate'].includes(action)
	)
		return false;
	return undefined;
};

const WS = 'my-workspace';

function parse(result: { content: { text: string }[]; isError?: boolean }) {
	return { isError: result.isError === true, text: result.content[0]?.text ?? '' };
}

function run(api: ReturnType<typeof mockApi>, tool: string, args: Record<string, unknown>) {
	return dispatch(api as unknown as Api, WS, isReadOnly, tool, args);
}

// ── Reads (regression — unchanged from 3a) ───────────────────────────────────

describe('dispatch — wsSlug injection (DR-4)', () => {
	it('passes the route wsSlug to a read handler, never an arg', async () => {
		const api = mockApi();
		await run(api, 'pad_item', { action: 'list', status: 'open' });
		expect(api.items.list).toHaveBeenCalledWith(WS, expect.objectContaining({ status: 'open' }));
	});

	it('injects wsSlug into search filters', async () => {
		const api = mockApi();
		await run(api, 'pad_search', { action: 'query', query: 'hello' });
		expect(api.search).toHaveBeenCalledWith('hello', expect.objectContaining({ workspace: WS }));
	});

	it('forwards ref to items.get', async () => {
		const api = mockApi();
		await run(api, 'pad_item', { action: 'get', ref: 'TASK-5' });
		expect(api.items.get).toHaveBeenCalledWith(WS, 'TASK-5');
	});
});

// ── Supplied-workspace rejection applies to writes too (DR-4) ────────────────

describe('dispatch — supplied-workspace rejection (DR-4)', () => {
	it('rejects an agent-supplied workspace arg outright on a read', async () => {
		const api = mockApi();
		const result = await run(api, 'pad_item', { action: 'list', workspace: 'other-workspace' });
		const { isError, text } = parse(result);
		expect(isError).toBe(true);
		expect(text).toMatch(/workspace/i);
		expect(api.items.list).not.toHaveBeenCalled();
	});

	it('rejects an agent-supplied workspace arg on a WRITE (create), no client call', async () => {
		const api = mockApi();
		const result = await run(api, 'pad_item', {
			action: 'create',
			collection: 'tasks',
			title: 'X',
			workspace: 'other-workspace',
		});
		expect(parse(result).isError).toBe(true);
		expect(api.items.create).not.toHaveBeenCalled();
	});

	it('rejects an agent-supplied workspace on delete even when it equals the route slug', async () => {
		const api = mockApi();
		const result = await run(api, 'pad_item', { action: 'delete', ref: 'TASK-1', workspace: WS });
		expect(parse(result).isError).toBe(true);
		expect(api.items.delete).not.toHaveBeenCalled();
	});

	it('ignores an empty-string workspace arg (treated as not supplied)', async () => {
		const api = mockApi();
		const result = await run(api, 'pad_item', { action: 'list', workspace: '' });
		expect(parse(result).isError).toBe(false);
		expect(api.items.list).toHaveBeenCalledWith(WS, expect.anything());
	});
});

// ── pad_item writes ─────────────────────────────────────────────────────────

describe('dispatch — pad_item writes', () => {
	it('create rolls flat fields into the fields JSON + injects ws + source', async () => {
		const api = mockApi();
		await run(api, 'pad_item', {
			action: 'create',
			collection: 'tasks',
			title: 'Fix bug',
			status: 'open',
			priority: 'high',
			parent: 'PLAN-3',
			content: 'body',
			tags: ['v1', 'frontend'],
		});
		expect(api.items.create).toHaveBeenCalledTimes(1);
		const [ws, coll, data] = api.items.create.mock.calls[0] as unknown as [string, string, any];
		expect(ws).toBe(WS);
		expect(coll).toBe('tasks');
		expect(data.content).toBe('body');
		expect(data.source).toBe('web');
		expect(JSON.parse(data.fields)).toEqual({ status: 'open', priority: 'high', parent: 'PLAN-3' });
		expect(JSON.parse(data.tags)).toEqual(['v1', 'frontend']);
	});

	it('create resolves role slug → agent_role_id and assign → assigned_user_id', async () => {
		const api = mockApi();
		await run(api, 'pad_item', {
			action: 'create',
			collection: 'tasks',
			title: 'X',
			role: 'reviewer',
			assign: 'dave@example.com',
		});
		expect(api.agentRoles.get).toHaveBeenCalledWith(WS, 'reviewer');
		expect(api.members.list).toHaveBeenCalledWith(WS);
		const data = (api.items.create.mock.calls[0] as unknown as [string, string, any])[2];
		expect(data.agent_role_id).toBe('role-uuid');
		expect(data.assigned_user_id).toBe('user-uuid');
	});

	it('update errors (no item write) when an assignee can not be resolved', async () => {
		const api = mockApi();
		api.members.list.mockResolvedValueOnce({ members: [], invitations: [] });
		const result = await run(api, 'pad_item', {
			action: 'update',
			ref: 'TASK-1',
			assign: 'ghost@example.com',
		});
		expect(parse(result).isError).toBe(true);
		expect(api.items.update).not.toHaveBeenCalled();
	});

	it('create errors without collection/title and never calls the client', async () => {
		const api = mockApi();
		const result = await run(api, 'pad_item', { action: 'create', title: 'no collection' });
		expect(parse(result).isError).toBe(true);
		expect(api.items.create).not.toHaveBeenCalled();
	});

	it('update omits fields JSON when only title changes (no field blow-away)', async () => {
		const api = mockApi();
		await run(api, 'pad_item', { action: 'update', ref: 'TASK-1', title: 'Renamed' });
		const [ws, ref, data] = api.items.update.mock.calls[0] as unknown as [string, string, any];
		expect(ws).toBe(WS);
		expect(ref).toBe('TASK-1');
		expect(data.title).toBe('Renamed');
		expect(data.fields).toBeUndefined();
	});

	it('update forwards status into fields + the audit comment + force', async () => {
		const api = mockApi();
		await run(api, 'pad_item', {
			action: 'update',
			ref: 'TASK-1',
			status: 'done',
			comment: 'shipped',
			force: true,
		});
		const data = (api.items.update.mock.calls[0] as unknown as [string, string, any])[2];
		expect(JSON.parse(data.fields)).toEqual({ status: 'done' });
		expect(data.comment).toBe('shipped');
		expect(data.force).toBe(true);
	});

	it('create maps `field` key=value entries into the fields JSON', async () => {
		const api = mockApi();
		await run(api, 'pad_item', {
			action: 'create',
			collection: 'tasks',
			title: 'X',
			field: ['due_date=2026-06-01', 'effort=l'],
		});
		const data = (api.items.create.mock.calls[0] as unknown as [string, string, any])[2];
		expect(JSON.parse(data.fields)).toEqual({ due_date: '2026-06-01', effort: 'l' });
	});

	it('create errors (no client call) on a malformed `field` entry — no silent drop', async () => {
		const api = mockApi();
		const result = await run(api, 'pad_item', {
			action: 'create',
			collection: 'tasks',
			title: 'X',
			field: ['not-a-pair'],
		});
		expect(parse(result).isError).toBe(true);
		expect(parse(result).text).toMatch(/field/i);
		expect(api.items.create).not.toHaveBeenCalled();
	});

	it('create errors on a non-string `field` entry — no silent drop', async () => {
		const api = mockApi();
		const result = await run(api, 'pad_item', {
			action: 'create',
			collection: 'tasks',
			title: 'X',
			field: [{ key: 'status', value: 'done' }],
		});
		expect(parse(result).isError).toBe(true);
		expect(api.items.create).not.toHaveBeenCalled();
	});

	it('delete maps to items.delete with the route ws', async () => {
		const api = mockApi();
		await run(api, 'pad_item', { action: 'delete', ref: 'TASK-1' });
		expect(api.items.delete).toHaveBeenCalledWith(WS, 'TASK-1');
	});

	it('move passes target_collection + force', async () => {
		const api = mockApi();
		await run(api, 'pad_item', {
			action: 'move',
			ref: 'TASK-1',
			target_collection: 'ideas',
			force: true,
		});
		expect(api.items.move).toHaveBeenCalledWith(WS, 'TASK-1', 'ideas', undefined, { force: true });
	});

	it('move passes `field` overrides through to field_overrides', async () => {
		const api = mockApi();
		await run(api, 'pad_item', {
			action: 'move',
			ref: 'TASK-1',
			target_collection: 'ideas',
			field: ['category=infra'],
		});
		expect(api.items.move).toHaveBeenCalledWith(
			WS,
			'TASK-1',
			'ideas',
			{ category: 'infra' },
			expect.anything(),
		);
	});

	it('comment maps message → body and reply_to → parent_id', async () => {
		const api = mockApi();
		await run(api, 'pad_item', {
			action: 'comment',
			ref: 'TASK-1',
			message: 'looks good',
			reply_to: 'c-0',
		});
		expect(api.comments.create).toHaveBeenCalledWith(
			WS,
			'TASK-1',
			expect.objectContaining({ body: 'looks good', parent_id: 'c-0' }),
		);
	});

	it('star/unstar map to the right client methods', async () => {
		const api = mockApi();
		await run(api, 'pad_item', { action: 'star', ref: 'TASK-1' });
		await run(api, 'pad_item', { action: 'unstar', ref: 'TASK-1' });
		expect(api.items.star).toHaveBeenCalledWith(WS, 'TASK-1');
		expect(api.items.unstar).toHaveBeenCalledWith(WS, 'TASK-1');
	});

	it('bulk-update (status) maps to the move verb with refs as ids', async () => {
		const api = mockApi();
		await run(api, 'pad_item', { action: 'bulk-update', refs: ['TASK-1', 'TASK-2'], status: 'done' });
		expect(api.items.bulk).toHaveBeenCalledWith(
			WS,
			expect.objectContaining({ op: 'move', ids: ['TASK-1', 'TASK-2'], status: 'done' }),
		);
	});

	it('bulk-update (priority) maps to the set-priority verb', async () => {
		const api = mockApi();
		await run(api, 'pad_item', { action: 'bulk-update', refs: ['TASK-1'], priority: 'high' });
		expect(api.items.bulk).toHaveBeenCalledWith(
			WS,
			expect.objectContaining({ op: 'set-priority', ids: ['TASK-1'], priority: 'high' }),
		);
	});

	it('bulk-update errors (no partial write) on a non-string ref element', async () => {
		const api = mockApi();
		const result = await run(api, 'pad_item', {
			action: 'bulk-update',
			refs: ['TASK-1', 123],
			status: 'done',
		});
		expect(parse(result).isError).toBe(true);
		expect(api.items.bulk).not.toHaveBeenCalled();
	});

	it('bulk-update errors (no client call) when refs missing', async () => {
		const api = mockApi();
		const result = await run(api, 'pad_item', { action: 'bulk-update', status: 'done' });
		expect(parse(result).isError).toBe(true);
		expect(api.items.bulk).not.toHaveBeenCalled();
	});
});

// ── pad_item link / unlink ───────────────────────────────────────────────────

describe('dispatch — pad_item link/unlink', () => {
	it('link resolves the target ref to an id and stores the canonical type', async () => {
		const api = mockApi();
		await run(api, 'pad_item', {
			action: 'link',
			ref: 'TASK-1',
			target: 'TASK-2',
			link_type: 'blocks',
		});
		expect(api.items.get).toHaveBeenCalledWith(WS, 'TASK-2');
		expect(api.links.create).toHaveBeenCalledWith(WS, 'TASK-1', {
			target_id: 'uuid-target',
			link_type: 'blocks',
		});
	});

	it('blocked-by inverts source/target before creating a blocks edge', async () => {
		const api = mockApi();
		await run(api, 'pad_item', {
			action: 'link',
			ref: 'TASK-1',
			target: 'TASK-2',
			link_type: 'blocked-by',
		});
		// source becomes the target (TASK-2), target id resolved from ref (TASK-1).
		expect(api.items.get).toHaveBeenCalledWith(WS, 'TASK-1');
		expect(api.links.create).toHaveBeenCalledWith(WS, 'TASK-2', {
			target_id: 'uuid-target',
			link_type: 'blocks',
		});
	});

	it('split-from stores as split_from', async () => {
		const api = mockApi();
		await run(api, 'pad_item', {
			action: 'link',
			ref: 'TASK-1',
			target: 'PLAN-2',
			link_type: 'split-from',
		});
		expect(api.links.create).toHaveBeenCalledWith(WS, 'TASK-1', {
			target_id: 'uuid-target',
			link_type: 'split_from',
		});
	});

	it('link errors on an unknown link_type, no client call', async () => {
		const api = mockApi();
		const result = await run(api, 'pad_item', {
			action: 'link',
			ref: 'TASK-1',
			target: 'TASK-2',
			link_type: 'nonsense',
		});
		expect(parse(result).isError).toBe(true);
		expect(api.links.create).not.toHaveBeenCalled();
	});

	it('unlink finds the matching edge and deletes by id', async () => {
		const api = mockApi();
		await run(api, 'pad_item', {
			action: 'unlink',
			ref: 'TASK-1',
			target: 'TASK-2',
			link_type: 'blocks',
		});
		expect(api.links.list).toHaveBeenCalledWith(WS, 'TASK-1');
		expect(api.links.delete).toHaveBeenCalledWith(WS, 'link-1');
	});
});

// ── pad_item export / import (artifact passthrough) ──────────────────────────

describe('dispatch — pad_item export/import', () => {
	it('export returns the artifact text', async () => {
		const api = mockApi();
		const result = await run(api, 'pad_item', { action: 'export', ref: 'PLAYB-1' });
		expect(api.exportItemArtifact).toHaveBeenCalledWith(WS, 'PLAYB-1');
		expect(parse(result).isError).toBe(false);
	});

	it('import passes the full artifact text through to importArtifact', async () => {
		const api = mockApi();
		const ARTIFACT = '---\ncollection: playbooks\n---\n# Ship\nbody';
		await run(api, 'pad_item', { action: 'import', artifact: ARTIFACT });
		expect(api.importArtifact).toHaveBeenCalledWith(WS, ARTIFACT);
	});

	it('import errors (no client call) when artifact is missing', async () => {
		const api = mockApi();
		const result = await run(api, 'pad_item', { action: 'import' });
		expect(parse(result).isError).toBe(true);
		expect(api.importArtifact).not.toHaveBeenCalled();
	});
});

// ── pad_collection / pad_role writes ─────────────────────────────────────────

describe('dispatch — collection + role writes', () => {
	it('collection create maps name + slug + schema', async () => {
		const api = mockApi();
		await run(api, 'pad_collection', {
			action: 'create',
			name: 'Risks',
			slug: 'risks',
			schema: '{}',
		});
		expect(api.collections.create).toHaveBeenCalledWith(
			WS,
			expect.objectContaining({ name: 'Risks', slug: 'risks', schema: '{}' }),
		);
	});

	it('collection create rolls layout/default_view/board_group_by into settings + prefix', async () => {
		const api = mockApi();
		await run(api, 'pad_collection', {
			action: 'create',
			name: 'Risks',
			prefix: 'RISK',
			layout: 'balanced',
			default_view: 'board',
			board_group_by: 'status',
		});
		const data = (api.collections.create.mock.calls[0] as unknown as [string, any])[1];
		expect(data.prefix).toBe('RISK');
		expect(JSON.parse(data.settings)).toEqual({
			layout: 'balanced',
			default_view: 'board',
			board_group_by: 'status',
		});
	});

	it('collection create accepts an object-shaped schema', async () => {
		const api = mockApi();
		await run(api, 'pad_collection', {
			action: 'create',
			name: 'Risks',
			schema: { fields: [{ key: 'status', type: 'select' }] },
		});
		const data = (api.collections.create.mock.calls[0] as unknown as [string, any])[1];
		expect(JSON.parse(data.schema)).toEqual({ fields: [{ key: 'status', type: 'select' }] });
	});

	it('collection create errors on the fields DSL (no browser parser), no client call', async () => {
		const api = mockApi();
		const result = await run(api, 'pad_collection', {
			action: 'create',
			name: 'Risks',
			fields: 'status:select:open,done',
		});
		expect(parse(result).isError).toBe(true);
		expect(parse(result).text).toMatch(/fields/i);
		expect(api.collections.create).not.toHaveBeenCalled();
	});

	it('collection update targets the slug + carries prefix/sort_order', async () => {
		const api = mockApi();
		await run(api, 'pad_collection', {
			action: 'update',
			slug: 'risks',
			name: 'Risk Register',
			prefix: 'RISK',
			sort_order: 3,
		});
		expect(api.collections.update).toHaveBeenCalledWith(
			WS,
			'risks',
			expect.objectContaining({ name: 'Risk Register', prefix: 'RISK', sort_order: 3 }),
		);
	});

	it('collection delete targets the slug', async () => {
		const api = mockApi();
		await run(api, 'pad_collection', { action: 'delete', slug: 'risks' });
		expect(api.collections.delete).toHaveBeenCalledWith(WS, 'risks');
	});

	it('role create maps name + description', async () => {
		const api = mockApi();
		await run(api, 'pad_role', { action: 'create', name: 'Reviewer', description: 'Reviews PRs' });
		expect(api.agentRoles.create).toHaveBeenCalledWith(
			WS,
			expect.objectContaining({ name: 'Reviewer', description: 'Reviews PRs' }),
		);
	});

	it('role update targets the slug and maps new_slug → body.slug + sort_order', async () => {
		const api = mockApi();
		await run(api, 'pad_role', {
			action: 'update',
			slug: 'reviewer',
			new_slug: 'pr-reviewer',
			icon: '👀',
			sort_order: 2,
		});
		expect(api.agentRoles.update).toHaveBeenCalledWith(
			WS,
			'reviewer',
			expect.objectContaining({ slug: 'pr-reviewer', icon: '👀', sort_order: 2 }),
		);
	});
});

// ── pad_playbook.run + pad_library.activate + pad_meta.bootstrap ─────────────

describe('dispatch — playbook/library/meta writes', () => {
	it('playbook run forwards ref + args + raw_args', async () => {
		const api = mockApi();
		await run(api, 'pad_playbook', {
			action: 'run',
			ref: 'ship',
			args: { stop_after_each: true },
			raw_args: ['PLAN-1'],
		});
		expect(api.playbooks.run).toHaveBeenCalledWith(
			WS,
			'ship',
			expect.objectContaining({ args: { stop_after_each: true }, raw_args: ['PLAN-1'] }),
		);
	});

	it('library activate resolves by title via the route ws', async () => {
		const api = mockApi();
		await run(api, 'pad_library', { action: 'activate', title: 'Ship tasks' });
		expect(api.library.activateByTitle).toHaveBeenCalledWith(WS, 'Ship tasks');
	});

	it('library activate errors (no client call) without a title', async () => {
		const api = mockApi();
		const result = await run(api, 'pad_library', { action: 'activate' });
		expect(parse(result).isError).toBe(true);
		expect(api.library.activateByTitle).not.toHaveBeenCalled();
	});

	it('meta bootstrap maps to agentBootstrap with the route ws', async () => {
		const api = mockApi();
		const result = await run(api, 'pad_meta', { action: 'bootstrap' });
		expect(api.agentBootstrap).toHaveBeenCalledWith(WS);
		expect(parse(result).isError).toBe(false);
	});
});

// ── Envelope + error behaviour ───────────────────────────────────────────────

describe('dispatch — result envelope + errors', () => {
	it('wraps a read result as JSON text content', async () => {
		const api = mockApi();
		const result = await run(api, 'pad_project', { action: 'dashboard' });
		const { isError, text } = parse(result);
		expect(isError).toBe(false);
		expect(JSON.parse(text)).toEqual({ ok: true });
	});

	it('errors clearly when action is missing', async () => {
		const api = mockApi();
		const result = await run(api, 'pad_item', {});
		expect(parse(result).isError).toBe(true);
		expect(parse(result).text).toMatch(/action/);
	});

	it('errors for a catalog action with no browser mapping (pad_project.standup)', async () => {
		const api = mockApi();
		const result = await run(api, 'pad_project', { action: 'standup' });
		const { isError, text } = parse(result);
		expect(isError).toBe(true);
		expect(text).toMatch(/not available/i);
	});

	it('surfaces a thrown client error as an error result (server ACL → tool error)', async () => {
		const api = mockApi();
		api.items.create.mockRejectedValueOnce(new Error('forbidden'));
		const result = await run(api, 'pad_item', {
			action: 'create',
			collection: 'tasks',
			title: 'X',
		});
		const { isError, text } = parse(result);
		expect(isError).toBe(true);
		expect(text).toMatch(/forbidden/);
	});

	it('errors when there is no active workspace', async () => {
		const api = mockApi();
		const result = await dispatch(api as unknown as Api, '', isReadOnly, 'pad_item', {
			action: 'list',
		});
		expect(parse(result).isError).toBe(true);
	});
});
