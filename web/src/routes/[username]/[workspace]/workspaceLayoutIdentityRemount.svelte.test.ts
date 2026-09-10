// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// BUG-3005, codex round 2 — the persistent workspace layout across an identity
// change. Two bindings, and neither is observable anywhere else (CONVE-19):
//
//   1. the layout re-runs its per-workspace work on the identity signal, and
//      DISCONNECTS the SSE stream first. `sseService.connect` is idempotent per
//      workspace, so without the explicit disconnect B's tab keeps the
//      EventSource A opened — still authorized, because signing in as B
//      replaces the cookie without destroying A's session row, which is
//      precisely the case BUG-3007's server-side close cannot see;
//   2. leaf pages are remounted, keyed on the identity EPOCH. Fourteen route
//      components under this layout own user-scoped state and key their reloads
//      on the workspace slug, so a same-route swap re-runs none of them.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { createRawSnippet, tick } from 'svelte';

const api = vi.hoisted(() => ({
	auth: { session: vi.fn() },
	workspaces: { get: vi.fn(), me: vi.fn(), list: vi.fn() },
	collections: { list: vi.fn() },
	items: { starred: vi.fn(), list: vi.fn() },
	dashboard: { get: vi.fn() },
	tags: { list: vi.fn() },
}));

vi.mock('$lib/api/client', () => ({
	api,
	setAccessRevokedHandler: () => {},
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

const sse = vi.hoisted(() => ({
	connect: vi.fn(),
	disconnect: vi.fn(),
	onItemEvent: vi.fn(() => () => {}),
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

describe('workspace layout across an identity change', () => {
	beforeEach(async () => {
		vi.resetModules();
		mountCount = 0;
		sse.connect.mockClear();
		sse.disconnect.mockClear();
		api.auth.session.mockReset();
		api.collections.list.mockResolvedValue([]);
		api.items.starred.mockResolvedValue([]);
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

	it('disconnects the previous identity\'s SSE stream and reconnects', async () => {
		render(Layout, { props: { children: childSnippet } });
		await settle();

		// PRECONDITION: the layout connected for A. Without this, "it
		// reconnected" is a claim about a stream that was never opened.
		expect(sse.connect).toHaveBeenCalled();
		const connectsBefore = sse.connect.mock.calls.length;
		const disconnectsBefore = sse.disconnect.mock.calls.length;

		api.auth.session.mockResolvedValue(sessionFor('user-b'));
		await authStore.load();
		await settle();

		// The DISCONNECT is the load-bearing half: `connect` is idempotent per
		// workspace, so reconnecting without it keeps A's EventSource.
		expect(sse.disconnect.mock.calls.length).toBeGreaterThan(disconnectsBefore);
		expect(sse.connect.mock.calls.length).toBeGreaterThan(connectsBefore);
	});

	it('remounts the leaf page so its route-local state cannot survive', async () => {
		render(Layout, { props: { children: childSnippet } });
		await settle();

		// PRECONDITION: mounted once for A.
		expect(mountCount).toBe(1);

		api.auth.session.mockResolvedValue(sessionFor('user-b'));
		await authStore.load();
		await settle();

		expect(mountCount).toBe(2);
	});

	it('does NOT remount when the session refetch returns the same user', async () => {
		// The counterfactual. Keying on anything that moves on an ordinary
		// session poll would tear down and refetch every leaf page during normal
		// use — worse than the staleness being fixed.
		render(Layout, { props: { children: childSnippet } });
		await settle();
		expect(mountCount).toBe(1);

		await authStore.load();
		await settle();

		expect(mountCount).toBe(1);
	});
});
