import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/svelte';

// TASK-2203 (audit C46): a failed load is an error with a retry, never
// "No items tagged …".

const answers = vi.hoisted(() => ({ next: [] as Array<'fail' | 'forbidden' | 'empty' | 'fail-after-swap'> }));
const fence = vi.hoisted(() => ({ ok: true }));

vi.mock('$app/state', async () => ({ page: (await import('../../../../../test/mocks/reactivePage.svelte')).page }));
vi.mock('$app/environment', () => ({ browser: true }));
vi.mock('$lib/api/client', () => ({
	api: {
		items: {
			list: vi.fn(async () => {
				const a = answers.next.shift() ?? 'empty';
				if (a === 'fail-after-swap') { fence.ok = false; throw new Error('Service unavailable'); }
				if (a === 'fail') throw new Error('Service unavailable');
				if (a === 'forbidden') throw Object.assign(new Error('Forbidden'), { code: 'forbidden' });
				return [];
			})
		},
		collections: { list: vi.fn(async () => []) }
	}
}));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: { get identityEpoch() { return 0; }, identityFence: () => () => fence.ok, onIdentityChange: () => () => {} }
}));
vi.mock('$lib/stores/workspace.svelte', () => ({ workspaceStore: { get current() { return { name: 'WS' }; } } }));
vi.mock('$lib/scroll/restore.svelte', () => ({
	createScrollRestoration: () => ({ snapshot: { capture: () => null, restore: () => {} } })
}));

import { page } from '$app/state';
import TagPage from './+page.svelte';

beforeEach(() => {
	answers.next = [];
	fence.ok = true;
	localStorage.clear();
	page.params = { username: 'dave', workspace: 'ws', tag: 'release' };
	page.url = new URL('http://localhost/dave/ws/tags/release');
});
afterEach(() => cleanup());

describe('Tag page: a failed load is not an empty tag (TASK-2203)', () => {
	it('shows the error and a retry; the retry that finds nothing shows the empty state', async () => {
		answers.next = ['fail', 'empty'];
		render(TagPage);
		await screen.findByText("Couldn't load the items with this tag");
		expect(screen.queryByText(/No items tagged/)).toBeNull();
		screen.getByRole('button', { name: 'Try again' }).click();
		await screen.findByText(/No items tagged/);
		expect(screen.queryByText("Couldn't load the items with this tag")).toBeNull();
	});

	it('a refusal says so and offers no retry', async () => {
		answers.next = ['forbidden'];
		render(TagPage);
		await screen.findByText("You don't have access to the items with this tag");
		expect(screen.queryByRole('button', { name: 'Try again' })).toBeNull();
	});

	it('a failure that lands after an account swap shows nothing to the new account (codex r1)', async () => {
		answers.next = ['fail-after-swap'];
		render(TagPage);
		await new Promise((r) => setTimeout(r, 50));
		expect(screen.queryByText("Couldn't load the items with this tag")).toBeNull();
	});
});
