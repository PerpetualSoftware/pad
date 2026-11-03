// TASK-2227: the dashboard's 30 s poll refetched the server's heaviest
// aggregation every tick in every open tab, hidden or not, even with the live
// stream connected and syncService already reloading on real changes (measured:
// 72 s idle with SSE connected = 8 calls / 112,890 B). A tick now does nothing
// in a hidden tab or while the stream is live; it runs when the stream is
// reconnecting, refused, or polling under the HTTP/1.1 budget (BUG-3320). A
// hidden tab that comes back without a live stream catches up once.
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render } from '@testing-library/svelte';
import { tick } from 'svelte';

const mocks = vi.hoisted(() => ({
	itemEventCallbacks: [] as Array<(e: Record<string, unknown>) => void>,
	unsubscribed: 0,
	needsOnboarding: true,
	sseStatus: 'connected' as string,
	hidden: false,
}));

// Reactive, so a leg can switch workspaces under the mounted page.
vi.mock('$app/state', async () => ({ page: (await import('../../../test/mocks/reactivePage.svelte')).page }));
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
		get status() {
			return mocks.sseStatus;
		},
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

Object.defineProperty(document, 'visibilityState', {
	configurable: true,
	get: () => (mocks.hidden ? 'hidden' : 'visible'),
});
const becomeVisible = () => {
	mocks.hidden = false;
	document.dispatchEvent(new Event('visibilitychange'));
};

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
	mocks.needsOnboarding = false;
	mocks.sseStatus = 'connected';
	mocks.hidden = false;
});

afterEach(() => {
	cleanup();
	vi.useRealTimers();
});

const tickPoll = async (n = 1) => {
	for (let i = 0; i < n; i++) {
		await vi.advanceTimersByTimeAsync(30_000);
		await flush();
	}
};

describe('the dashboard poll runs only when nothing else keeps the board live (TASK-2227)', () => {
	it('with the stream live, ticks fetch nothing', async () => {
		await mount();
		await tickPoll(3);
		expect(dashboardGets()).toBe(1);
	});

	for (const status of ['reconnecting', 'polling', 'disconnected', 'unauthorized']) {
		it(`with the stream ${status}, each tick reloads`, async () => {
			mocks.sseStatus = status;
			await mount();
			await tickPoll(2);
			expect(dashboardGets()).toBe(3);
		});
	}

	it('a hidden tab skips its ticks and catches up once when it comes back without a live stream', async () => {
		mocks.sseStatus = 'reconnecting';
		await mount();
		mocks.hidden = true;
		await tickPoll(3);
		expect(dashboardGets(), 'a hidden tab polled').toBe(1);
		becomeVisible();
		await flush();
		expect(dashboardGets(), 'the tab did not catch up when it became visible').toBe(2);
	});

	it('a tab that comes back with the stream live does not reload (sync covers it)', async () => {
		await mount();
		mocks.hidden = true;
		await tickPoll(1);
		becomeVisible();
		await flush();
		expect(dashboardGets()).toBe(1);
	});
});
