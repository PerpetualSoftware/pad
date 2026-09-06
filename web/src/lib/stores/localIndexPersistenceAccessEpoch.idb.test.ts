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

		// SECOND LEG — CHANGED IN ROUND 3, because the PROPERTY changed.
		//
		// This used to assert that a writer with no baseline is let through
		// against any cache, on the reading that null means "no claim". Round 3
		// showed that is the way IN: a delayed callback from before this tab had
		// a baseline, or a fresh state after a `reset()`, could insert into a
		// cache that had already resynced past it. Null does not mean the write
		// is safe; it means the WRITER knows nothing about scope, which is a
		// reason to refuse it against a cache that does know.
		await persistUpserts(U, WS, [row('unclaimed', 4)], null);
		expect((await rawItems(U, WS)).map((r) => r.id).sort()).toEqual(['allowed', 'keeper']);
	});

	it('refuses a slower tab\'s older snapshot rather than clobbering a newer one', async () => {
		// Two tabs can resync at once. If tab A started under an older scope and
		// lands second, an unfenced `persistReplace` clears the store and writes
		// its own rows and epoch over tab B's newer ones — transient rather than
		// permanently inert, since a later delta contradicts the regressed
		// epoch, but that is a full round trip and a window of visibly wrong
		// rows for no gain.
		//
		// Unlike the delta fence there is nothing to salvage from a refused
		// replace: its removals are expressed as "everything not in this
		// snapshot", a claim about a scope that has already been superseded.
		const U = null;
		const WS = 'ws-replace-fence';
		const { persistReplace, hydrate } = await loadPersistence();

		await persistReplace(U, WS, [row('newer', 9)], '9', false, 'e2');
		await persistReplace(U, WS, [row('older', 1)], '1', false, 'e1');

		const after = await hydrate(U, WS);
		expect(after.items.map((r) => r.id)).toEqual(['newer']);
		expect(after.accessEpoch).toBe('e2');
		expect(after.cursor).toBe('9');

		// CONTROL LEG: a replace under the CURRENT epoch must still land, or
		// every resync after the first would be a no-op.
		await persistReplace(U, WS, [row('newest', 12)], '12', false, 'e2');
		const healthy = await hydrate(U, WS);
		expect(healthy.items.map((r) => r.id)).toEqual(['newest']);
		expect(healthy.cursor).toBe('12');
	});

	it('lets an unclaimed writer through only when the CACHE has no epoch either', async () => {
		// The exemption that is legitimate, and the reason the fence cannot
		// simply refuse every null writer: a cache with no recorded epoch has
		// had no resync, so nothing can be stale relative to it. Refusing here
		// would drop the first write of every session.
		const U = null;
		const WS = 'ws-epoch-fence-virgin';
		const { persistUpserts, hydrate } = await loadPersistence();

		await persistUpserts(U, WS, [row('first', 1)], null);
		const after = await hydrate(U, WS);
		expect(after.items.map((r) => r.id)).toEqual(['first']);
		expect(after.accessEpoch).toBeNull();
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

		// A FENCE NEVER BLOCKS A REMOVAL (round 3). The stale batch's row write
		// is refused above; its REMOVALS must still apply, because a removal
		// can only narrow what the cache asserts and no later delta re-sends a
		// removal for a sequence already behind the persisted cursor. Round 2's
		// version refused the whole transaction and so caused the exact
		// permanent state the fence exists to prevent.
		await persistDelta(U, WS, [], '6', false, 'e1', ['keeper']);
		const afterRemoval = await hydrate(U, WS);
		expect(afterRemoval.items.map((r) => r.id)).toEqual([]);
		// ...and the refused batch still may not advance the cursor or the
		// epoch, which is the half that must NOT leak through with it.
		expect(afterRemoval.cursor).toBe('5');
		expect(afterRemoval.accessEpoch).toBe('e2');

		// CONTROL LEG. A current-epoch delta must still land, or every
		// authoritative write after a resync would be silently dropped.
		await persistDelta(U, WS, [row('fresh', 7)], '7', false, 'e2');
		const healthy = await hydrate(U, WS);
		// 'keeper' is gone because the refused batch's REMOVAL was applied — the
		// leg above. Only 'fresh' remains, which is what a current-epoch write
		// landing proves.
		expect(healthy.items.map((r) => r.id)).toEqual(['fresh']);
		expect(healthy.cursor).toBe('7');
	});
});
