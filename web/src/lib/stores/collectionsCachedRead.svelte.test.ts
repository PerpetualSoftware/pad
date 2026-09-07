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
	readDurableEpoch: vi.fn(async () => undefined as string | null | undefined),
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

const { api } = await import('$lib/api/client');
const { collectionStore } = await import('./collections.svelte');

function coll(slug: string): Collection {
	return { id: `id-${slug}`, slug, name: slug } as unknown as Collection;
}

afterEach(() => {
	for (const fn of Object.values(persistence)) {
		if (typeof fn === 'function' && 'mockClear' in fn) fn.mockClear();
	}
});

describe('TASK-2946 — what loadCollections hands the cache', () => {
	it('stamps with the DURABLE epoch, so a cold tab with no RAM epoch still caches', async () => {
		// The path the feature actually runs on, and the one every IDB test
		// missed by seeding epochs directly (codex round 2). The workspace
		// layout starts this fetch BEFORE `localIndex.bootstrap`, so RAM's epoch
		// is null here; an earlier draft bracketed the fetch with RAM and
		// therefore refused to cache on every fresh online visit — inert in its
		// most common path, with all nine IDB legs green.
		persistence.readDurableEpoch.mockResolvedValueOnce('e1');
		vi.spyOn(api.collections, 'list').mockResolvedValueOnce([coll('tasks')]);

		await collectionStore.loadCollections('alpha');

		expect(persistence.persistCollections).toHaveBeenCalledTimes(1);
		const args = persistence.persistCollections.mock.calls.at(-1) as unknown[];
		expect(args[3]).toBe('e1');
	});

	it('does not cache a list it could not read a durable scope for', async () => {
		// `undefined` is "no sync row / unreadable cache", not "the scope is
		// null" — nothing to vouch for the stamp, so the write is refused
		// downstream. Pinned here so the caller keeps passing the value through
		// rather than substituting a default.
		persistence.readDurableEpoch.mockResolvedValueOnce(undefined);
		vi.spyOn(api.collections, 'list').mockResolvedValueOnce([coll('tasks')]);

		await collectionStore.loadCollections('beta');

		const args = persistence.persistCollections.mock.calls.at(-1) as unknown[];
		expect(args[3]).toBeUndefined();
	});
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
