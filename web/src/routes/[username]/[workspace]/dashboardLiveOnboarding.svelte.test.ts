// BUG-3447: while an agent onboards a new workspace, the launchpad (and the
// swap to the dashboard once the first item exists) waited on the 30 s poll:
// item and collection events updated the sidebar, not this page. While the
// workspace still needs onboarding, the page now refetches on those events
// (coalesced), and it does not once onboarding is over: the board is the
// aggregated view the poll and sync already keep.
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render } from '@testing-library/svelte';
import { tick } from 'svelte';

const mocks = vi.hoisted(() => ({
	itemEventCallbacks: [] as Array<(e: Record<string, unknown>) => void>,
	unsubscribed: 0,
	needsOnboarding: true,
}));

vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		identityEpoch: 0,
		userId: 'u1',
		user: { id: 'u1', name: 'A', email: 'a@example.com' },
		mcpPublicUrl: '',
		identityFence: () => () => true,
		onIdentityChange: () => () => {},
	},
}));
vi.mock('$lib/api/client', () => ({
	api: {
		dashboard: { get: vi.fn(async () => board(mocks.needsOnboarding)) },
		collections: { list: vi.fn(async () => []) },
		workspaces: { claimCode: vi.fn(async () => ({ code: 'x', expires_at: '' })) },
	},
	PadApiError: class PadApiError extends Error {},
}));
vi.mock('$lib/services/sse.svelte', () => ({
	sseService: {
		onItemEvent: (cb: (e: Record<string, unknown>) => void) => {
			mocks.itemEventCallbacks.push(cb);
			return () => {
				mocks.unsubscribed++;
				mocks.itemEventCallbacks = mocks.itemEventCallbacks.filter((c) => c !== cb);
			};
		},
	},
}));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: {
		setCurrent: vi.fn(async () => {}),
		get current() { return { id: 'w1', slug: 'ws', name: 'WS' }; },
		get isOwner() { return true; },
		get membershipKnown() { return true; },
		get currentRole() { return 'owner'; },
		get workspaces() { return []; },
		loadAll: vi.fn(async () => {}),
	},
}));
vi.mock('$lib/services/sync.svelte', () => ({
	syncService: { onSync: () => () => {}, start: () => {}, stop: () => {} },
}));
vi.mock('$lib/stores/ui.svelte', () => ({
	uiStore: {
		get connectAfterNavigateSlug() { return null; },
		consumeConnectAfterNavigate() {},
		requestQuickAdd() {},
	},
}));
vi.mock('$lib/stores/collections.svelte', () => ({ collectionStore: { loadCollections: vi.fn() } }));
vi.mock('$lib/stores/title.svelte', () => ({ titleStore: { setPageTitle() {} } }));
vi.mock('$lib/scroll/restore.svelte', () => ({
	createScrollRestoration: () => ({ snapshot: { capture: () => null, restore: () => {} } }),
}));

import { api } from '$lib/api/client';
import { page } from '$app/state';
import DashboardPage from './+page.svelte';

function board(needsOnboarding: boolean) {
	return {
		summary: { total_items: 0, by_collection: {} },
		active_items: [],
		starred_items: [],
		active_plans: [],
		attention: [],
		recent_activity: [],
		suggested_next: [],
		has_agent_activity: false,
		needs_onboarding: needsOnboarding,
		degraded: false,
		degraded_sections: [],
	};
}

const dashboardGets = () => (api.dashboard.get as unknown as { mock: { calls: unknown[] } }).mock.calls.length;

async function flush() {
	for (let i = 0; i < 10; i++) {
		await Promise.resolve();
		await tick();
	}
}

async function mount() {
	page.params = { username: 'dave', workspace: 'ws' };
	page.url = new URL('http://localhost/dave/ws');
	const r = render(DashboardPage);
	await flush();
	expect(dashboardGets(), 'the first load never ran').toBe(1);
	expect(mocks.itemEventCallbacks.length, 'the page did not subscribe to item events').toBeGreaterThan(0);
	return r;
}

function fire(event: Record<string, unknown>) {
	for (const cb of [...mocks.itemEventCallbacks]) cb({ workspace_id: 'w1', ...event });
}

beforeEach(() => {
	vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval'] });
	(api.dashboard.get as unknown as { mockClear: () => void }).mockClear();
	mocks.itemEventCallbacks = [];
	mocks.unsubscribed = 0;
	mocks.needsOnboarding = true;
});

afterEach(() => {
	cleanup();
	vi.useRealTimers();
});

describe('the launchpad follows the agent live (BUG-3447)', () => {
	it('refetches once for a burst of item and collection events while onboarding', async () => {
		await mount();
		fire({ type: 'collection_updated', collection_id: 'c1' });
		fire({ type: 'item_created', item_id: 'i1' });
		fire({ type: 'item_created', item_id: 'i2' });
		await vi.advanceTimersByTimeAsync(2000);
		await flush();
		expect(dashboardGets(), 'one coalesced refetch, well before the 30 s poll').toBe(2);
	});

	it('does not refetch on events once onboarding is over (the poll and sync own the board)', async () => {
		mocks.needsOnboarding = false;
		await mount();
		fire({ type: 'item_created', item_id: 'i1' });
		await vi.advanceTimersByTimeAsync(2000);
		await flush();
		expect(dashboardGets()).toBe(1);
	});

	it('stops listening when the page goes away', async () => {
		const r = await mount();
		r.unmount();
		expect(mocks.unsubscribed).toBeGreaterThan(0);
	});
});
