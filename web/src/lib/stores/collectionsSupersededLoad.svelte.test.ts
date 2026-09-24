import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Collection } from '$lib/types';

/**
 * BUG-2144: a slower `loadCollections(A)` resolving AFTER `loadCollections(B)`
 * must not leave A's collections in the global store while B is active.
 *
 * The guard is `loadCollections`' `!isLatest()` drop (collectionsFlight). It is
 * tested here through `collectionStore` itself, because the only other test of
 * `isLatest` is singleFlight.test.ts, which tests the primitive and not its
 * binding (CONVE-19): with the drop removed from `loadCollections`, every store
 * test still passed (measured on BUG-2144's trail). The existing superseded-
 * load leg in collectionsEnsure resolves every request with the SAME list, so
 * it could not see which one committed; each workspace here answers with its
 * own list, which is what makes the stale commit observable.
 */

async function flush(): Promise<void> {
	await new Promise((r) => setTimeout(r, 0));
}

function coll(slug: string): Collection {
	return { id: `id-${slug}`, slug, name: slug, is_default: true, sort_order: 0 } as Collection;
}

async function load() {
	vi.resetModules();
	const { api } = await import('$lib/api/client');
	const { collectionStore } = await import('./collections.svelte');
	return { api, collectionStore };
}

afterEach(() => {
	vi.restoreAllMocks();
});

describe('BUG-2144 — a superseded collections load does not commit', () => {
	it('CONTROL: a single load commits its list and stamps its workspace', async () => {
		const { api, collectionStore } = await load();
		vi.spyOn(api.collections, 'list').mockResolvedValue([coll('alpha-only')]);

		await collectionStore.loadCollections('alpha');

		expect(collectionStore.collections.map((c) => c.slug)).toEqual(['alpha-only']);
		expect(collectionStore.collectionsWorkspace).toBe('alpha');
	});

	it('A then B, B answers first, A answers last: the store holds B and says so', async () => {
		const { api, collectionStore } = await load();
		const pending = new Map<string, (v: Collection[]) => void>();
		vi.spyOn(api.collections, 'list').mockImplementation(
			(ws: string) => new Promise<Collection[]>((resolve) => pending.set(ws, resolve))
		);

		const alpha = collectionStore.loadCollections('alpha');
		const beta = collectionStore.loadCollections('beta');
		await flush();
		expect([...pending.keys()].sort()).toEqual(['alpha', 'beta']);

		pending.get('beta')!([coll('beta-only')]);
		await beta;
		pending.get('alpha')!([coll('alpha-only')]);
		await alpha;

		expect(collectionStore.collections.map((c) => c.slug)).toEqual(['beta-only']);
		expect(collectionStore.collectionsWorkspace).toBe('beta');
		expect(collectionStore.collectionsAreFreshFor('beta')).toBe(true);
		expect(collectionStore.collectionsAreFreshFor('alpha')).toBe(false);
	});
});
