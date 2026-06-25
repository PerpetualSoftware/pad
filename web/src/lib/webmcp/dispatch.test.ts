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
			get: vi.fn(async () => ({ ref: 'TASK-1' })),
			backlinks: vi.fn(async () => []),
		},
		dashboard: { get: vi.fn(async () => ({ ok: true })) },
		collections: { list: vi.fn(async () => []) },
		agentRoles: { list: vi.fn(async () => []) },
		playbooks: { list: vi.fn(async () => []), get: vi.fn(async () => ({})) },
		library: { get: vi.fn(async () => ({})) },
		workspaces: { list: vi.fn(async () => []) },
	};
}

type Api = typeof ApiClient;

// All catalog read actions are read_only=true; writes false. The real
// lookup comes from the served read set; here we hardcode the relevant ones.
const READ = new Set([
	'pad_search:query',
	'pad_item:list',
	'pad_item:get',
	'pad_item:backlinks',
	'pad_project:dashboard',
	'pad_collection:list',
	'pad_role:list',
	'pad_playbook:list',
	'pad_playbook:get',
	'pad_library:list',
	'pad_workspace:list',
]);
const isReadOnly = (tool: string, action: string): boolean | undefined => {
	// Mirror the served read set: known reads → true, known writes → false.
	if (READ.has(`${tool}:${action}`)) return true;
	if (['create', 'update', 'delete', 'import'].includes(action)) return false;
	return undefined;
};

const WS = 'my-workspace';

function parse(result: { content: { text: string }[]; isError?: boolean }) {
	return { isError: result.isError === true, text: result.content[0]?.text ?? '' };
}

describe('dispatch — wsSlug injection (DR-4)', () => {
	it('passes the route wsSlug to a read handler, never an arg', async () => {
		const api = mockApi();
		await dispatch(api as unknown as Api, WS, isReadOnly, 'pad_item', {
			action: 'list',
			status: 'open',
		});
		expect(api.items.list).toHaveBeenCalledWith(WS, expect.objectContaining({ status: 'open' }));
	});

	it('injects wsSlug into search filters', async () => {
		const api = mockApi();
		await dispatch(api as unknown as Api, WS, isReadOnly, 'pad_search', {
			action: 'query',
			query: 'hello',
		});
		expect(api.search).toHaveBeenCalledWith('hello', expect.objectContaining({ workspace: WS }));
	});

	it('forwards ref to items.get', async () => {
		const api = mockApi();
		await dispatch(api as unknown as Api, WS, isReadOnly, 'pad_item', {
			action: 'get',
			ref: 'TASK-5',
		});
		expect(api.items.get).toHaveBeenCalledWith(WS, 'TASK-5');
	});
});

describe('dispatch — supplied-workspace rejection (DR-4)', () => {
	it('rejects an agent-supplied workspace arg outright', async () => {
		const api = mockApi();
		const result = await dispatch(api as unknown as Api, WS, isReadOnly, 'pad_item', {
			action: 'list',
			workspace: 'other-workspace',
		});
		const { isError, text } = parse(result);
		expect(isError).toBe(true);
		expect(text).toMatch(/workspace/i);
		// And it never reached the handler.
		expect(api.items.list).not.toHaveBeenCalled();
	});

	it('ignores an empty-string workspace arg (treated as not supplied)', async () => {
		const api = mockApi();
		const result = await dispatch(api as unknown as Api, WS, isReadOnly, 'pad_item', {
			action: 'list',
			workspace: '',
		});
		expect(parse(result).isError).toBe(false);
		expect(api.items.list).toHaveBeenCalledWith(WS, expect.anything());
	});

	it('rejects even when the supplied workspace equals the route slug', async () => {
		const api = mockApi();
		const result = await dispatch(api as unknown as Api, WS, isReadOnly, 'pad_item', {
			action: 'list',
			workspace: WS,
		});
		expect(parse(result).isError).toBe(true);
		expect(api.items.list).not.toHaveBeenCalled();
	});
});

describe('dispatch — write actions (TASK-3b)', () => {
	it('returns a precise not-wired error for a write action, no silent no-op', async () => {
		const api = mockApi();
		const result = await dispatch(api as unknown as Api, WS, isReadOnly, 'pad_item', {
			action: 'delete',
			ref: 'TASK-1',
		});
		const { isError, text } = parse(result);
		expect(isError).toBe(true);
		expect(text).toMatch(/TASK-3b/);
	});
});

describe('dispatch — read action result envelope', () => {
	it('wraps the client result as JSON text content', async () => {
		const api = mockApi();
		const result = await dispatch(api as unknown as Api, WS, isReadOnly, 'pad_project', {
			action: 'dashboard',
		});
		const { isError, text } = parse(result);
		expect(isError).toBe(false);
		expect(JSON.parse(text)).toEqual({ ok: true });
	});

	it('errors clearly when action is missing', async () => {
		const api = mockApi();
		const result = await dispatch(api as unknown as Api, WS, isReadOnly, 'pad_item', {});
		expect(parse(result).isError).toBe(true);
		expect(parse(result).text).toMatch(/action/);
	});

	it('errors for a read action with no browser mapping (e.g. pad_project.standup)', async () => {
		const api = mockApi();
		// standup is read_only in the catalog but has no browser handler.
		const isReadStandup = (t: string, a: string) =>
			a === 'standup' ? true : isReadOnly(t, a);
		const result = await dispatch(api as unknown as Api, WS, isReadStandup, 'pad_project', {
			action: 'standup',
		});
		const { isError, text } = parse(result);
		expect(isError).toBe(true);
		expect(text).toMatch(/not available/i);
	});

	it('surfaces a thrown client error as an error result', async () => {
		const api = mockApi();
		api.dashboard.get.mockRejectedValueOnce(new Error('boom'));
		const result = await dispatch(api as unknown as Api, WS, isReadOnly, 'pad_project', {
			action: 'dashboard',
		});
		const { isError, text } = parse(result);
		expect(isError).toBe(true);
		expect(text).toMatch(/boom/);
	});

	it('errors when there is no active workspace', async () => {
		const api = mockApi();
		const result = await dispatch(api as unknown as Api, '', isReadOnly, 'pad_item', {
			action: 'list',
		});
		expect(parse(result).isError).toBe(true);
	});
});
