import { describe, it, expect } from 'vitest';
import type { ItemIndexRow } from '$lib/types';
import { loadPersistence, rawItems } from '../../test/idbHarness';

/**
 * TASK-2922 / PLAN-2903 item 1 — F2: a cache that holds old-scope rows while
 * advertising the new epoch.
 *
 * THE ROUTE, and the ordering is the whole thing. A write that commits BEFORE
 * the resync is superseded by `persistReplace`'s clear and needs no fence (the
 * last leg here pins that). The hazard is the write that lands AFTER: a tab
 * that has not learned about the scope change has none of the RAM-local guards
 * that would stop it — `scopeEpoch` never bumped there, `fencedIds` is
 * recomputed only by a resync, `movedOutFloor` saw no eviction — so the row
 * reaches `persistUpserts`, and both the stored row and its tombstone were just
 * cleared by the replace, so `resolveRowWrite` sees an unopposed insert.
 *
 * WHY IT DOES NOT SELF-HEAL, which is what the earlier acceptance rested on.
 * The repair belongs to the tab that made the stale write — its own next
 * `/items-changes` carries the epoch it does not hold, and it resyncs. A tab
 * that goes away after its write commits takes the repair with it, and
 * `persistUpserts` writes no meta row, so the epoch on disk stays at the value
 * the RESYNC stamped. Every later reader — every poll, every cold boot — then
 * compares equal and re-checks nothing.
 *
 * These are sequential calls through the real module. IDB serializes the
 * transactions, so ordering the calls IS the interleaving (see idbHarness).
 */

function row(id: string, seq: number): ItemIndexRow {
	return { id, seq, collection_slug: 'c' } as unknown as ItemIndexRow;
}

