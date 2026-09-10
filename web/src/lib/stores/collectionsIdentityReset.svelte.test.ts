import { describe, expect, it, vi, beforeEach } from 'vitest';

/**
 * BUG-3005 — `collectionStore` retains `collections`, `items` and `activeItem`
 * globally, and its loader is keyed only by workspace SLUG.
 *
 * The workspace layout drives it from a slug-keyed effect, so a same-route
 * sign-in as a different user starts no new load at all: A's collection names
 * and item titles stay on screen inside B's session, with no request having
 * been made for them. And a load issued as A can commit after B signs in,
 * because the store's only fence is a NAVIGATION fence that a same-route swap
 * does not move.
 */

const api = vi.hoisted(() => ({
	collections: { list: vi.fn() },
	items: { list: vi.fn(), listByCollection: vi.fn(), get: vi.fn() },
}));

vi.mock('$lib/api/client', () => ({ api }));

// The durable cache is a different concern (TASK-2946); stub it inert so this
// file tests the identity fence and nothing else.
vi.mock('./localIndexPersistence', () => ({
	hydrateCollections: vi.fn(async () => null),
	persistCollections: vi.fn(async () => {}),
	readDurableEpoch: vi.fn(async () => null),
}));

// A REAL epoch and fence. A stub that always answered "still current" would let
// every assertion below pass against a store with no fence at all.
const auth = vi.hoisted(() => {
	const listeners = new Set<() => void>();
	let epoch = 0;
	return {
		userId: 'user-1',
		get identityEpoch() { return epoch; },
		identityFence() {
			const captured = epoch;
			return () => epoch === captured;
		},
		onIdentityChange(fn: () => void) {
			listeners.add(fn);
			return () => listeners.delete(fn);
		},
		fireIdentityChange() {
			epoch++;
			for (const fn of listeners) fn();
		},
		resetListeners() {
			listeners.clear();
			epoch = 0;
		},
	};
});

vi.mock('./auth.svelte', () => ({ authStore: auth }));

const collection = (slug: string) => ({ id: slug, slug, name: slug, is_default: true, sort_order: 1 });
const item = (id: string) => ({ id, slug: id, title: id });

