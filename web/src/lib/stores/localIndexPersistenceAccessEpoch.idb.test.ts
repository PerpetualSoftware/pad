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

describe('IDEA-2898 — a writer that cannot confirm the scope may not speak for it', () => {
	it('lets an unconfirmed writer store rows but not touch the meta row, and asks for a resync', async () => {
		// ROUND 4 CHANGED THIS PROPERTY, and the change is the point rather
		// than a detail. Rounds 1-3 refused the unconfirmed writer's ROWS, on
		// the reading that a differing epoch means a STALE writer. It does not:
		// an epoch is a hash, two epochs are merely different, and the writer
		// this refused might have been carrying the broader scope.
		//
		// What is actually protected is the META ROW. A writer that cannot
		// write meta cannot make the cache LIE about which scope it holds, and
		// a cache whose epoch is honest gets repaired by the resync this write
		// asks for. Whether the row belongs is the resync's decision, because
		// it is the only party that can make it.
		const U = null;
		const WS = 'ws-epoch-unconfirmed';
		const { persistReplace, persistUpserts, hydrate } = await loadPersistence();

		await persistReplace(U, WS, [row('keeper', 1)], '5', false, 'e2');

		const unconfirmed = await persistUpserts(U, WS, [row('secret', 2)], 'e1');
		expect(unconfirmed).toBe(true); // the ask for an authoritative resync

		const after = await hydrate(U, WS);
		expect(after.items.map((r) => r.id).sort()).toEqual(['keeper', 'secret']);
		// The half that matters: the cache still says what it actually holds.
		expect(after.accessEpoch).toBe('e2');
		expect(after.cursor).toBe('5');

		// CONTROL LEG. A confirmed writer asks for nothing — otherwise every
		// ordinary write would drag a full resync behind it.
		expect(await persistUpserts(U, WS, [row('allowed', 3)], 'e2')).toBe(false);
	});

	it('refuses an unconfirmed delta ENTIRELY, rows included', async () => {
		// THE THIRD REVISION OF THIS PROPERTY, and each one is on the trail.
		// Round 3 refused the rows and applied the removals. Round 4 refused
		// the removals and applied the rows. Round 5 refuses everything, and
		// this is the version with an argument rather than an intuition behind
		// it: a delta's rows and its CURSOR are ONE STATEMENT — "the cache is
		// current through here". Splitting them leaves a half nothing can
		// replay, because a later confirmed writer advances the durable cursor
		// past the withheld interval and no delta re-sends it. A withheld
		// removal leaves a revoked row forever; a withheld row is missing
		// forever. All-or-nothing is the only disposition that keeps the
		// statement true.
		const U = null;
		const WS = 'ws-epoch-unconfirmed-delta';
		const { persistReplace, persistDelta, hydrate } = await loadPersistence();

		await persistReplace(U, WS, [row('keeper', 1)], '20', false, 'e2');

		const unconfirmed = await persistDelta(U, WS, [row('added', 6)], '15', false, 'e1', [
			'keeper',
		]);
		expect(unconfirmed).toBe(true);

		const after = await hydrate(U, WS);
		// Nothing from that batch is in the cache: not the row, not the removal.
		expect(after.items.map((r) => r.id)).toEqual(['keeper']);
		expect(after.cursor).toBe('20');
		expect(after.accessEpoch).toBe('e2');

		// CONTROL LEG: a confirmed delta applies BOTH halves — its row and its
		// removal — and advances the cursor. Without this leg, a persistDelta
		// that refused every batch would pass everything above.
		expect(
			await persistDelta(U, WS, [row('added', 21)], '21', false, 'e2', ['keeper']),
		).toBe(false);
		const healthy = await hydrate(U, WS);
		expect(healthy.items.map((r) => r.id)).toEqual(['added']);
		expect(healthy.cursor).toBe('21');
	});

	it('refuses an unconfirmed REPLACE outright, because a replace claims the whole scope', async () => {
		// The one writer whose rows are also refused, and for a reason that is
		// not about staleness: a replace says "these are the rows, everything
		// else is gone, and this is the scope". None of that is separable, and
		// none of it is sayable by a writer whose epoch the cache does not
		// share.
		const U = null;
		const WS = 'ws-epoch-unconfirmed-replace';
		const { persistReplace, hydrate } = await loadPersistence();

		await persistReplace(U, WS, [row('newer', 9)], '9', false, 'e2');
		expect(await persistReplace(U, WS, [row('other', 1)], '1', false, 'e1')).toBe(true);

		const after = await hydrate(U, WS);
		expect(after.items.map((r) => r.id)).toEqual(['newer']);
		expect(after.accessEpoch).toBe('e2');
		expect(after.cursor).toBe('9');

		// CONTROL LEG: a confirmed replace still lands, or no resync could ever
		// install its snapshot.
		expect(await persistReplace(U, WS, [row('newest', 12)], '12', false, 'e2')).toBe(false);
		expect((await hydrate(U, WS)).items.map((r) => r.id)).toEqual(['newest']);
	});

	it('treats a cache with no epoch as nothing to contradict', async () => {
		// The exemption that has survived every round: a cache with no recorded
		// epoch has had no resync, so no writer can fail to match it. Refusing
		// here would drop the first write of every session.
		const U = null;
		const WS = 'ws-epoch-virgin';
		const { persistUpserts, hydrate } = await loadPersistence();

		expect(await persistUpserts(U, WS, [row('first', 1)], null)).toBe(false);
		expect(await persistUpserts(U, WS, [row('second', 2)], 'e1')).toBe(false);
		const after = await hydrate(U, WS);
		expect(after.items.map((r) => r.id).sort()).toEqual(['first', 'second']);
		expect(after.accessEpoch).toBeNull();
	});

	it('treats a writer with no epoch against a cache that has one as unconfirmed', async () => {
		// Null does not mean "safe"; it means the WRITER knows nothing about
		// scope. Against a cache that does know, that is exactly the case where
		// the server has to be asked.
		const U = null;
		const WS = 'ws-epoch-null-writer';
		const { persistReplace, persistUpserts, hydrate } = await loadPersistence();

		await persistReplace(U, WS, [row('keeper', 1)], '5', false, 'e2');
		expect(await persistUpserts(U, WS, [row('unclaimed', 4)], null)).toBe(true);
		expect((await hydrate(U, WS)).accessEpoch).toBe('e2');
	});
});
