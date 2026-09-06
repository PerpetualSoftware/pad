import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '$lib/api/client';
import type { ItemIndexRow } from '$lib/types';

/**
 * IDEA-2898 — the WIRING assertion (CONVE-19).
 *
 * One of this change's properties is not about what a function decides but
 * about what it HANDS ON: a resync writing `null` to the durable epoch while
 * deliberately keeping a known one in RAM. It is invisible to a behavioural
 * test in jsdom, because IDB is unsupported there and the persistence calls are
 * no-ops — the store's own tests would pass with the argument dropped.
 *
 * So this file mocks the persistence module and asserts the ARGUMENT. That is a
 * weaker kind of test and it is the right one here: the question is whether the
 * caller passes the value, not whether the callee honours it (the callee is
 * pinned by localIndexPersistenceAccessEpoch.idb.test.ts against a real
 * database).
 */

const persistence = vi.hoisted(() => ({
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
	persistReplace: vi.fn(async () => undefined),
	persistAccessEpoch: vi.fn(async () => undefined),
	persistRetag: vi.fn(async () => undefined),
	persistUpserts: vi.fn(async () => undefined),
	wipe: vi.fn(async () => undefined),
}));
vi.mock('./localIndexPersistence', () => persistence);

const { localIndex } = await import('./localIndex.svelte');

const ws = 'access-epoch-wiring';

function row(id: string, seq: number, collection = 'kept'): ItemIndexRow {
	return {
		id,
		seq,
		title: `Title ${id}`,
		collection_slug: collection,
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
	} as ItemIndexRow;
}

afterEach(() => {
	vi.restoreAllMocks();
	for (const fn of Object.values(persistence)) fn.mockClear();
	localIndex.reset(ws);
});

