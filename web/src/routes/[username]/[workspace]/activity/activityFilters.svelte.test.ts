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
