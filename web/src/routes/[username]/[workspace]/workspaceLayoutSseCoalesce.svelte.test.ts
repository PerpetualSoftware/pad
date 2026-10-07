import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { createRawSnippet, tick } from 'svelte';

const api = vi.hoisted(() => ({
	auth: { session: vi.fn() },
	workspaces: { get: vi.fn(), me: vi.fn(), list: vi.fn() },
	collections: { list: vi.fn() },
	items: { starred: vi.fn(), list: vi.fn(), get: vi.fn() },
	dashboard: { get: vi.fn() },
	tags: { list: vi.fn() },
}));

vi.mock('$lib/api/client', () => ({
	api,
	setAccessRevokedHandler: () => {},
	// BUG-2983 added this seam; the layout calls it at module scope, so a mock
	// without it throws before any assertion runs.
	setIdentityProvider: () => {},
	setRateLimitHandler: () => {},
	isPlanLimitError: () => false,
	planLimitMessage: () => '',
	PadApiError: class extends Error {},
}));

vi.mock('$app/navigation', () => ({
	goto: vi.fn(),
	beforeNavigate: () => {},
	afterNavigate: () => {},
	invalidateAll: vi.fn(),
	pushState: vi.fn(),
	replaceState: vi.fn(),
}));

const itemCallbacks = vi.hoisted(() => [] as Array<(e: Record<string, unknown>) => unknown>);
const sse = vi.hoisted(() => ({
	connect: vi.fn(),
	disconnect: vi.fn(),
	onItemEvent: vi.fn((cb: (e: Record<string, unknown>) => void) => {
		itemCallbacks.push(cb);
		return () => {};
	}),
	onCollectionEvent: vi.fn(() => () => {}),
	onWorkspaceEvent: vi.fn(() => () => {}),
	onConnectionChange: vi.fn(() => () => {}),
}));
vi.mock('$lib/services/sse.svelte', () => ({ sseService: sse }));

const sync = vi.hoisted(() => ({
	init: vi.fn(),
	setWorkspace: vi.fn(),
	onSync: vi.fn(() => () => {}),
	markSynced: vi.fn(),
	syncNow: vi.fn(),
	destroy: vi.fn(),
}));
vi.mock('$lib/services/sync.svelte', () => ({ syncService: sync }));

vi.mock('$lib/webmcp/register', () => ({ registerWorkspaceTools: vi.fn(async () => null) }));

import Layout from './+layout.svelte';
import { authStore } from '$lib/stores/auth.svelte';
import { page } from '$app/state';

function sessionFor(id: string) {
	return { authenticated: true, user: { id, email: `${id}@example.com` } };
}

/** Counts how many times the leaf-page snippet has been constructed. */
let mountCount = 0;
const childSnippet = createRawSnippet(() => ({
	// Counted in `render`, not in the factory: the factory is invoked once per
	// snippet value, so a remount would not move it and the test would be
	// vacuous. `render` runs each time the block is (re)created.
	render: () => {
		mountCount++;
		return `<div>leaf</div>`;
	},
}));

async function settle(): Promise<void> {
	for (let i = 0; i < 10; i++) {
		await Promise.resolve();
		await tick();
	}
}

// TASK-2224, codex r2: SSE events are coalesced before they reconcile, so a
// layout destroyed inside the window holds callbacks awaiting a run that
// destroy CANCELS. A cancelled callback must stop: carrying on would reload the
// old workspace's collections and toast its events onto whatever comes next.
describe('workspace layout: an SSE callback stops when its coalesced run is cancelled', () => {
	beforeEach(async () => {
		vi.resetModules();
		itemCallbacks.length = 0;
		api.auth.session.mockReset();
		api.collections.list.mockReset();
		api.collections.list.mockResolvedValue([]);
		api.items.starred.mockResolvedValue([]);
		api.items.get.mockResolvedValue({ id: 'i1', slug: 'i1', title: 'x' });
		api.workspaces.get.mockResolvedValue({ id: 'w1', slug: 'ws', name: 'WS' });
		api.workspaces.me.mockResolvedValue({ role: 'owner' });
		api.dashboard.get.mockResolvedValue({ has_agent_activity: true });
		api.tags.list.mockResolvedValue([]);
		page.params.workspace = 'ws';
		page.params.username = 'alice';
		api.auth.session.mockResolvedValue(sessionFor('user-a'));
		await authStore.load();
	});

	afterEach(() => {
		cleanup();
		authStore.clear();
	});

	it('reloads nothing for an event whose layout was destroyed inside the window', async () => {
		const r = render(Layout, { props: { children: childSnippet } });
		await settle();
		expect(itemCallbacks.length, 'the layout did not subscribe to item events').toBeGreaterThan(0);
		const loadsBefore = api.collections.list.mock.calls.length;

		const pending = itemCallbacks.map((cb) =>
			Promise.resolve(cb({ type: 'item_created', item_id: 'i1', workspace_id: 'w1', title: 'x', source: 'cli' })),
		);
		r.unmount(); // inside the 300 ms window: cancels the pending reconcile
		await Promise.all(pending.map((p) => p.catch(() => undefined)));
		await new Promise((res) => setTimeout(res, 1500)); // past every window
		await settle();

		expect(api.collections.list.mock.calls.length, 'a destroyed layout reloaded collections for an event it no longer owns').toBe(loadsBefore);
	});

	it('control: a live layout reloads collections once for the event', async () => {
		render(Layout, { props: { children: childSnippet } });
		await settle();
		const loadsBefore = api.collections.list.mock.calls.length;
		for (const cb of itemCallbacks) void cb({ type: 'item_created', item_id: 'i1', workspace_id: 'w1', title: 'x', source: 'cli' });
		await new Promise((res) => setTimeout(res, 1500));
		await settle();
		expect(api.collections.list.mock.calls.length).toBe(loadsBefore + 1);
	});
});
