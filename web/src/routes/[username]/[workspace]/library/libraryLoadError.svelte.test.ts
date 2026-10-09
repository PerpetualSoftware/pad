import { describe, expect, it, vi, afterEach, beforeEach } from 'vitest';
import { cleanup, render, screen } from '@testing-library/svelte';

// TASK-2203 (audit C46): a failed load rendered as "No conventions available."
// with no retry. The library always ships entries, so empty meant "did not
// load", and the page said the opposite.

vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get identityEpoch() { return 0; },
		get userId() { return 'u1'; },
		get user() { return { id: 'u1', name: 'A', email: 'a@example.com' }; },
		identityFence() { return () => true; },
		onIdentityChange() { return () => {}; },
		clear() {}
	}
}));

const answers = vi.hoisted(() => ({ next: [] as Array<'fail' | 'forbidden' | 'empty'> }));
vi.mock('$lib/api/client', () => ({
	api: {
		library: {
			get: vi.fn(async () => {
				const a = answers.next.shift() ?? 'empty';
				if (a === 'fail') throw new Error('Service unavailable');
				if (a === 'forbidden') throw Object.assign(new Error('Forbidden'), { code: 'forbidden' });
				return { categories: [] };
			}),
			getPlaybooks: vi.fn(async () => ({ categories: [] }))
		},
		items: { listByCollection: vi.fn(async () => []) },
		builtins: { list: vi.fn(async () => []) },
		collections: { list: vi.fn(async () => []) }
	}
}));
vi.mock('$lib/collections/canCreateIn', () => ({ canCreateIn: () => true }));
vi.mock('$lib/scroll/restore.svelte', () => ({
	createScrollRestoration: () => ({ snapshot: { capture: () => null, restore: () => {} } })
}));

import { page } from '$app/state';
import LibraryPage from './+page.svelte';

beforeEach(() => {
	answers.next = [];
	page.params = { username: 'dave', workspace: 'ws' };
});
afterEach(() => cleanup());

describe('Library: a failed load is not an empty library (TASK-2203)', () => {
	it('conventions: the error and a retry; the retry that finds nothing shows the empty line', async () => {
		answers.next = ['fail', 'empty'];
		page.url = new URL('http://localhost/dave/ws/library');
		render(LibraryPage);
		await screen.findByText("Couldn't load the convention library");
		expect(screen.queryByText('No conventions available.')).toBeNull();
		screen.getByRole('button', { name: 'Try again' }).click();
		await screen.findByText('No conventions available.');
		expect(screen.queryByText("Couldn't load the convention library")).toBeNull();
	});

	it('playbooks: the same, on its own tab', async () => {
		answers.next = ['fail'];
		page.url = new URL('http://localhost/dave/ws/library?tab=playbooks');
		render(LibraryPage);
		await screen.findByText("Couldn't load the playbook library");
		expect(screen.queryByText('No playbooks available.')).toBeNull();
	});

	it('a refusal says so and offers no retry', async () => {
		answers.next = ['forbidden'];
		page.url = new URL('http://localhost/dave/ws/library');
		render(LibraryPage);
		await screen.findByText("You don't have access to the convention library");
		expect(screen.queryByRole('button', { name: 'Try again' })).toBeNull();
	});
});
