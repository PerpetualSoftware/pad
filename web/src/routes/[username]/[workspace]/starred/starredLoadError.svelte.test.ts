import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render, screen } from '@testing-library/svelte';

// TASK-2203 (audit C46): a failed load is an error with a retry, never
// "No starred items".

const answers = vi.hoisted(() => ({ next: [] as Array<'fail' | 'forbidden' | 'empty'> }));

vi.mock('$lib/api/client', () => ({
	api: {
		items: {
			starred: vi.fn(async () => {
				const a = answers.next.shift() ?? 'empty';
				if (a === 'fail') throw new Error('Service unavailable');
				if (a === 'forbidden') throw Object.assign(new Error('Forbidden'), { code: 'forbidden' });
				return [];
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
vi.mock('$lib/scroll/restore.svelte', () => ({
	createScrollRestoration: () => ({ snapshot: { capture: () => null, restore: () => {} } }),
}));

import { page } from '$app/state';
import StarredPage from './+page.svelte';

beforeEach(() => {
	answers.next = [];
	page.params = { username: 'dave', workspace: 'ws' };
	page.url = new URL('http://localhost/dave/ws/starred');
});
afterEach(() => cleanup());

describe('Starred: a failed load is not an empty list (TASK-2203)', () => {
	it('shows the error and a retry; the retry that finds nothing shows the empty state', async () => {
		answers.next = ['fail', 'empty'];
		render(StarredPage);
		await screen.findByText("Couldn't load your starred items");
		expect(screen.queryByText('No starred items')).toBeNull();
		screen.getByRole('button', { name: 'Try again' }).click();
		await screen.findByText('No starred items');
		expect(screen.queryByText("Couldn't load your starred items")).toBeNull();
	});

	it('a refusal says so and offers no retry', async () => {
		answers.next = ['forbidden'];
		render(StarredPage);
		await screen.findByText("You don't have access to your starred items");
		expect(screen.queryByRole('button', { name: 'Try again' })).toBeNull();
	});
});
