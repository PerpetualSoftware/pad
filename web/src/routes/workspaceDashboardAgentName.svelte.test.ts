import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';
import { page } from '$app/state';
import type { Activity } from '$lib/types';

/**
 * TASK-2759, the workspace dashboard's Recent Activity rows.
 *
 * This is the first surface most people see, and it rendered a flat "agent"
 * badge for every agent write. Asserted through the PAGE (CONVE-19): the row
 * passes `activity.metadata` to the shared helper, and passing the wrong
 * argument — or not calling it — is invisible to the helper's own tests.
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

let seq = 0;
function act(over: Partial<Activity> = {}): Activity {
	seq += 1;
	return {
		id: `a${seq}`,
		workspace_id: 'ws',
		action: 'updated',
		actor: 'agent',
		source: 'cli',
		metadata: JSON.stringify({ agent: 'wren' }),
		created_at: new Date().toISOString(),
		item_ref: 'TASK-1',
		item_title: 'A thing',
		item_slug: 'a-thing',
		collection_slug: 'tasks',
		...over
	} as Activity;
}

/**
 * A complete DashboardResponse with only `recent_activity` populated.
 *
 * Every array is present because the page reads `.length` on them
 * unconditionally in its section guards — a payload missing one throws
 * during render and leaves the whole page blank, which reads exactly like
 * "the badge did not render".
 */
function dashboard(recent: Activity[]) {
	return {
		summary: { total_items: recent.length, by_collection: {} },
		active_items: [],
		starred_items: [],
		active_plans: [],
		attention: [],
		recent_activity: recent,
		suggested_next: [],
		has_agent_activity: true,
		needs_onboarding: false,
		degraded: false,
		degraded_sections: []
	};
}

let host: HTMLElement;
let app: Record<string, unknown> | null = null;

async function mountWith(recent: Activity[]): Promise<void> {
	dashboardGet.mockResolvedValue(dashboard(recent));
	app = mount(DashboardPage, { target: host, props: {} }) as Record<string, unknown>;
	flushSync();
	for (let i = 0; i < 6; i++) {
		await Promise.resolve();
		await tick();
	}
	flushSync();
}

/** Actor badges in the Recent Activity list, in render order. */
function agentBadges(): string[] {
	return [...host.querySelectorAll('.activity-row .actor-badge.agent')].map(
		(el) => el.textContent?.trim() ?? ''
	);
}

beforeEach(() => {
	host = document.createElement('div');
	document.body.appendChild(host);
	page.params.workspace = 'ws';
	page.params.username = 'alice';
	dashboardGet.mockReset();
});

afterEach(() => {
	if (app) unmount(app as never);
	app = null;
	host.remove();
});

describe('workspace dashboard — Recent Activity agent badge', () => {
	it('badges each agent row with its stamped name', async () => {
		await mountWith([act(), act({ metadata: JSON.stringify({ agent: 'rook' }) })]);

		expect(agentBadges()).toEqual(['wren', 'rook']);
	});

	it('badges a generic-looking client id verbatim', async () => {
		await mountWith([act({ metadata: JSON.stringify({ agent: 'claude-code' }) })]);

		expect(agentBadges()).toEqual(['claude-code']);
	});

	it.each([
		['no agent key', JSON.stringify({ changes: 'status' })],
		['an empty name', JSON.stringify({ agent: '' })],
		['unparseable metadata', 'not json']
	])('falls back to the generic badge given %s', async (_case, metadata) => {
		await mountWith([act({ metadata })]);

		// The counterfactual for the whole change: this is what EVERY agent row
		// showed before it, so a badge stuck on this string is the failure mode.
		expect(agentBadges()).toEqual(['agent']);
	});

	it('leaves a human row on its own name and never reads the stamp', async () => {
		await mountWith([act({ actor: 'user', actor_name: 'Dave', source: 'web' })]);

		expect(agentBadges()).toEqual([]);
		expect(host.textContent).toContain('Dave');
		expect(host.textContent).not.toContain('wren');
	});
});
