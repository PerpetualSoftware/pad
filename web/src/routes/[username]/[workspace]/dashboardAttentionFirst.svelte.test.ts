// TASK-3588: Needs Attention renders first when it has anything.
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render } from '@testing-library/svelte';
import { tick } from 'svelte';

const mocks = vi.hoisted(() => ({
	itemEventCallbacks: [] as Array<(e: Record<string, unknown>) => void>,
	unsubscribed: 0,
	needsOnboarding: false,
	attention: [] as unknown[],
	suggested: [] as unknown[],
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
		dashboard: { get: vi.fn(async () => board()) },
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

function board() {
	return {
		summary: { total_items: 3, by_collection: {} },
		active_items: [{ slug: 'busy', title: 'Busy task', collection_slug: 'tasks', collection_icon: '✓', status: 'in-progress', updated_at: '2026-10-11T00:00:00Z' }],
		starred_items: [],
		active_plans: [],
		attention: mocks.attention,
		recent_activity: [],
		suggested_next: mocks.suggested,
		has_agent_activity: false,
		needs_onboarding: false,
		degraded: false,
		degraded_sections: [],
	};
}

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
	return r;
}

/** The section labels in document order. */
function sectionOrder(container: HTMLElement): string[] {
	return Array.from(container.querySelectorAll('.section-label')).map((e) => (e.textContent ?? '').trim());
}

beforeEach(() => {
	vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval'] });
	mocks.attention = [];
	mocks.suggested = [];
});

afterEach(() => {
	cleanup();
	vi.useRealTimers();
});

// TASK-3588: "what needs me?" is the daily question, so a populated Needs
// Attention renders first, above the inventory; empty, the layout is as it was.
describe('Needs Attention comes first (TASK-3588)', () => {
	it('populated: it is the first section, above Active Work, with its reason and link', async () => {
		mocks.attention = [{ type: 'stalled', item_slug: 'stuck', item_title: 'Stuck task', collection: 'tasks', reason: 'In progress for 9 days' }];
		mocks.suggested = [{ item_slug: 'next', item_title: 'Next task', collection: 'tasks', reason: 'High priority' }];
		const { container } = await mount();
		const order = sectionOrder(container);
		expect(order[0]).toBe('Needs Attention');
		expect(order.indexOf('Needs Attention')).toBeLessThan(order.indexOf('Active Work'));
		// Up Next stays where it was, below the inventory.
		expect(order.indexOf('Up Next')).toBeGreaterThan(order.indexOf('Active Work'));
		const section = container.querySelector('[data-testid="needs-attention"]')!;
		expect(section.textContent).toContain('In progress for 9 days');
		expect(section.querySelector('a')?.getAttribute('href')).toBe('/dave/ws/tasks/stuck');
		// Rendered once, not also in the old place.
		expect(order.filter((l) => l === 'Needs Attention')).toHaveLength(1);
	});

	it('empty: no attention section, and the order is unchanged', async () => {
		mocks.suggested = [{ item_slug: 'next', item_title: 'Next task', collection: 'tasks', reason: 'High priority' }];
		const { container } = await mount();
		const order = sectionOrder(container);
		expect(order).not.toContain('Needs Attention');
		expect(container.querySelector('[data-testid="needs-attention"]')).toBeNull();
		expect(order[0]).toBe('Active Work');
		expect(order.indexOf('Up Next')).toBeGreaterThan(order.indexOf('Collections'));
	});
});
