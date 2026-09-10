// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// BUG-3005, codex round 1 — the starred PAGE keeps its own copy of the
// previous user's items, and the store fix alone does not reach it.
//
// Two things make this page worse than an ordinary stale cache, and the second
// is caused by the store fix itself:
//
//   - its load effect is keyed on the workspace slug and the terminal filter,
//     so a same-route account swap starts no reload at all;
//   - `items` falls back to UNFILTERED `fetchedItems` whenever
//     `starredStore.loaded` is false — which is exactly what the store's
//     identity reset sets it to. So the store's fix routes this page around
//     its only filter and B sees A's starred items in full.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick } from 'svelte';

const api = vi.hoisted(() => ({
	auth: { session: vi.fn() },
	items: { starred: vi.fn(), star: vi.fn(), unstar: vi.fn() },
	collections: { list: vi.fn() },
	workspaces: { get: vi.fn(), me: vi.fn(), list: vi.fn() },
}));

vi.mock('$lib/api/client', () => ({
	api,
	setAccessRevokedHandler: () => {},
	setRateLimitHandler: () => {},
	isPlanLimitError: () => false,
	planLimitMessage: () => '',
}));

vi.mock('$app/navigation', () => ({
	goto: vi.fn(),
	beforeNavigate: () => {},
	afterNavigate: () => {},
	invalidateAll: vi.fn(),
	pushState: vi.fn(),
	replaceState: vi.fn(),
}));

import StarredPage from './+page.svelte';
import { authStore } from '$lib/stores/auth.svelte';
import { starredStore } from '$lib/stores/starred.svelte';
import { page } from '$app/state';

function sessionFor(id: string) {
	return { authenticated: true, user: { id, email: `${id}@example.com` } };
}

const ITEM_A = {
	id: 'item-a',
	slug: 'alphas-secret',
	title: "Alpha's secret item",
	collection_slug: 'ideas',
	status: 'open',
	fields: {},
};

async function settle(): Promise<void> {
	for (let i = 0; i < 10; i++) {
		await Promise.resolve();
		await tick();
	}
}

describe('starred page across an identity change', () => {
	beforeEach(async () => {
		vi.resetModules();
		api.items.starred.mockReset();
		api.collections.list.mockReset();
		api.auth.session.mockReset();
		page.params.workspace = 'ws';
		page.params.username = 'alice';
		// Establish an identity so later transitions actually fire — the first
		// resolution is the baseline and notifies nobody.
		api.auth.session.mockResolvedValue(sessionFor('user-a'));
		await authStore.load();
	});

	afterEach(() => {
		cleanup();
		authStore.clear();
	});

	it("does not render A's starred items after B signs in on the same route", async () => {
		api.items.starred.mockResolvedValue([ITEM_A]);
		api.collections.list.mockResolvedValue([
			{ id: 'c1', slug: 'ideas', name: 'Ideas', is_default: true, sort_order: 1 },
		]);

		const screen = render(StarredPage);
		const count = () => screen.container.querySelector('.count')?.textContent ?? '';
		await settle();

		// PRECONDITION: the page is showing one starred item — A's. Asserted
		// through the header COUNT rather than the card's title text: the count
		// is derived from the same `items` array the cards are, and ItemCard
		// needs workspace/collection context this harness does not stand up, so
		// asserting on its rendered title would test the card rather than the
		// leak. Without this precondition the assertion below passes against a
		// page that rendered nothing at all.
		expect(count()).toBe('1');

		// B signs in with no navigation — the route, and therefore the page's
		// load effect key, never changes. B's own starred list is empty.
		api.items.starred.mockResolvedValue([]);
		api.collections.list.mockResolvedValue([]);
		api.auth.session.mockResolvedValue(sessionFor('user-b'));
		await authStore.load();
		await settle();

		expect(count()).toBe('0');
	});

	it('reloads the shared starredStore, not just its own copy', async () => {
		// codex round 2. The page renders correctly through its own fallback
		// either way, so a page-local reload HIDES the store's emptiness rather
		// than fixing it — and every other view in the workspace consults
		// `starredStore.isStarred()`, so they show every item as unstarred until
		// something else happens to reload it.
		api.items.starred.mockResolvedValue([ITEM_A]);
		api.collections.list.mockResolvedValue([]);

		// The workspace LAYOUT is what calls `starredStore.load` on mount, and
		// this harness renders the page alone — so seed the store the way the
		// layout would, or the precondition below is asserting about a store
		// nothing ever populated.
		await starredStore.load('ws');
		render(StarredPage);
		await settle();
		// PRECONDITION: the store holds A's star, so "B's is loaded" is a claim
		// about a store that was populated and changed rather than one that was
		// always in this state.
		expect(starredStore.isStarred('item-a')).toBe(true);

		api.items.starred.mockResolvedValue([{ ...ITEM_A, id: 'item-b' }]);
		api.auth.session.mockResolvedValue(sessionFor('user-b'));
		await authStore.load();
		await settle();

		expect(starredStore.loaded).toBe(true);
		expect(starredStore.isStarred('item-a')).toBe(false);
		expect(starredStore.isStarred('item-b')).toBe(true);
	});
});
