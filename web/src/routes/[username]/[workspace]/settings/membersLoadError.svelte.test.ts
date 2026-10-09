import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render, screen } from '@testing-library/svelte';
import { page } from '$app/state';

// TASK-2203 (audit C46): the Members tab answered every failure of the members
// list, including the 403 a guest always gets, with "No members yet.".

const answers = vi.hoisted(() => ({ next: [] as Array<'fail' | 'forbidden' | 'empty'> }));

vi.mock('$lib/api/client', () => ({
	api: {
		workspaces: {
			get: vi.fn(async () => ({ id: 'ws1', slug: 'ws', name: 'WS', context: {} })),
			me: vi.fn(async () => ({ role: 'owner', collection_grants: [], item_grants: [] })),
			list: vi.fn(async () => [])
		},
		collections: { list: vi.fn(async () => []) },
		members: {
			list: vi.fn(async () => {
				const a = answers.next.shift() ?? 'empty';
				if (a === 'fail') throw new Error('Service unavailable');
				if (a === 'forbidden') throw Object.assign(new Error('Forbidden'), { code: 'forbidden' });
				return { members: [], invitations: [] };
			})
		}
	},
	isPlanLimitError: () => false,
	planLimitMessage: () => ''
}));
vi.mock('$lib/services/sse.svelte', () => ({ sseService: { onItemEvent: () => () => {} } }));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get userId() { return 'u-me'; },
		get identityEpoch() { return 0; },
		identityFence() { return () => true; },
		onIdentityChange() { return () => {}; },
		get user() { return { id: 'u-me', name: 'Me', email: 'me@example.com' }; }
	}
}));

const { default: SettingsPage } = await import('./+page.svelte');

beforeEach(() => {
	answers.next = [];
	page.params = { username: 'dave', workspace: 'ws' };
	window.location.hash = '#members';
});
afterEach(() => {
	cleanup();
	window.location.hash = '';
});

describe('Settings, Members: a failed load is not an empty list (TASK-2203)', () => {
	it('shows the error and a retry; the retry that finds nothing shows the empty state', async () => {
		answers.next = ['fail', 'empty'];
		render(SettingsPage);
		await screen.findByText("Couldn't load the members list");
		expect(screen.queryByText('No members yet.')).toBeNull();
		screen.getByRole('button', { name: 'Try again' }).click();
		await screen.findByText('No members yet.');
		expect(screen.queryByText("Couldn't load the members list")).toBeNull();
	});

	it('a guest (403) is told it may not see the list, not that there is nobody', async () => {
		answers.next = ['forbidden'];
		render(SettingsPage);
		await screen.findByText("You don't have access to the members list");
		expect(screen.queryByText('No members yet.')).toBeNull();
		expect(screen.queryByRole('button', { name: 'Try again' })).toBeNull();
	});
});
