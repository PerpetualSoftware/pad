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
		retags: {},
	})),
	persistDelta: vi.fn(async () => undefined),
	persistRemovals: vi.fn(async () => undefined),
	persistReplace: vi.fn(async () => undefined),
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
