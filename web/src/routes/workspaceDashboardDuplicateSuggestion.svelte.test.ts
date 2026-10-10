import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';
import { page } from '$app/state';

/**
 * BUG-3538: suggested_next can carry one item twice: two fired reminders on
 * an item are two entries by design, and before the server fix an item under
 * two active plans was one entry per plan. The list was keyed by item_slug,
 * Svelte throws on a duplicate key during render (production builds too), and
 * the dashboard stayed on its loading skeleton (cookie-sales, day 90).
 */
const dashboardGet = vi.hoisted(() => vi.fn<(slug: string) => Promise<unknown>>());

vi.mock('$lib/services/sync.svelte', () => ({
	syncService: { onSync: () => () => {}, start: () => {}, stop: () => {} }
}));
vi.mock('$lib/api/client', () => ({
	api: {
		dashboard: { get: (slug: string) => dashboardGet(slug) },
		collections: { list: vi.fn().mockResolvedValue([]) },
		workspaces: {
			get: vi.fn().mockResolvedValue({ id: 'w1', slug: 'ws', name: 'WS' }),
			me: vi.fn().mockResolvedValue({ role: 'owner' }),
			list: vi.fn().mockResolvedValue([])
		}
	},
	PadApiError: class extends Error {}
}));

const { default: DashboardPage } = await import('./[username]/[workspace]/+page.svelte');

const sug = (reason: string) => ({
	item_slug: 'scope-rls',
	item_ref: 'TASK-13',
	item_title: 'Scope RLS',
	collection: 'tasks',
	reason
});

let host: HTMLElement;
let app: Record<string, unknown> | null = null;

beforeEach(() => {
	host = document.createElement('div');
	document.body.appendChild(host);
	page.params.workspace = 'ws';
	page.params.username = 'alice';
});
afterEach(() => {
	if (app) unmount(app as never);
	app = null;
	host.remove();
});

describe('workspace dashboard — a repeated item in suggested_next (BUG-3538)', () => {
	it('renders the dashboard, both entries, and no skeleton', async () => {
		dashboardGet.mockResolvedValue({
			summary: { total_items: 1, by_collection: {} },
			active_items: [],
			starred_items: [],
			active_plans: [],
			attention: [],
			recent_activity: [],
			suggested_next: [sug('REMINDER due — armed for 2026-10-01'), sug('REMINDER due — armed for 2026-10-02')],
			has_agent_activity: true,
			needs_onboarding: false,
			degraded: false,
			degraded_sections: []
		});
		app = mount(DashboardPage, { target: host, props: {} }) as Record<string, unknown>;
		flushSync();
		for (let i = 0; i < 6; i++) {
			await Promise.resolve();
			await tick();
		}
		flushSync();
		expect(host.querySelector('.skeleton-dashboard'), 'the dashboard is still on its skeleton').toBeNull();
		const cards = [...host.querySelectorAll('.suggested-card .sug-reason')].map((e) => e.textContent?.trim());
		expect(cards).toEqual(['REMINDER due — armed for 2026-10-01', 'REMINDER due — armed for 2026-10-02']);
	});
});
