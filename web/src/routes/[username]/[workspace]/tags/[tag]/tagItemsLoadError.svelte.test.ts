import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render, screen } from '@testing-library/svelte';

// TASK-2203 (audit C46): a failed load is an error with a retry, never
// "No items tagged …". Since TASK-2231 the page reads the local index and
// the layout's collection list instead of fetching, so the failures it must
// own are those: an index that could not load, a revoked caller (whose index
// is reset rather than failed), and a collection list that could not load.

const fence = vi.hoisted(() => ({ ok: true }));

vi.mock('$app/state', async () => ({ page: (await import('../../../../../test/mocks/reactivePage.svelte')).page }));
vi.mock('$app/env', () => ({ browser: true }));
// The page makes no request of its own; a call here is the regression.
vi.mock('$lib/api/client', () => ({ api: new Proxy({}, { get: () => { throw new Error('the tag page must not call the API'); } }) }));
vi.mock('$lib/stores/localIndex.svelte', async () => ({
	localIndex: (await import('../../../../../test/mocks/indexedPageStores.svelte')).localIndex
}));
vi.mock('$lib/stores/collections.svelte', async () => ({
	collectionStore: (await import('../../../../../test/mocks/indexedPageStores.svelte')).collectionStore
}));
vi.mock('$lib/stores/workspaceIndexEntry', async () => ({
	enterWorkspaceIndex: (await import('../../../../../test/mocks/indexedPageStores.svelte')).enterWorkspaceIndex
}));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get identityEpoch() { return 0; },
		get userId() { return 'u1'; },
		identityFence: () => () => fence.ok,
		onIdentityChange: () => () => {}
	}
}));
vi.mock('$lib/stores/workspace.svelte', () => ({ workspaceStore: { get current() { return { name: 'WS' }; } } }));
vi.mock('$lib/scroll/restore.svelte', () => ({
	createScrollRestoration: () => ({ snapshot: { capture: () => null, restore: () => {} } })
}));

import { page } from '$app/state';
import { fake, resetFake } from '../../../../../test/mocks/indexedPageStores.svelte';
import TagPage from './+page.svelte';

beforeEach(() => {
	resetFake();
	fence.ok = true;
	localStorage.clear();
	page.params = { username: 'dave', workspace: 'ws', tag: 'release' };
	page.url = new URL('http://localhost/dave/ws/tags/release');
});
afterEach(() => cleanup());

describe('Tag page: a failed load is not an empty tag (TASK-2203)', () => {
	it('an index that could not load shows the error and a retry; the retry that finds nothing shows the empty state', async () => {
		fake.indexState = 'error';
		render(TagPage);
		await screen.findByText("Couldn't load the items with this tag");
		expect(screen.queryByText(/No items tagged/)).toBeNull();
		fake.indexOnEntry = 'ready';
		screen.getByRole('button', { name: 'Try again' }).click();
		await screen.findByText(/No items tagged/);
		expect(screen.queryByText("Couldn't load the items with this tag")).toBeNull();
	});

	it('a collection list that could not load shows the error, and the retry asks again', async () => {
		fake.collectionsFresh = false;
		fake.collectionFailures = 1;
		render(TagPage);
		await screen.findByText("Couldn't load the items with this tag");
		screen.getByRole('button', { name: 'Try again' }).click();
		await screen.findByText(/No items tagged/);
		expect(fake.ensures).toBe(2);
	});

	it('a revoked caller is told so and offered no retry', async () => {
		fake.indexState = 'cold';
		fake.revoked = true;
		render(TagPage);
		await screen.findByText("You don't have access to the items with this tag");
		expect(screen.queryByRole('button', { name: 'Try again' })).toBeNull();
	});

	it('a failure that lands after an account swap shows nothing to the new account (codex r1)', async () => {
		fake.collectionsFresh = false;
		fake.collectionFailures = 1;
		fence.ok = false;
		render(TagPage);
		await new Promise((r) => setTimeout(r, 50));
		// PRECONDITION: the failing request was made, so the absence is the fence's.
		expect(fake.ensures).toBe(1);
		expect(screen.queryByText("Couldn't load the items with this tag")).toBeNull();
	});
});
