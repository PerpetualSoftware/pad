// TASK-3279 (PLAN-3002 Q9): an ephemeral tab becomes durable on any write in
// its workspace, and `request()` is where every REST write is seen. These
// tests drive the real `request()` through the public `api` object with a
// stubbed fetch, so they vouch for the wiring, not only for the classifier.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, onWorkspaceWrite, workspaceWriteSlug } from './client';

function jsonFetch(status = 200, headers: Record<string, string> = {}) {
	return vi.fn(
		async () =>
			new Response(status === 204 ? null : JSON.stringify({ tabs: [], id: 'x' }), {
				status,
				headers: { 'Content-Type': 'application/json', ...headers },
			}),
	);
}

let seen: string[] = [];
let stop: () => void;

beforeEach(() => {
	vi.unstubAllGlobals();
	seen = [];
	stop = onWorkspaceWrite((slug) => seen.push(slug));
});
afterEach(() => {
	stop();
	vi.unstubAllGlobals();
});

describe('which requests are workspace writes', () => {
	it.each([
		['PATCH', '/workspaces/ws/items/TASK-1', 'ws'],
		['POST', '/workspaces/ws/collections/tasks/items', 'ws'],
		['DELETE', '/workspaces/ws/items/TASK-1', 'ws'],
		['PUT', '/workspaces/ws/items/TASK-1/fields', 'ws'],
		['PATCH', '/workspaces/ws', 'ws'],
		['PATCH', '/workspaces/ws?x=1', 'ws'],
		['PATCH', '/workspaces/ws/items/TASK-1?source=collab-snapshot', 'ws'],
		['POST', '/workspaces/ws/items/TASK-1/comments', 'ws'],
		['POST', '/workspaces/ws/playbooks/PLAYB-1/versions', 'ws'],
	])('%s %s writes to %s', (method, path, slug) => {
		expect(workspaceWriteSlug(path, method)).toBe(slug);
	});

	it.each([
		['GET', '/workspaces/ws/items/TASK-1'],
		['HEAD', '/workspaces/ws/attachments/a'],
		[undefined, '/workspaces/ws/items'],
		// Read-only POSTs: sent by merely opening an item, or side-effect-free.
		['POST', '/workspaces/ws/items/TASK-1/collab-watermark'],
		['POST', '/workspaces/ws/items/TASK-1/copy/preflight'],
		['POST', '/workspaces/ws/playbooks/match'],
		['POST', '/workspaces/ws/playbooks/ship/run'],
		// Not under a workspace.
		['PATCH', '/me/workspace-tabs/ws'],
		['POST', '/invitations/abc/accept'],
		['POST', '/workspacesish/ws/items'],
	])('%s %s is not', (method, path) => {
		expect(workspaceWriteSlug(path, method as string | undefined)).toBeNull();
	});

	it('does not exempt a WRITE whose path only contains a read-only word', () => {
		// The copy itself mutates; only its preflight is read-only.
		expect(workspaceWriteSlug('/workspaces/ws/items/TASK-1/copy', 'POST')).toBe('ws');
		// The exemptions are POST-only.
		expect(workspaceWriteSlug('/workspaces/ws/items/TASK-1/collab-watermark', 'DELETE')).toBe('ws');
	});
});

describe('request() reports a write once it has succeeded', () => {
	it('reports a successful item update with its workspace', async () => {
		vi.stubGlobal('fetch', jsonFetch());
		await api.items.update('ws', 'TASK-1', { title: 't' } as never);
		expect(seen).toEqual(['ws']);
	});

	it('reports nothing for a read', async () => {
		vi.stubGlobal('fetch', jsonFetch());
		await api.workspaces.get('ws');
		expect(seen).toEqual([]);
	});

	it('reports nothing for a write the server refused', async () => {
		vi.stubGlobal(
			'fetch',
			vi.fn(async () => new Response(JSON.stringify({ error: { code: 'forbidden', message: 'no' } }), { status: 403 })),
		);
		await api.items.update('ws', 'TASK-1', { title: 't' } as never).catch(() => {});
		expect(seen).toEqual([]);
	});

	it('reports nothing for a write refused with 429, which is not retried', async () => {
		const fetch = vi.fn(
			async () =>
				new Response(JSON.stringify({ error: { code: 'rate_limited', message: 'slow' } }), {
					status: 429,
					headers: { 'Retry-After': '0' },
				}),
		);
		vi.stubGlobal('fetch', fetch);
		await api.items.update('ws', 'TASK-1', { title: 't' } as never).catch(() => {});
		expect(fetch).toHaveBeenCalledTimes(1);
		expect(seen).toEqual([]);
	});
});
