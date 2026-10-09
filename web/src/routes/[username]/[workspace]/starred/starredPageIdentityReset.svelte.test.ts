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
//
// TASK-2231 removed the page's copy: it lists the STARRED STORE's ids (fenced
// and cleared on an identity change) against the local index's rows. These
// legs now pin the same end state through those sources — B never sees A's
// starred items — with the store's real fence and the real auth store, and
// the counterfactual that the same harness does render them for A.
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
	// BUG-2983 added this seam; the layout calls it at module scope, so a mock
	// without it throws before any assertion runs.
	setIdentityProvider: () => {},
	setRateLimitHandler: () => {},
	isPlanLimitError: () => false,
	planLimitMessage: () => '',
}));

const ITEM_ROW = vi.hoisted(() => ({
	id: 'item-a',
	slug: 'alphas-secret',
	title: "Alpha's secret item",
	collection_id: 'c-ideas',
	collection_slug: 'ideas',
	fields: '{"status":"open"}',
	tags: '[]',
	pinned: false,
	updated_at: '2026-01-01T00:00:00Z',
	created_at: '2026-01-01T00:00:00Z',
}));
vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: { bootstrapStateFor: () => 'ready', accessRevokedFor: () => false, getAll: () => [ITEM_ROW] },
}));
vi.mock('$lib/stores/workspaceIndexEntry', () => ({ enterWorkspaceIndex: async () => true }));
vi.mock('$lib/stores/collections.svelte', () => ({
	collectionStore: {
		collectionsAreFreshFor: () => true,
		collections: [{ id: 'c-ideas', slug: 'ideas', name: 'Ideas', icon: '💡', sort_order: 1, schema: '{"fields":[]}', settings: '{}' }],
		ensureCollections: async () => {},
	},
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
		starredStore.clear();
	});

	it('refuses a load issued as A that settles after the identity changed', async () => {
		// A's starred load is out when the account changes. The STORE's identity
		// fence refuses the settle and its listener clears the ids, so the page,
		// which derives from them, has nothing of A's to show. Every mock carries
		// a default beside the pending one-shot (codex round 5 on the earlier
		// version of this file).
		let resolveA!: (v: unknown) => void;
		api.items.starred.mockReturnValueOnce(new Promise((r) => { resolveA = r; }));
		api.items.starred.mockResolvedValue([]);

		void starredStore.load('ws');
		const screen = render(StarredPage);
		const count = () => screen.container.querySelector('.count')?.textContent ?? '';
		await settle();
		// PRECONDITION: A's request is out and unanswered.
		expect(api.items.starred).toHaveBeenCalled();

		api.auth.session.mockResolvedValue(sessionFor('user-b'));
		await authStore.load();
		resolveA([ITEM_A]);
		await settle();

		expect(count()).not.toBe('1');
		expect(screen.queryByText("Alpha's secret item")).toBeNull();
	});

	it("drops A's items the moment the identity changes after they rendered", async () => {
		// BUG-3005's shape: nothing is racing. A's list is on screen, then the
		// account changes in place. The store's reset clears the ids; a page
		// that kept its own copy would go on showing them.
		api.items.starred.mockResolvedValue([ITEM_A]);
		await starredStore.load('ws');
		const screen = render(StarredPage);
		await settle();
		expect(screen.queryByText("Alpha's secret item")).not.toBeNull();

		api.items.starred.mockResolvedValue([]);
		api.auth.session.mockResolvedValue(sessionFor('user-b'));
		await authStore.load();
		await settle();
		expect(screen.queryByText("Alpha's secret item")).toBeNull();
	});

	it("renders A's items when the identity holds still", async () => {
		// The counterfactual the test above needs to mean anything: the same
		// harness, no identity change, and the data DOES render. Without it,
		// `count === '0'` is equally consistent with a page that never works.
		api.items.starred.mockResolvedValue([ITEM_A]);

		void starredStore.load('ws');
		const screen = render(StarredPage);
		const count = () => screen.container.querySelector('.count')?.textContent ?? '';
		await settle();

		expect(count()).toBe('1');
	});
});
