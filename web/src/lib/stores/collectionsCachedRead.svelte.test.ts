import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Collection } from '$lib/types';

/**
 * TASK-2946 — `collectionStore.cachedCollection`, the read the board falls back
 * to when the server is unreachable.
 *
 * The fence itself lives in `hydrateCollections` and is pinned against a real
 * IndexedDB in localIndexPersistenceCollections.idb.test.ts. What is pinned
 * HERE is the store's half: that it asks for the list under the SAME user
 * namespace the rows live in, and that it selects by slug rather than handing
 * back whatever the cache holds.
 */

const persistence = vi.hoisted(() => ({
	hydrateCollections: vi.fn(async () => null as Collection[] | null),
	persistCollections: vi.fn(async () => undefined),
	hydrate: vi.fn(async () => ({
		items: [],
		cursor: '0',
		includesUnparentedMetadata: null,
		accessEpoch: null,
		durableRead: true,
		retags: {},
	})),
	persistDelta: vi.fn(async () => undefined),
	persistRemovals: vi.fn(async () => undefined),
	persistReplace: vi.fn(async () => true),
	persistAccessEpoch: vi.fn(async () => undefined),
	persistRetag: vi.fn(async () => undefined),
	persistUpserts: vi.fn(async () => undefined),
	wipe: vi.fn(async () => undefined),
	LOCAL_INDEX_SCHEMA_VERSION: 4,
	IDB_FORMAT_VERSION: 2,
}));
vi.mock('./localIndexPersistence', () => persistence);

const { collectionStore } = await import('./collections.svelte');

function coll(slug: string): Collection {
	return { id: `id-${slug}`, slug, name: slug } as unknown as Collection;
}

afterEach(() => {
	for (const fn of Object.values(persistence)) {
		if (typeof fn === 'function' && 'mockClear' in fn) fn.mockClear();
	}
});

describe('TASK-2946 — cachedCollection', () => {
	it('selects the requested slug out of the cached list', async () => {
		persistence.hydrateCollections.mockResolvedValueOnce([coll('tasks'), coll('ideas')]);

		const found = await collectionStore.cachedCollection('alpha', 'ideas');

		expect(found?.slug).toBe('ideas');
	});

	it('returns null for a slug the cached list does not contain', async () => {
		// Not the same as an empty cache, and the caller must not tell them
		// apart by accident: a list that simply lacks this collection means the
		// scope did not include it, and rendering anything would be inventing.
		persistence.hydrateCollections.mockResolvedValueOnce([coll('tasks')]);

		expect(await collectionStore.cachedCollection('alpha', 'ideas')).toBeNull();
	});

	it('returns null when the fence refused the list', async () => {
		persistence.hydrateCollections.mockResolvedValueOnce(null);

		expect(await collectionStore.cachedCollection('alpha', 'tasks')).toBeNull();
	});
});
