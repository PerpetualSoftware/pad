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
		// NO vi.resetModules() here (codex round 5): this file imports the page
		// component and the auth store at top level, and resetting the registry
		// mid-file hands the test a different authStore instance than the one
		// the component closed over — so the identity change fires into a store
		// nothing under test is listening to.
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
		//
		// The page's FIRST load is the one left pending, because it is the only
		// one this harness can hold open reliably — an attempt to start a second
		// through the terminal-filter toggle left the fence undetectable by its
		// own mutant, which is the failure this file has produced twice now.
		// EVERY mock also carries a DEFAULT beside the pending one-shot (codex
		// round 5): without that, a later call resolved `undefined`, `items.map`
		// threw, and vitest reported PASSED with an unhandled error beside it.
		let resolveA!: (v: unknown) => void;
		api.items.starred.mockReturnValueOnce(new Promise((r) => { resolveA = r; }));
		api.items.starred.mockResolvedValue([]);
		api.collections.list.mockResolvedValue([]);

		const screen = render(StarredPage);
		const count = () => screen.container.querySelector('.count')?.textContent ?? '';
		await settle();
		// PRECONDITION: A's request is out and unanswered, so what follows is
		// about the settle rather than about a page that never asked.
		expect(api.items.starred).toHaveBeenCalled();
		expect(count()).toBe('0');

		api.auth.session.mockResolvedValue(sessionFor('user-b'));
		await authStore.load();
		resolveA([ITEM_A]);
		await settle();

		expect(count()).toBe('0');
		expect(screen.queryByText("Alpha's secret item")).toBeNull();

		// WHAT THIS LEG DOES NOT PROVE, measured rather than assumed: removing
		// the page's identity fence (`|| !isSameIdentity()`) leaves it GREEN.
		// The mutant survives because the page's own `$effect` re-runs during
		// the identity transition and advances `loadSeq`, so the NAVIGATION
		// fence refuses A's settle first and the identity fence never decides
		// anything here. The leg is an end-state regression fence — A's items
		// must not appear — and the identity fence's coverage of the pre-reload
		// window is an argument, not a measurement. Said plainly because a
		// surviving mutant recorded as a passing test is how this file already
		// shipped one assertion pointed the wrong way.
	});

	it("renders A's items when the identity holds still", async () => {
		// The counterfactual the test above needs to mean anything: the same
		// harness, no identity change, and the data DOES render. Without it,
		// `count === '0'` is equally consistent with a page that never works.
		api.items.starred.mockResolvedValue([ITEM_A]);
		api.collections.list.mockResolvedValue([]);

		const screen = render(StarredPage);
		const count = () => screen.container.querySelector('.count')?.textContent ?? '';
		await settle();

		expect(count()).toBe('1');
	});
});
