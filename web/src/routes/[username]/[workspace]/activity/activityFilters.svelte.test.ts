import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/svelte';

// TASK-2219: every activity filter reaches the server, and Load more stays
// under a collection filter (it was hidden while the filter ran client-side).

const calls = vi.hoisted(() => ({ params: [] as Array<Record<string, unknown>> }));
const row = (i: number) => ({
	id: 'a' + i, action: 'updated', actor: 'user', source: 'cli', item_id: 'i1', item_title: 'Row ' + i, item_ref: 'TASK-1',
	collection_slug: 'tasks', metadata: '{}', actor_name: 'Dana',
	created_at: new Date(Date.UTC(2026, 9, 9, 0, 0, 60 - i)).toISOString(),
});

vi.mock('$lib/api/client', () => ({
	api: {
		activity: {
			list: vi.fn(async (_slug: string, params: Record<string, unknown>) => {
				calls.params.push(params);
				return Array.from({ length: 50 }, (_, i) => row(i));
			}),
		},
		collections: {
			list: vi.fn(async () => [{ id: 'c1', slug: 'tasks', name: 'Tasks', icon: '✓', schema: '{}', settings: '{}' }]),
		},
	},
}));
vi.mock('$lib/stores/workspace.svelte', () => ({ workspaceStore: { current: null, setCurrent: vi.fn(async () => {}) } }));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get identityEpoch() { return 0; },
		get userId() { return 'u1'; },
		identityFence: () => () => true,
		onIdentityChange: () => () => {},
	},
}));
vi.mock('$app/state', async () => ({ page: (await import('../../../../test/mocks/reactivePage.svelte')).page }));
// goto moves the reactive page.url, as a real navigation does.
const navs = vi.hoisted(() => [] as Array<{ url: string; opts: Record<string, unknown> }>);
const navMode = vi.hoisted(() => ({ reject: false, shallow: [] as string[] }));
vi.mock('$app/navigation', async () => {
	const { page: p } = await import('../../../../test/mocks/reactivePage.svelte');
	return {
		goto: vi.fn(async (url: string | URL, opts: Record<string, unknown> = {}) => {
			navs.push({ url: String(url), opts });
			if (navMode.reject) throw new Error('navigation failed');
			p.url = new URL(String(url));
		}),
		replaceState: vi.fn((url: string | URL) => {
			navMode.shallow.push(String(url));
		}),
	};
});
vi.mock('$lib/stores/workspaceIndexEntry', () => ({ enterWorkspaceIndex: vi.fn(async () => {}) }));
vi.mock('$lib/services/sse.svelte', () => ({ sseService: { onItemEvent: () => () => {} } }));
vi.mock('$lib/scroll/restore.svelte', () => ({
	createScrollRestoration: () => ({ snapshot: { capture: () => null, restore: () => {} } }),
}));

import { page } from '$app/state';
import ActivityPage from './+page.svelte';

const last = () => calls.params[calls.params.length - 1];
const settle = () => new Promise((r) => setTimeout(r, 20));

beforeEach(() => {
	calls.params = [];
	navs.length = 0;
	navMode.reject = false;
	navMode.shallow = [];
	try { localStorage.setItem('pad-activity-view', 'audit'); } catch {}
	page.params = { username: 'dave', workspace: 'ws' };
	page.url = new URL('http://localhost/dave/ws/activity');
});
afterEach(() => cleanup());

