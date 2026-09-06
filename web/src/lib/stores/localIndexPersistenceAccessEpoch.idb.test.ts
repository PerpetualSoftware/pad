import { describe, it, expect } from 'vitest';
import type { ItemIndexRow } from '$lib/types';
import { loadPersistence, rawItems } from '../../test/idbHarness';

/**
 * IDEA-2898 review round 1, F2 — the cross-tab hole the in-RAM fence cannot see.
 *
 * `fencedIds` stops a stale optimistic write from resurrecting a resync-dropped
 * row within ONE tab. Two tabs share one database, and that is where this bites:
 * tab A resyncs under a narrowed scope (`persistReplace` clears the items store,
 * clears the tombstones, stamps the new epoch), then tab B's already-queued
 * write for a row it saw under the OLD scope commits into the cleared store.
 *
 * What makes it worse than an ordinary stale row — and the reason it is a P1
 * rather than a wart — is that `persistUpserts` does not touch the meta row. The
 * cache ends up holding an old-scope row while ADVERTISING the new epoch, so on
 * warm boot hydrate returns the revoked row, the next delta reports the same
 * epoch the cache claims, and no resync ever fires again. The signal is not
 * merely wrong once; it is permanently inert.
 *
 * These are sequential calls through the real module, which is how the
 * PLAN-2636 races are pinned: IDB serializes the transactions, so ordering the
 * calls IS the interleaving.
 */

function row(id: string, seq: number): ItemIndexRow {
	return { id, seq, collection_slug: 'revoked' } as unknown as ItemIndexRow;
}

describe('IDEA-2898 F2 — a writer under a superseded epoch cannot reinsert into the cache', () => {
	it('refuses a stale-epoch upsert, accepts a current-epoch one, and lets an unclaimed writer through', async () => {
		const U = null;
		const WS = 'ws-epoch-fence';
		const { persistReplace, persistUpserts, hydrate } = await loadPersistence();

		// Tab A resyncs under the narrowed scope: 'secret' is gone, epoch e2.
		await persistReplace(U, WS, [row('keeper', 1)], '5', false, 'e2');
		expect((await rawItems(U, WS)).map((r) => r.id).sort()).toEqual(['keeper']);

		// Tab B, still believing it is under e1, writes the row it saw then.
		await persistUpserts(U, WS, [row('secret', 2)], 'e1');
		expect((await rawItems(U, WS)).map((r) => r.id).sort()).toEqual(['keeper']);

		// And the meta row still says e2 — the check that the fence protected
		// the SIGNAL, not just this one row. A cache holding 'secret' while
		// advertising e2 is the state no later delta can detect.
		const afterRefusal = await hydrate(U, WS);
		expect(afterRefusal.accessEpoch).toBe('e2');
		expect(afterRefusal.items.map((r) => r.id).sort()).toEqual(['keeper']);

		// CONTROL LEG. The fence must not refuse a writer that is current —
		// otherwise every optimistic write after any resync would be silently
		// dropped, which no assertion above would catch.
		await persistUpserts(U, WS, [row('allowed', 3)], 'e2');
		expect((await rawItems(U, WS)).map((r) => r.id).sort()).toEqual(['allowed', 'keeper']);

		// SECOND CONTROL LEG. A writer with no baseline yet makes no claim, and
		// a workspace that has never resynced has nothing to be stale relative
		// to. Refusing these would break the first write of every session.
		await persistUpserts(U, WS, [row('unclaimed', 4)], null);
		expect((await rawItems(U, WS)).map((r) => r.id).sort()).toEqual([
			'allowed',
			'keeper',
			'unclaimed',
		]);
	});

	it('fences persistDelta too, and refuses its meta write rather than regressing the epoch', async () => {
		// Round 2. An AUTHORITATIVE delta is not exempt from the cross-tab
		// race, and it is the worse case of the two: `persistUpserts` writing
		// late leaves a stale row under the current epoch, while `persistDelta`
		// writing late leaves a stale row AND drags the meta row back to the
		// old epoch. That second part disables the fence for every writer that
		// follows — one late delta and the cache stops defending itself.
		const U = null;
		const WS = 'ws-epoch-fence-delta';
		const { persistReplace, persistDelta, hydrate } = await loadPersistence();

		await persistReplace(U, WS, [row('keeper', 1)], '5', false, 'e2');

		// The old tab's delta, built under e1, committing after the replace.
		await persistDelta(U, WS, [row('secret', 6)], '6', false, 'e1');

		const after = await hydrate(U, WS);
		expect(after.items.map((r) => r.id).sort()).toEqual(['keeper']);
		// The meta row is the part that matters most: a regressed epoch here is
		// not one bad row, it is the fence switched off.
		expect(after.accessEpoch).toBe('e2');
		expect(after.cursor).toBe('5');

		// CONTROL LEG. A current-epoch delta must still land, or every
		// authoritative write after a resync would be silently dropped.
		await persistDelta(U, WS, [row('fresh', 7)], '7', false, 'e2');
		const healthy = await hydrate(U, WS);
		expect(healthy.items.map((r) => r.id).sort()).toEqual(['fresh', 'keeper']);
		expect(healthy.cursor).toBe('7');
	});
});
