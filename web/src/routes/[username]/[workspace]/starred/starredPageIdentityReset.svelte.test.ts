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

	it('refuses a load issued as A that settles after the identity changed', async () => {
		// The page's identity LISTENER is gone (the tab reloads instead), so
		// what it still owns is the pre-reload window: a request issued as A can
		// settle before the reload takes the page away, and `loadSeq` is a
		// navigation fence that an account swap does not move.
		let resolve!: (v: unknown) => void;
		api.items.starred.mockReturnValueOnce(new Promise((r) => { resolve = r; }));
		api.collections.list.mockResolvedValue([]);

		const screen = render(StarredPage);
		const count = () => screen.container.querySelector('.count')?.textContent ?? '';
		await settle();
		// PRECONDITION: nothing rendered yet, so a later '0' is the fence
		// refusing rather than a page that never had data.
		expect(count()).toBe('0');

		api.auth.session.mockResolvedValue(sessionFor('user-b'));
		await authStore.load();
		resolve([ITEM_A]);
		await settle();

		expect(count()).toBe('0');
	});
});
