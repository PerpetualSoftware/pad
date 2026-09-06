import { describe, it, expect } from 'vitest';
import type { ItemIndexRow } from '$lib/types';
import { loadPersistence, openSecondConnection } from '../../test/idbHarness';

/**
 * IDEA-2898 — the durable half of the access baseline.
 *
 * The comparison in `localIndex` can only detect an OFFLINE revocation if the
 * epoch the cache was written under survives the tab being closed. That is this
 * module's whole job here: `persistDelta` and `persistReplace` stamp the meta
 * row, and `hydrate` hands the stamp back so the first delta of the next
 * session has something to disagree with.
 *
 * These are sequential calls through the real module, which is how the
 * PLAN-2636 races are pinned: IDB serializes the transactions, so ordering the
 * calls IS the interleaving.
 *
 * SCOPE. This file covers only what the minimal shape ships. The cross-tab
 * hazard — a write from a tab that has not yet learned the new scope
 * reinserting a row while the cache still advertises the current epoch — is
 * NOT closed here and is not meant to be; it is F2, and PLAN-2903 owns it.
 */

function row(id: string, seq: number): ItemIndexRow {
	return { id, seq, collection_slug: 'c' } as unknown as ItemIndexRow;
}

describe('IDEA-2898 — the access epoch survives a reload', () => {
	it('round-trips the epoch a replace was written under', async () => {
		const U = null;
		const WS = 'ws-epoch-replace';
		const { persistReplace, hydrate } = await loadPersistence();

		await persistReplace(U, WS, [row('a', 1)], '5', false, 'e1');

		const after = await hydrate(U, WS);
		expect(after.accessEpoch).toBe('e1');
		expect(after.cursor).toBe('5');
		expect(after.items.map((r) => r.id)).toEqual(['a']);
	});

	it('moves the stored epoch forward when a delta carries a new one', async () => {
		const U = null;
		const WS = 'ws-epoch-delta';
		const { persistReplace, persistDelta, hydrate } = await loadPersistence();

		await persistReplace(U, WS, [row('a', 1)], '5', false, 'e1');
		await persistDelta(U, WS, [row('b', 6)], '6', false, 'e2');

		// The stamp tracks the LAST authoritative statement, not the first —
		// otherwise a session that resynced would keep re-detecting the same
		// change on every reload.
		const after = await hydrate(U, WS);
		expect(after.accessEpoch).toBe('e2');
		expect(after.cursor).toBe('6');
	});

	it('hydrates a null epoch rather than inventing one when the server sent none', async () => {
		const U = null;
		const WS = 'ws-epoch-absent';
		const { persistDelta, hydrate } = await loadPersistence();

		// A server that predates the field: the caller has no epoch to stamp.
		await persistDelta(U, WS, [row('a', 1)], '5', false, null);

		// Null must stay null. `ensureAccessScope` reads a null baseline over a
		// populated cache as "cannot know what this was authorised for" and
		// resyncs; substituting any value here would turn that into a silent
		// adopt, which is the exact defect this change exists to close.
		expect((await hydrate(U, WS)).accessEpoch).toBeNull();
	});

	it('persistAccessEpoch rewrites ONLY the epoch on an existing cache', async () => {
		const U = null;
		const WS = 'ws-epoch-patch';
		const { persistReplace, persistAccessEpoch, hydrate } = await loadPersistence();

		await persistReplace(U, WS, [row('a', 1)], '5', false, 'e1');
		await persistAccessEpoch(U, WS, 'e2');

		const after = await hydrate(U, WS);
		expect(after.accessEpoch).toBe('e2');
		// Everything else the meta row carries is untouched — this exists to
		// repair a baseline after a JOINED resync, not to make a claim about
		// how far the cache has synced.
		expect(after.cursor).toBe('5');
		expect(after.includesUnparentedMetadata).toBe(false);
		expect(after.items.map((r) => r.id)).toEqual(['a']);
	});

	it('persistAccessEpoch does NOT mint a meta row for a cache that never synced', async () => {
		const U = null;
		const WS = 'ws-epoch-patch-empty';
		const { persistAccessEpoch, hydrate } = await loadPersistence();

		await persistAccessEpoch(U, WS, 'e1');

		// A cache with no meta row has nothing to describe. Writing one here
		// would invent a cursor — and a cursor is a claim that everything up to
		// it has been seen, which is exactly the claim this cache cannot make.
		const after = await hydrate(U, WS);
		expect(after.accessEpoch).toBeNull();
		expect(after.cursor).toBe('0');
	});

	it('reads a meta row with no epoch key as null, not undefined', async () => {
		const U = null;
		const WS = 'ws-epoch-legacy-key';
		const { hydrate, LOCAL_INDEX_SCHEMA_VERSION } = await loadPersistence();

		// A meta row at the CURRENT cache version whose `accessEpoch` key is
		// simply absent. Written raw, at the module's own format version, so
		// the ONLY thing this fixture can exercise is the missing key — a
		// stale `schemaVersion` would wipe the cache for an unrelated reason
		// and the assertion would no longer discriminate.
		const db = await openSecondConnection(U, WS);
		try {
			await db.put('meta', {
				key: 'sync',
				cursor: '5',
				schemaVersion: LOCAL_INDEX_SCHEMA_VERSION,
				includesUnparentedMetadata: true,
			});
			await db.put('items', row('a', 1));
		} finally {
			db.close();
		}

		// NULL, not undefined. `ensureAccessScope` branches on `=== null` to
		// find the no-baseline case; an `undefined` would miss that branch,
		// compare unequal to every incoming epoch, and resync on every poll
		// forever.
		const after = await hydrate(U, WS);
		expect(after.accessEpoch).toBeNull();
		expect(after.items.map((r) => r.id)).toEqual(['a']);
	});
});
