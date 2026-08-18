import { describe, it, expect } from 'vitest';
import type { ItemIndexRow } from '$lib/types';
import { loadPersistence, rawItem, openSecondConnection, harnessDbName } from '../../test/idbHarness';

/**
 * PLAN-2636 unit 1 — end-to-end IDB coverage for the localIndex persistence
 * layer, which ran as a no-op under vitest until this project existed (no
 * IndexedDB in the node env — Wren's #1148 finding). The sibling
 * `localIndexPersistence.test.ts` pins the PURE decision (`shouldWriteRow`)
 * and explicitly says it does NOT cover the interleaving outcome. This file
 * covers exactly that outcome through a real (fake-indexeddb) database.
 *
 * The ruling on the PLAN-2636 trail: the family's races are SNAPSHOT-
 * staleness, not read-staleness — the stale thing is the RAM snapshot handed
 * to persistUpserts, never its `get`, which reads committed state. IDB
 * serializes overlapping readwrite transactions, so the race BUG-2609 fixed
 * reproduces with plain SEQUENTIAL calls: a newer delta commits its atomic
 * rows+cursor transaction, then the older fire-and-forget snapshot lands last.
 * No interleaving hook — none is needed, and an awaited barrier between a get
 * and a put would fail on a real browser (TransactionInactiveError) anyway.
 */

function row(id: string, seq: number | undefined, extra: Record<string, unknown> = {}): ItemIndexRow {
	return { id, seq, ...extra } as unknown as ItemIndexRow;
}

describe('BUG-2609 end-to-end: stale snapshot does not clobber a newer persisted row', () => {
	it('a delayed older-seq upsert landing after a newer delta is refused, and hydrate agrees', async () => {
		const { persistDelta, persistUpserts, hydrate } = await loadPersistence();
		const U = null;
		const WS = 'ws-2609';

		// 1) The SSE delta lands first — newer-seq row + cursor advance, atomic.
		await persistDelta(U, WS, [row('x', 7)], 'cursor-7', false);
		// 2) The stale RAM snapshot (older seq) lands LAST — the fire-and-forget
		//    upsert that BUG-2609's evidence run captured mid-flight.
		await persistUpserts(U, WS, [row('x', 5)]);

		// The seq guard refused the stale write: IDB still holds seq 7...
		expect((await rawItem(U, WS, 'x'))?.seq).toBe(7);
		// ...and a warm hydrate reflects that, with the cursor still past the delta.
		const h = await hydrate(U, WS);
		expect(h.items.find((i) => i.id === 'x')?.seq).toBe(7);
		expect(h.cursor).toBe('cursor-7');
	});

	it('a seq-less optimistic snapshot does not overwrite a stamped persisted row', async () => {
		// The asymmetry leg, end-to-end: the optimistic-reorder path clears seq
		// (TASK-1357); persisting it must not strand the cache with an
		// unorderable row while the cursor sits past the correcting delta.
		const { persistDelta, persistUpserts, hydrate } = await loadPersistence();
		const U = null;
		const WS = 'ws-2609-asym';

		await persistDelta(U, WS, [row('x', 7)], 'cursor-7', false);
		await persistUpserts(U, WS, [row('x', undefined)]); // seq-less snapshot lands last

		expect((await rawItem(U, WS, 'x'))?.seq).toBe(7);
		expect((await hydrate(U, WS)).items.find((i) => i.id === 'x')?.seq).toBe(7);
	});

	it('a genuinely newer upsert still wins (the guard refuses only STALE writes)', async () => {
		const { persistUpserts, hydrate } = await loadPersistence();
		const U = null;
		const WS = 'ws-2609-fwd';

		await persistUpserts(U, WS, [row('x', 5)]);
		await persistUpserts(U, WS, [row('x', 8)]); // newer — must land

		expect((await rawItem(U, WS, 'x'))?.seq).toBe(8);
		expect((await hydrate(U, WS)).items.find((i) => i.id === 'x')?.seq).toBe(8);
	});
});

describe('IDB transaction serialization (platform contract this layer leans on)', () => {
	// Optional characterization the lead invited: the persistence write path's
	// correctness rests on IDB serializing overlapping readwrite transactions,
	// so the get inside persistUpserts always reads committed state. This pins
	// the guarantee at the raw-store level — a fake-indexeddb regression that
	// broke it would surface here rather than as a mysterious flake upstream.
	it('two overlapping readwrite transactions on the same store do not interleave', async () => {
		const db = await openSecondConnection(null, 'ws-serialize');
		try {
			const order: string[] = [];

			// Tx A: read, then (after a microtask) write — started first.
			const txA = db.transaction('items', 'readwrite');
			const pA = (async () => {
				await txA.objectStore('items').get('k');
				order.push('A:read');
				txA.objectStore('items').put({ id: 'k', seq: 1, who: 'A' });
				order.push('A:write');
				await txA.done;
				order.push('A:done');
			})();

			// Tx B: started while A is open; must not commit before A finishes.
			const txB = db.transaction('items', 'readwrite');
			const pB = (async () => {
				const seen = (await txB.objectStore('items').get('k')) as { who?: string } | undefined;
				order.push(`B:read(${seen?.who ?? 'none'})`);
				txB.objectStore('items').put({ id: 'k', seq: 2, who: 'B' });
				await txB.done;
				order.push('B:done');
			})();

			await Promise.all([pA, pB]);

			// A's transaction commits entirely before B's read runs — B sees A's
			// committed write, never an interleaved half-state. (The exact order
			// of the two read/write log lines within A is not asserted; the
			// contract is that B:read observes A's value and B:done follows
			// A:done.)
			expect(order.indexOf('A:done')).toBeLessThan(order.indexOf('B:done'));
			expect(order).toContain('B:read(A)');
			const final = (await rawViaDb(db)) ?? {};
			expect((final as { who?: string }).who).toBe('B');
		} finally {
			db.close();
		}
	});
});

async function rawViaDb(db: Awaited<ReturnType<typeof openSecondConnection>>): Promise<unknown> {
	return db.get('items', 'k');
}

describe('database naming', () => {
	it('the harness addresses the exact database the module writes to', async () => {
		// Guards the vacuous-test failure mode: if harnessDbName drifted from
		// the module's dbName, rawItem would read an empty sibling DB and every
		// assertion above would pass for the wrong reason. Persist through the
		// module, then read the raw store under the harness name directly.
		const { persistUpserts } = await loadPersistence();
		await persistUpserts('user-42', 'My WS', [row('y', 1)]);

		const dbNameShouldBe = harnessDbName('user-42', 'My WS');
		expect(dbNameShouldBe).toBe(
			`pad-local-index-${encodeURIComponent('user-42')}-${encodeURIComponent('My WS')}`,
		);
		expect((await rawItem('user-42', 'My WS', 'y'))?.seq).toBe(1);
	});
});
