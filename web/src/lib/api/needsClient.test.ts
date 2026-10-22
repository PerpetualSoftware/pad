// TASK-3923: the three api.needs.* methods put the right URL, method and body
// on the wire.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api } from './client';

function okFetch(body: unknown) {
	return vi.fn(
		async (_url: string, _init?: RequestInit) =>
			new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } })
	);
}

beforeEach(() => vi.unstubAllGlobals());
afterEach(() => vi.unstubAllGlobals());

const marker = { seen_comment_at: 't', seen_comment_ids: ['c1'], seen_transition_seq: 3 };

describe('api.needs', () => {
	it('list: GET /me/needs with only the params given', async () => {
		const fn = okFetch({ rows: [], total: 0, hidden: 0, workspaces: [] });
		vi.stubGlobal('fetch', fn);
		await api.needs.list();
		expect(fn.mock.calls[0][0]).toBe('/api/v1/me/needs');
		expect(fn.mock.calls[0][1]?.method ?? 'GET').toBe('GET');
		await api.needs.list({ sort: 'importance', view: 'hidden', cursor: 'x y', limit: 20 });
		const u = new URL(String(fn.mock.calls[1][0]), 'http://h');
		expect(u.pathname).toBe('/api/v1/me/needs');
		expect(Object.fromEntries(u.searchParams)).toEqual({
			sort: 'importance',
			view: 'hidden',
			cursor: 'x y',
			limit: '20'
		});
	});

	it('snooze: PUT .../needs/snooze with the body as given', async () => {
		const fn = okFetch({ ref: 'TASK-5', snooze: {} });
		vi.stubGlobal('fetch', fn);
		await api.needs.snooze('my ws', 'TASK-5', { kind: 'defer', defer_until: '2026-10-23T07:00:00.000Z', marker });
		expect(fn.mock.calls[0][0]).toBe('/api/v1/workspaces/my%20ws/items/TASK-5/needs/snooze');
		const init = fn.mock.calls[0][1];
		expect(init?.method).toBe('PUT');
		expect(JSON.parse(String(init?.body))).toStrictEqual({
			kind: 'defer',
			defer_until: '2026-10-23T07:00:00.000Z',
			marker
		});
	});

	it('restore: DELETE the same route with no body', async () => {
		const fn = okFetch({ ref: 'TASK-5', snooze: null });
		vi.stubGlobal('fetch', fn);
		await api.needs.restore('ws', 'TASK-5');
		expect(fn.mock.calls[0][0]).toBe('/api/v1/workspaces/ws/items/TASK-5/needs/snooze');
		const init = fn.mock.calls[0][1];
		expect(init?.method).toBe('DELETE');
		expect(init?.body).toBeUndefined();
	});
});