describe('Activity filters reach the server (TASK-2219)', () => {
	it('the collection filter is a request parameter, and Load more stays and carries it', async () => {
		render(ActivityPage);
		await screen.findByRole('button', { name: 'Load more activity' });
		await screen.findByRole('option', { name: /Tasks/ });

		await fireEvent.change(screen.getByLabelText('Collection'), { target: { value: 'tasks' } });
		await settle();
		expect(last()).toMatchObject({ collection: 'tasks' });

		const more = await screen.findByRole('button', { name: 'Load more activity' });
		await fireEvent.click(more);
		await settle();
		expect(last()).toMatchObject({ collection: 'tasks', before_id: 'a49' });
	});

	it('the actor filter sends the server value', async () => {
		render(ActivityPage);
		await screen.findByRole('button', { name: 'Load more activity' });
		await fireEvent.change(screen.getByLabelText('Actor'), { target: { value: 'agent' } });
		await settle();
		expect(last()).toMatchObject({ actor: 'agent' });
		await fireEvent.change(screen.getByLabelText('Actor'), { target: { value: '' } });
		await settle();
		expect(last()).not.toHaveProperty('actor');
	});

	it('the badge says web or CLI in words', async () => {
		render(ActivityPage);
		await screen.findByRole('button', { name: 'Load more activity' });
		expect(screen.getAllByText('Dana, via CLI').length).toBeGreaterThan(0);
	});
});