describe('collectionStore across an identity change', () => {
	beforeEach(() => {
		vi.resetModules();
		auth.resetListeners();
		api.collections.list.mockReset();
		api.items.list.mockReset();
		api.items.listByCollection.mockReset();
		api.items.get.mockReset();
	});

	it('drops collections, items and the open item when the identity changes', async () => {
		const { collectionStore } = await import('./collections.svelte');
		api.collections.list.mockResolvedValueOnce([collection('tasks')]);
		await collectionStore.loadCollections('ws');
		api.items.list.mockResolvedValueOnce([item('item-a')]);
		await collectionStore.loadItems('ws');
		api.items.get.mockResolvedValueOnce(item('item-a'));
		await collectionStore.loadItem('ws', 'item-a');

		// PRECONDITION: all three are populated, or "they were dropped" is a
		// claim about state that was never there.
		expect(collectionStore.collections).toHaveLength(1);
		expect(collectionStore.items).toHaveLength(1);
		expect(collectionStore.activeItem).not.toBeNull();
		expect(collectionStore.collectionsAreFreshFor('ws')).toBe(true);

		auth.fireIdentityChange();

		expect(collectionStore.collections).toHaveLength(0);
		expect(collectionStore.items).toHaveLength(0);
		expect(collectionStore.activeItem).toBeNull();
	});

	it('clears the FRESHNESS STAMPS with the arrays, so recovery re-fetches', async () => {
		// Emptying `collections` while leaving `collectionsWorkspace` set would
		// make `collectionsAreFreshFor(ws)` answer true for an empty list — the
		// one answer that stops TASK-2200's recovery from ever re-fetching, so
		// the sidebar would stay permanently empty for the new user.
		const { collectionStore } = await import('./collections.svelte');
		api.collections.list.mockResolvedValueOnce([collection('tasks')]);
		await collectionStore.loadCollections('ws');
		api.items.list.mockResolvedValueOnce([item('item-a')]);
		await collectionStore.loadItems('ws');
		expect(collectionStore.itemsAreFreshFor('ws')).toBe(true);

		auth.fireIdentityChange();

		expect(collectionStore.collectionsAreFreshFor('ws')).toBe(false);
		expect(collectionStore.itemsAreFreshFor('ws')).toBe(false);
	});

	it('refuses a collections load that RESOLVES after the identity changed', async () => {
		// The navigation fence cannot catch this: the workspace never changed,
		// so this load is still the latest one for `ws`.
		const { collectionStore } = await import('./collections.svelte');
		let resolve!: (v: unknown) => void;
		api.collections.list.mockReturnValueOnce(new Promise((r) => { resolve = r; }));

		const inflight = collectionStore.loadCollections('ws');
		auth.fireIdentityChange();
		resolve([collection('tasks')]);
		await inflight;

		expect(collectionStore.collections).toHaveLength(0);
		expect(collectionStore.collectionsAreFreshFor('ws')).toBe(false);
	});

	it('refuses an items load that resolves after the identity changed', async () => {
		const { collectionStore } = await import('./collections.svelte');
		let resolve!: (v: unknown) => void;
		api.items.list.mockReturnValueOnce(new Promise((r) => { resolve = r; }));

		const inflight = collectionStore.loadItems('ws');
		auth.fireIdentityChange();
		resolve([item('item-a')]);
		await inflight;

		expect(collectionStore.items).toHaveLength(0);
		expect(collectionStore.itemsAreFreshFor('ws')).toBe(false);
	});

	it('refuses a COLLECTION-FILTERED items load that resolves after the change', async () => {
		// The other arm of the same method. It assigns through a different
		// branch and stamps `itemsWorkspace` differently, so fencing one arm
		// leaves the other publishing another user's items.
		const { collectionStore } = await import('./collections.svelte');
		let resolve!: (v: unknown) => void;
		api.items.listByCollection.mockReturnValueOnce(new Promise((r) => { resolve = r; }));

		const inflight = collectionStore.loadItems('ws', 'tasks');
		auth.fireIdentityChange();
		resolve([item('item-a')]);
		await inflight;

		expect(collectionStore.items).toHaveLength(0);
	});

	it('returns null from loadItem when the identity moved, and does not publish it', async () => {
		// The return value matters as much as the store write: the caller renders
		// what it is handed, so handing back A's item puts it on B's screen even
		// with `activeItem` untouched.
		const { collectionStore } = await import('./collections.svelte');
		let resolve!: (v: unknown) => void;
		api.items.get.mockReturnValueOnce(new Promise((r) => { resolve = r; }));

		const inflight = collectionStore.loadItem('ws', 'item-a');
		auth.fireIdentityChange();
		resolve(item('item-a'));

		expect(await inflight).toBeNull();
		expect(collectionStore.activeItem).toBeNull();
	});

	it('still commits an ordinary load when the identity holds still', async () => {
		// The counterfactual. A fence that refused everything would leave every
		// user with an empty sidebar, which is worse than the leak.
		const { collectionStore } = await import('./collections.svelte');
		api.collections.list.mockResolvedValueOnce([collection('tasks')]);
		await collectionStore.loadCollections('ws');
		api.items.list.mockResolvedValueOnce([item('item-a')]);
		await collectionStore.loadItems('ws');
		api.items.get.mockResolvedValueOnce(item('item-a'));
		const loaded = await collectionStore.loadItem('ws', 'item-a');

		expect(collectionStore.collections).toHaveLength(1);
		expect(collectionStore.items).toHaveLength(1);
		expect(loaded).not.toBeNull();
		expect(collectionStore.collectionsAreFreshFor('ws')).toBe(true);
	});
});