describe('IDEA-2898 — what the resync hands on', () => {
	it('adopts the PERSISTED epoch on a warm hydrate, so an unchanged scope costs nothing', async () => {
		// The offline-revocation case turns on this line, and its absence is
		// almost invisible: with no baseline adopted, a populated cache resyncs
		// on the first delta, which looks like detection working. The
		// discriminating case is the QUIET one — a cache whose scope has NOT
		// changed must not pay a full snapshot on every reload.
		//
		// Needs the mocked persistence module: jsdom has no IndexedDB, so the
		// real `hydrate` always returns an empty cache and the warm branch is
		// unreachable in the behavioural file.
		persistence.hydrate.mockResolvedValueOnce({
			items: [row('cached', 1, 'kept')],
			cursor: '1',
			includesUnparentedMetadata: false,
			accessEpoch: 'persisted-e1',
			durableRead: true,
			retags: {},
		} as unknown as Awaited<ReturnType<typeof persistence.hydrate>>);
		const listIndex = vi.spyOn(api.items, 'listIndex');
		vi.spyOn(api.items, 'changes').mockResolvedValue({
			changes: [],
			cursor: '1',
			includes_unparented_metadata: false,
			access_epoch: 'persisted-e1',
		});

		await localIndex.bootstrap(ws, { userId: null });

		expect(localIndex.accessEpochFor(ws)).toBe('persisted-e1');
		// The scope the cache was written under is the scope the server still
		// reports, so no authoritative snapshot was needed.
		expect(listIndex).not.toHaveBeenCalled();
	});

	it('persists the told epoch after a JOINED resync, not only in RAM', async () => {
		// Resyncs are deduplicated per workspace. When `ensureAccessScope`
		// joins one that is already running, that resync did its own
		// `persistReplace` under its OWN baseline — so assigning the told epoch
		// afterwards updates RAM and leaves the meta row behind. The session
		// converges and every reload then hydrates the stale baseline and pays
		// a full resync for a scope that has not changed since. Invisible to a
		// single-session behavioural test, which is why it is asserted here.
		persistence.hydrate.mockResolvedValueOnce({
			items: [row('cached', 1, 'kept')],
			cursor: '1',
			includesUnparentedMetadata: false,
			accessEpoch: 'e1',
			durableRead: true,
			retags: {},
		} as unknown as Awaited<ReturnType<typeof persistence.hydrate>>);
		vi.spyOn(api.items, 'changes').mockResolvedValue({
			changes: [],
			cursor: '1',
			includes_unparented_metadata: false,
			access_epoch: 'e1',
		});
		await localIndex.bootstrap(ws, { userId: null });
		expect(localIndex.accessEpochFor(ws)).toBe('e1');

		// A resync that carries NO epoch on its snapshot, started first and
		// still in flight, so the next caller joins it rather than starting one.
		let releaseSnapshot: (() => void) | undefined;
		vi.spyOn(api.items, 'listIndex').mockReturnValue(
			new Promise((resolve) => {
				releaseSnapshot = () =>
					resolve({
						items: [row('cached', 1, 'kept')],
						total: 1,
						cursor: '1',
						includes_unparented_metadata: false,
						// No access_epoch: an older server answering the snapshot.
					});
			}) as unknown as ReturnType<typeof api.items.listIndex>,
		);
		const first = localIndex.ensureProjectionScope(ws, true);
		const joined = localIndex.ensureAccessScope(ws, 'e2');
		releaseSnapshot?.();
		await Promise.all([first, joined]);

		expect(localIndex.accessEpochFor(ws)).toBe('e2');
		expect(persistence.persistAccessEpoch).toHaveBeenCalled();
		const args = persistence.persistAccessEpoch.mock.calls.at(-1) as unknown[];
		expect(args[2]).toBe('e2');
		// AND the value it expects to be replacing. The patch is a
		// compare-and-set; a wrong `expectedPrevious` makes it a no-op that
		// still looks right in RAM, and the disagreement only shows up as a
		// resync on the next reload.
		expect(args[3]).toBe('e1');
	});

	it('persists the told epoch after a joined resync on a NULL baseline too', async () => {
		// The same durable-repair as above, on the other branch. A populated
		// cache with no recorded baseline resyncs by design; if that resync is
		// one it JOINED, the meta row is left with no epoch and the very next
		// reload repeats the whole thing. Two branches, one property — and the
		// first version of this fix had it on only one of them, which no test
		// in this file could tell apart.
		persistence.hydrate.mockResolvedValueOnce({
			items: [row('cached', 1, 'kept')],
			cursor: '1',
			includesUnparentedMetadata: false,
			accessEpoch: null,
			durableRead: true,
			retags: {},
		} as unknown as Awaited<ReturnType<typeof persistence.hydrate>>);
		vi.spyOn(api.items, 'changes').mockResolvedValue({
			changes: [],
			cursor: '1',
			includes_unparented_metadata: false,
		});
		let releaseSnapshot: (() => void) | undefined;
		vi.spyOn(api.items, 'listIndex').mockReturnValue(
			new Promise((resolve) => {
				releaseSnapshot = () =>
					resolve({
						items: [row('cached', 1, 'kept')],
						total: 1,
						cursor: '1',
						includes_unparented_metadata: false,
						// No access_epoch on the snapshot.
					});
			}) as unknown as ReturnType<typeof api.items.listIndex>,
		);
		await localIndex.bootstrap(ws, { userId: null });
		expect(localIndex.accessEpochFor(ws)).toBeNull();

		persistence.persistAccessEpoch.mockClear();
		const first = localIndex.ensureProjectionScope(ws, true);
		const joined = localIndex.ensureAccessScope(ws, 'told');
		releaseSnapshot?.();
		await Promise.all([first, joined]);

		expect(localIndex.accessEpochFor(ws)).toBe('told');
		expect(persistence.persistAccessEpoch).toHaveBeenCalled();
		const nullBranchArgs = persistence.persistAccessEpoch.mock.calls.at(-1) as unknown[];
		expect(nullBranchArgs[2]).toBe('told');
		// The baseline being repaired on this branch is the ABSENT one.
		expect(nullBranchArgs[3]).toBeNull();
	});

	it('does NOT adopt when the durable read FAILED, only when it answered', async () => {
		// `hydrate` returns the same empty payload for a real IDB failure as for
		// a genuinely empty cache — deliberately, since a best-effort cache
		// should not take the app down. But those are opposite facts here, and
		// reading a failure as "nothing stored" puts the silent adopt straight
		// back: the durable cache may hold rows from a scope nobody checked,
		// and adopting stamps the new epoch onto them through the next delta.
		persistence.hydrate.mockResolvedValueOnce({
			items: [],
			cursor: '0',
			includesUnparentedMetadata: null,
			accessEpoch: null,
			durableRead: false,
			retags: {},
		} as unknown as Awaited<ReturnType<typeof persistence.hydrate>>);
		vi.spyOn(api.items, 'listIndex').mockResolvedValueOnce({
			items: [],
			total: 0,
			cursor: '0',
			includes_unparented_metadata: false,
		});
		await localIndex.bootstrap(ws, { userId: null });

		expect(await localIndex.ensureAccessScope(ws, 'e2')).toBe(false);
		expect(localIndex.accessEpochFor(ws)).toBeNull();

		// CONTROL LEG. The same shape with a SUCCESSFUL read adopts, so this is
		// a test of the read's outcome and not of the bootstrap generally.
		localIndex.reset(ws);
		persistence.hydrate.mockResolvedValueOnce({
			items: [],
			cursor: '0',
			includesUnparentedMetadata: null,
			accessEpoch: null,
			durableRead: true,
			retags: {},
		} as unknown as Awaited<ReturnType<typeof persistence.hydrate>>);
		vi.spyOn(api.items, 'listIndex').mockResolvedValueOnce({
			items: [],
			total: 0,
			cursor: '0',
			includes_unparented_metadata: false,
		});
		await localIndex.bootstrap(ws, { userId: null });

		expect(await localIndex.ensureAccessScope(ws, 'e2')).toBe(false);
		expect(localIndex.accessEpochFor(ws)).toBe('e2');
	});

	it('hands persistDelta the baseline the rows were applied under', async () => {
		// `applyDelta` writes through to IDB, and the meta row it stamps is the
		// ONLY record of the scope for the next session. Passing null there
		// leaves every reload with no baseline over a populated cache, which
		// resyncs on the first delta — a full snapshot per reload, forever, for
		// a scope that never changed. Invisible in jsdom, where persistence is
		// a no-op, so it is asserted as an ARGUMENT.
		// Through a cold bootstrap, because the silent adopt now requires the
		// durable cache to have answered first.
		vi.spyOn(api.items, 'listIndex').mockResolvedValueOnce({
			items: [],
			total: 0,
			cursor: '0',
			includes_unparented_metadata: false,
			access_epoch: 'e1',
		});
		await localIndex.bootstrap(ws, { userId: null });
		persistence.persistDelta.mockClear();

		localIndex.applyDelta(ws, [], '7', false);

		expect(persistence.persistDelta).toHaveBeenCalled();
		const args = persistence.persistDelta.mock.calls.at(-1) as unknown[];
		expect(args[5]).toBe('e1');
	});

	it('writes the PRESERVED baseline to IDB when a resync snapshot carries no epoch', async () => {
		// F3 keeps a known baseline in RAM when the snapshot has none. Writing
		// `null` to the durable copy would contradict it, and the contradiction
		// outlives the session: the next warm boot hydrates the durable value,
		// reads a null baseline over a populated cache as "cannot know what
		// this was authorised for", and pays a full snapshot to rediscover the
		// epoch this session already knew.
		await localIndex.ensureAccessScope(ws, 'e1');
		localIndex.upsert(ws, row('keeper', 1, 'kept'));
		localIndex.applyDelta(ws, [], '1', false);

		vi.spyOn(api.items, 'listIndex').mockResolvedValueOnce({
			items: [row('keeper', 1, 'kept')],
			total: 1,
			cursor: '1',
			includes_unparented_metadata: false,
			// No access_epoch — an older server answering the snapshot.
		});
		await localIndex.ensureAccessScope(ws, 'e2');

		expect(persistence.persistReplace).toHaveBeenCalled();
		const args = persistence.persistReplace.mock.calls.at(-1) as unknown[];
		expect(args[5]).not.toBeNull();
		expect(args[5]).toBe(localIndex.accessEpochFor(ws));
	});
});
