import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render, screen } from '@testing-library/svelte';

// TASK-2203 (audit C46): a failed first page is an error with a retry, never
// "No activity found"; a failed later page keeps what is shown and says so.

const answers = vi.hoisted(() => ({ next: [] as Array<'fail' | 'forbidden' | 'empty' | 'full'> }));
const row = (i: number) => ({
	id: 'a' + i, action: 'updated', actor: 'user', source: 'web', item_id: 'i1', item_title: 'Row ' + i, item_ref: 'TASK-1',
	collection_slug: 'tasks', metadata: '{}', created_at: new Date(Date.UTC(2026, 9, 9, 0, 0, 60 - i)).toISOString(),
});

vi.mock('$lib/api/client', () => ({
	api: {
		activity: {
			list: vi.fn(async () => {
				const a = answers.next.shift() ?? 'empty';
				if (a === 'fail') throw new Error('Service unavailable');
				if (a === 'forbidden') throw Object.assign(new Error('Forbidden'), { code: 'forbidden' });
				return a === 'full' ? Array.from({ length: 50 }, (_, i) => row(i)) : [];
			}),
		},
		collections: { list: vi.fn(async () => []) },
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
vi.mock('$lib/stores/workspaceIndexEntry', () => ({ enterWorkspaceIndex: vi.fn(async () => {}) }));
vi.mock('$lib/services/sse.svelte', () => ({ sseService: { onItemEvent: () => () => {} } }));
vi.mock('$lib/scroll/restore.svelte', () => ({
	createScrollRestoration: () => ({ snapshot: { capture: () => null, restore: () => {} } }),
}));

import { page } from '$app/state';
import ActivityPage from './+page.svelte';

beforeEach(() => {
	answers.next = [];
	try { localStorage.setItem('pad-activity-view', 'audit'); } catch {}
	page.params = { username: 'dave', workspace: 'ws' };
	page.url = new URL('http://localhost/dave/ws/activity');
});
afterEach(() => cleanup());

describe('Activity: a failed load is not an empty feed (TASK-2203)', () => {
	it('shows the error and a retry; the retry that finds nothing shows the empty state', async () => {
		answers.next = ['fail', 'empty'];
		render(ActivityPage);
		await screen.findByText("Couldn't load the activity feed");
		expect(screen.queryByText('No activity found')).toBeNull();
		screen.getByRole('button', { name: 'Try again' }).click();
		await screen.findByText('No activity found');
		expect(screen.queryByText("Couldn't load the activity feed")).toBeNull();
	});

	it('a refusal says so and offers no retry', async () => {
		answers.next = ['forbidden'];
		render(ActivityPage);
		await screen.findByText("You don't have access to the activity feed");
		expect(screen.queryByRole('button', { name: 'Try again' })).toBeNull();
	});

	it('a failed later page keeps the rows shown and says so on the button', async () => {
		answers.next = ['full', 'fail'];
		render(ActivityPage);
		const more = await screen.findByRole('button', { name: 'Load more activity' });
		more.click();
		await screen.findByRole('button', { name: "Couldn't load more. Try again" });
		expect(screen.getAllByText('Row 0').length).toBeGreaterThan(0);
		expect(screen.queryByText("Couldn't load the activity feed")).toBeNull();
	});
});