describe('TASK-2922 — a tab may only write under the scope the cache advertises', () => {
	it('refuses a behind tab reinserting a row the resync dropped', async () => {
		const U = null;
		const WS = 'ws-2922-behind';
		const { persistDelta, persistReplace, persistUpserts, hydrate } = await loadPersistence();

		// Both tabs share a cache holding `secret` under epoch e1.
		await persistDelta(U, WS, [row('secret', 10), row('ok', 11)], '11', false, 'e1');

		// Tab A learns the scope narrowed and resyncs. Its snapshot omits
		// `secret`, and it stamps e2 on the meta row.
		expect(await persistReplace(U, WS, [row('ok', 11)], '12', false, 'e2')).toBe(true);

		// Tab B never learned about e2 and still believes e1.
		await persistUpserts(U, WS, [row('secret', 10)], 'e1');

		// Asserted on the RAW store as well as through `hydrate`: a hydrate-only
		// assertion would also pass if the row were stored and merely filtered
		// on read, which is a different (and non-existent) mechanism.
		expect((await rawItems(U, WS)).map((r) => r.id).sort()).toEqual(['ok']);
		const after = await hydrate(U, WS);
		expect(after.items.map((r) => r.id)).toEqual(['ok']);
		// And the epoch is untouched — the refusal must not also cost the cache
		// the scope statement the resync correctly recorded.
		expect(after.accessEpoch).toBe('e2');
		expect(after.cursor).toBe('12');
	});

	it('CONTROL — the CURRENT tab writes normally through the same door', async () => {
		const U = null;
		const WS = 'ws-2922-current';
		const { persistDelta, persistReplace, persistUpserts, hydrate } = await loadPersistence();

		await persistDelta(U, WS, [row('ok', 11)], '11', false, 'e1');
		await persistReplace(U, WS, [row('ok', 11)], '12', false, 'e2');

		// Same call, same sequence, same door — only the writer's belief differs.
		// Without this leg the assertion above would also pass against a
		// `persistUpserts` that had simply stopped writing anything.
		await persistUpserts(U, WS, [row('fresh', 13)], 'e2');

		expect((await hydrate(U, WS)).items.map((r) => r.id).sort()).toEqual(['fresh', 'ok']);
	});

	it('refuses the WHOLE batch, not the contradicting row', async () => {
		const U = null;
		const WS = 'ws-2922-whole';
		const { persistDelta, persistReplace, persistUpserts, hydrate } = await loadPersistence();

		await persistDelta(U, WS, [row('secret', 10), row('ok', 11)], '11', false, 'e1');
		await persistReplace(U, WS, [row('ok', 11)], '12', false, 'e2');

		// The batch is one claim, made under one scope. Splitting it — writing
		// the rows that happen to be innocuous and dropping the rest — would
		// require deciding per row which scope authorised it, which is exactly
		// what the writer cannot tell us and why it names its scope once.
		await persistUpserts(U, WS, [row('secret', 10), row('unrelated', 13)], 'e1');

		expect((await hydrate(U, WS)).items.map((r) => r.id)).toEqual(['ok']);
	});

	it('an ordinary delta restores the durable copy a refusal deferred', async () => {
		const U = null;
		const WS = 'ws-2922-restore';
		const { persistDelta, persistReplace, persistUpserts, hydrate } = await loadPersistence();

		await persistDelta(U, WS, [row('ok', 11)], '11', false, 'e1');
		await persistReplace(U, WS, [row('ok', 11)], '12', false, 'e2');

		// A legitimate mutation in the behind tab — still in scope, so the
		// snapshot would carry it — but the tab's belief is stale, so the write
		// is refused.
		await persistUpserts(U, WS, [row('mine', 13)], 'e1');
		expect((await hydrate(U, WS)).items.map((r) => r.id)).toEqual(['ok']);

		// The row was SERVER TRUTH, and this door never advances the cursor, so
		// the mutation is still inside the next delta's window. This leg is what
		// makes "deferred, not lost" a measured claim rather than an assumption
		// about scheduling: it does not depend on WHEN the tab reconciles, only
		// on the row being re-supplied when it does (codex round 2 P1).
		await persistDelta(U, WS, [row('mine', 13)], '13', false, 'e2');

		expect((await hydrate(U, WS)).items.map((r) => r.id).sort()).toEqual(['mine', 'ok']);
	});

	it('writes normally when the cache carries no scope claim at all', async () => {
		const U = null;
		const WS = 'ws-2922-nometa';
		const { persistUpserts, hydrate } = await loadPersistence();

		// No meta row: nothing on disk for the batch to contradict. Minting one
		// here would invent a cursor, which is the claim that everything up to it
		// has been seen — the same reason `persistAccessEpoch` declines.
		await persistUpserts(U, WS, [row('a', 1)], 'e1');

		expect((await hydrate(U, WS)).items.map((r) => r.id)).toEqual(['a']);
	});

	it('treats null on both sides as a match — a server without access_epoch still writes', async () => {
		const U = null;
		const WS = 'ws-2922-null';
		const { persistDelta, persistUpserts, hydrate } = await loadPersistence();

		// A server that predates `access_epoch` persists a null baseline. `null`
		// is a real value on both sides here, not an absence, so the two agree
		// and this deployment is unaffected by the fence.
		await persistDelta(U, WS, [row('a', 1)], '1', false, null);
		await persistUpserts(U, WS, [row('b', 2)], null);

		expect((await hydrate(U, WS)).items.map((r) => r.id).sort()).toEqual(['a', 'b']);
	});

	it('a stale write that lands BEFORE the resync is dropped by the replace, not by this fence', async () => {
		const U = null;
		const WS = 'ws-2922-ordering';
		const { persistDelta, persistReplace, persistUpserts, hydrate } = await loadPersistence();

		await persistDelta(U, WS, [row('ok', 11)], '11', false, 'e1');
		// Tab B's stale write commits while the cache still advertises e1, so
		// the fence has nothing to object to and lets it through...
		await persistUpserts(U, WS, [row('secret', 10)], 'e1');
		expect((await hydrate(U, WS)).items.map((r) => r.id).sort()).toEqual(['ok', 'secret']);

		// ...and the resync's clear is what removes it. This leg exists so the
		// fence's scope is explicit: it closes the window AFTER a replace, and
		// the window before one was never open.
		await persistReplace(U, WS, [row('ok', 11)], '12', false, 'e2');
		expect((await hydrate(U, WS)).items.map((r) => r.id)).toEqual(['ok']);
	});
});