describe("the catch-up card's See everything link (TASK-3615)", () => {
	it('?since= reaches the request, shows as a chip, and the chip clears it', async () => {
		page.url = new URL('http://localhost/dave/ws/activity?since=2026-10-17T18%3A40%3A00Z&view=audit');
		render(ActivityPage);
		await screen.findByRole('button', { name: 'Load more activity' });
		await settle();
		expect(last()).toMatchObject({ since: '2026-10-17T18:40:00Z' });
		expect(screen.getByTestId('activity-since-chip')).toBeTruthy();

		await fireEvent.click(screen.getByRole('button', { name: 'Remove the since filter' }));
		await settle();
		expect(last()).not.toHaveProperty('since');
		expect(screen.queryByTestId('activity-since-chip')).toBeNull();
	});

	it('a ?since= the server would refuse is dropped, so the feed loads unfiltered', async () => {
		page.url = new URL('http://localhost/dave/ws/activity?since=yesterday');
		render(ActivityPage);
		await screen.findByRole('button', { name: 'Load more activity' });
		expect(last()).not.toHaveProperty('since');
		expect(screen.queryByTestId('activity-since-chip')).toBeNull();
	});

	it('?view=audit opens Audit without overwriting the stored choice; a click is still persisted', async () => {
		localStorage.setItem('pad-activity-view', 'live');
		page.url = new URL('http://localhost/dave/ws/activity?view=audit');
		render(ActivityPage);
		await screen.findByRole('button', { name: 'Load more activity' });
		await settle();
		expect(screen.getByRole('button', { name: 'Audit' }).classList.contains('selected')).toBe(true);
		expect(localStorage.getItem('pad-activity-view')).toBe('live');

		await fireEvent.click(screen.getByRole('button', { name: 'Audit' }));
		await settle();
		expect(localStorage.getItem('pad-activity-view')).toBe('audit');
	});

	for (const how of ['the chip ×', 'Clear filters'] as const) {
		it(`${how} drops since from the URL: a reload stays cleared, and the same link re-applies (PR B, lead sweep)`, async () => {
			const link = 'http://localhost/dave/ws/activity?since=2026-10-17T18%3A40%3A00Z&view=audit';
			page.url = new URL(link);
			render(ActivityPage);
			await screen.findByRole('button', { name: 'Load more activity' });
			await settle();
			// Control: the link applied.
			expect(last()).toMatchObject({ since: '2026-10-17T18:40:00Z' });

			if (how === 'the chip ×') await fireEvent.click(screen.getByRole('button', { name: 'Remove the since filter' }));
			else await fireEvent.click(screen.getByRole('button', { name: 'Clear filters' }));
			await settle();
			expect(navs).toHaveLength(1);
			expect(navs[0].opts).toMatchObject({ replaceState: true });
			expect(new URL(navs[0].url).searchParams.has('since')).toBe(false);
			expect(new URL(navs[0].url).searchParams.get('view')).toBe('audit');
			expect(last()).not.toHaveProperty('since');

			// The same link followed again re-applies the filter.
			page.url = new URL(link);
			await settle();
			expect(last()).toMatchObject({ since: '2026-10-17T18:40:00Z' });
			expect(screen.getByTestId('activity-since-chip')).toBeTruthy();

			// Clear again, then reload: the cleared URL loads with no chip and no since.
			await fireEvent.click(screen.getByRole('button', { name: 'Remove the since filter' }));
			await settle();
			const cleared = page.url.href;
			cleanup();
			calls.params = [];
			page.url = new URL(cleared);
			render(ActivityPage);
			await screen.findByRole('button', { name: 'Load more activity' });
			await settle();
			expect(calls.params.length).toBeGreaterThan(0);
			expect(last()).not.toHaveProperty('since');
			expect(screen.queryByTestId('activity-since-chip')).toBeNull();
		});
	}

	it('a clear whose navigation rejects still drops since from the address bar (PR B r3)', async () => {
		page.url = new URL('http://localhost/dave/ws/activity?since=2026-10-17T18%3A40%3A00Z&view=audit');
		render(ActivityPage);
		await screen.findByRole('button', { name: 'Load more activity' });
		await settle();
		navMode.reject = true;
		await fireEvent.click(screen.getByRole('button', { name: 'Remove the since filter' }));
		await settle();
		expect(navs).toHaveLength(1);
		expect(navMode.shallow).toHaveLength(1);
		expect(new URL(navMode.shallow[0]).searchParams.has('since')).toBe(false);
		expect(new URL(navMode.shallow[0]).searchParams.get('view')).toBe('audit');
		expect(last()).not.toHaveProperty('since');
	});

	it('choosing a view drops view from the URL: a reload keeps the choice, and the same link re-applies (PR B r4)', async () => {
		localStorage.setItem('pad-activity-view', 'live');
		const link = 'http://localhost/dave/ws/activity?since=2026-10-17T18%3A40%3A00Z&view=audit';
		page.url = new URL(link);
		render(ActivityPage);
		await screen.findByRole('button', { name: 'Load more activity' });
		await settle();
		// Control: the link's view applied.
		expect(screen.getByRole('button', { name: 'Audit' }).classList.contains('selected')).toBe(true);

		await fireEvent.click(screen.getByRole('button', { name: 'Live' }));
		await settle();
		expect(navs).toHaveLength(1);
		expect(navs[0].opts).toMatchObject({ replaceState: true });
		expect(new URL(navs[0].url).searchParams.has('view')).toBe(false);
		expect(new URL(navs[0].url).searchParams.get('since')).toBe('2026-10-17T18:40:00Z');
		expect(localStorage.getItem('pad-activity-view')).toBe('live');

		// Reload: the choice holds, because the URL no longer overrides it.
		const chosen = page.url.href;
		cleanup();
		page.url = new URL(chosen);
		render(ActivityPage);
		await screen.findByRole('button', { name: 'Load more activity' });
		await settle();
		expect(screen.getByRole('button', { name: 'Live' }).classList.contains('selected')).toBe(true);

		// The same link followed again re-applies its view.
		page.url = new URL(link);
		await settle();
		expect(screen.getByRole('button', { name: 'Audit' }).classList.contains('selected')).toBe(true);
	});

	it('a See everything followed on an already-mounted page applies its view too, unpersisted (PR B r1)', async () => {
		localStorage.setItem('pad-activity-view', 'live');
		render(ActivityPage);
		await screen.findByRole('button', { name: 'Load more activity' });
		await settle();
		// Control: mounted in the stored Live view.
		expect(screen.getByRole('button', { name: 'Audit' }).classList.contains('selected')).toBe(false);

		page.url = new URL('http://localhost/dave/ws/activity?since=2026-10-17T18%3A40%3A00Z&view=audit');
		await settle();
		expect(last()).toMatchObject({ since: '2026-10-17T18:40:00Z' });
		expect(screen.getByRole('button', { name: 'Audit' }).classList.contains('selected')).toBe(true);
		expect(localStorage.getItem('pad-activity-view')).toBe('live');
	});
});
