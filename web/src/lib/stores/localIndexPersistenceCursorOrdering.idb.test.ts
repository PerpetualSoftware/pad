import { describe, it, expect } from 'vitest';
import type { ItemIndexRow } from '$lib/types';
import { loadPersistence, rawItem, rawItems } from '../../test/idbHarness';

/**
 * TASK-2906 (PLAN-2903 item 2) — the durable cursor never moves backward, and
 * a batch whose position is behind it is dropped whole.
 *
 * The cursor was the one value this cache wrote without ordering: rows already
 * arbitrate on `seq` through `resolveRowWrite` and tombstones through
 * `raiseTombstone`'s `Math.max`, while `persistDelta` and `persistReplace` each
 * `put` a freshly built meta row unconditionally. See TASK-2906 checkpoints 1
 * and 2 for the writer table and the two traps.
 *
 * These are sequential calls through the real module, which is how this file's
 * siblings pin the PLAN-2636 races: IDB serializes the transactions, so
 * ordering the calls IS the cross-tab interleaving.
 */

function row(id: string, seq: number): ItemIndexRow {
	return { id, seq, collection_slug: 'c' } as unknown as ItemIndexRow;
}

async function cursorOf(
	mod: Awaited<ReturnType<typeof loadPersistence>>,
	userId: string | null,
	ws: string,
): Promise<string> {
	return (await mod.hydrate(userId, ws)).cursor;
}

describe('TASK-2906 — the durable cursor never moves backward', () => {
	it('refuses a delta whose cursor is behind the stored one, cursor and rows together', async () => {
		const U = null;
		const WS = 'ws-2906-delta-behind';
		const mod = await loadPersistence();
		const { persistDelta } = mod;

		// The ahead tab.
		await persistDelta(U, WS, [row('a', 500)], '500', false, null);
		// The behind tab's late delta. Its row would be a fresh INSERT — the id
		// is not stored, so `resolveRowWrite` has no seq and no tombstone to
		// refuse it with, and only the cursor can say it is stale.
		await persistDelta(U, WS, [row('late', 300)], '300', false, null);

		expect(await cursorOf(mod, U, WS)).toBe('500');
		expect(await rawItem(U, WS, 'late')).toBeUndefined();
	});

	it('refuses a replace whose cursor is behind, keeping the ahead tab’s rows uncleared', async () => {
		const U = null;
		const WS = 'ws-2906-replace-behind';
		const mod = await loadPersistence();
		const { persistDelta, persistReplace } = mod;

		await persistDelta(U, WS, [row('a', 500), row('b', 500)], '500', false, null);
		// A stale snapshot that was computed before the ahead tab advanced and
		// landed after it. A replace CLEARS the items store, so letting it
		// through would drop `b` as well as regress the cursor.
		await persistReplace(U, WS, [row('a', 400)], '400', false, null);

		expect(await cursorOf(mod, U, WS)).toBe('500');
		const ids = (await rawItems(U, WS)).map((r) => r.id).sort();
		expect(ids).toEqual(['a', 'b']);
		expect((await rawItem(U, WS, 'a'))?.seq).toBe(500);
	});

	it('does not reinsert a row the ahead tab’s resync removed', async () => {
		const U = null;
		const WS = 'ws-2906-no-resurrect';
		const mod = await loadPersistence();
		const { persistDelta, persistReplace } = mod;

		// Both rows visible under the old scope.
		await persistDelta(U, WS, [row('keep', 250), row('revoked', 250)], '250', false, 'e1');
		// The ahead tab resyncs under a narrowed scope: `revoked` is absent from
		// the authoritative snapshot, and the replace also clears tombstones —
		// which is exactly why no tombstone is left to refuse the reinsert.
		await persistReplace(U, WS, [row('keep', 500)], '500', false, 'e2');
		expect(await rawItem(U, WS, 'revoked')).toBeUndefined();

		// The behind tab never learned about the narrowing and replays its own
		// delta carrying the revoked row.
		await persistDelta(U, WS, [row('revoked', 250)], '300', false, 'e1');

		expect(await rawItem(U, WS, 'revoked')).toBeUndefined();
		expect(await cursorOf(mod, U, WS)).toBe('500');
	});

	it('keeps the rows a regressed cursor would strand once the ahead tab advances again', async () => {
		const U = null;
		const WS = 'ws-2906-loss-path';
		const mod = await loadPersistence();
		const { persistDelta, persistReplace } = mod;

		// This is the case that makes a regression more than wasteful. A cursor
		// that is too LOW only costs a replay — but nothing keeps it low: the
		// ahead tab's next ordinary delta advances it past the gap, and then
		// nothing will ever replay the window the replace cleared.
		await persistDelta(U, WS, [row('a', 500), row('b', 500)], '500', false, null);
		await persistReplace(U, WS, [row('a', 400)], '400', false, null);
		await persistDelta(U, WS, [row('c', 501)], '501', false, null);

		expect(await cursorOf(mod, U, WS)).toBe('501');
		// Without the gate the cursor still reads 501 here — `b` is what tells
		// the two builds apart, so assert the row, not just the position.
		expect(await rawItem(U, WS, 'b')).toBeDefined();
		const ids = (await rawItems(U, WS)).map((r) => r.id).sort();
		expect(ids).toEqual(['a', 'b', 'c']);
	});

	it('leaves the OLD epoch on disk when it refuses a behind snapshot carrying a new one', async () => {
		const U = null;
		const WS = 'ws-2906-refusal-keeps-repair-signal';
		const mod = await loadPersistence();
		const { persistDelta, persistReplace } = mod;

		// A behind cursor settles POSITION, not SCOPE. A revocation writes no
		// item, so a snapshot can be strictly newer in scope while losing the
		// position race to an unrelated mutation in another tab (review round 1).
		await persistDelta(U, WS, [row('a', 500), row('revoked', 500)], '501', false, 'e1');
		await persistReplace(U, WS, [row('a', 500)], '500', false, 'e2');

		const after = await mod.hydrate(U, WS);
		// The refusal has to be WHOLE. Writing the epoch while withholding the
		// cursor would look like the kinder refusal and is the one thing that
		// breaks the repair: a durable `e2` agrees with the server, so no
		// hydrate and no ahead tab would ever notice the revoked row again.
		expect(after.accessEpoch).toBe('e1');
		expect(after.cursor).toBe('501');
		expect(await rawItem(U, WS, 'revoked')).toBeDefined();
	});

	it('accepts a batch at the SAME cursor — equal is not behind', async () => {
		const U = null;
		const WS = 'ws-2906-equal';
		const mod = await loadPersistence();
		const { persistDelta } = mod;

		await persistDelta(U, WS, [row('a', 500)], '500', false, null);
		// A second tab restating the same position with a row the first lacked
		// (a cold snapshot is exactly this shape). Refusing it would drop rows
		// for no ordering reason, so the gate is strictly-behind, not
		// at-or-behind.
		await persistDelta(U, WS, [row('b', 500)], '500', false, null);

		expect(await rawItem(U, WS, 'b')).toBeDefined();
		expect(await cursorOf(mod, U, WS)).toBe('500');
	});

	it('orders cursors that differ only above 2^53, where Number ties', async () => {
		const U = null;
		const WS = 'ws-2906-precision';
		const mod = await loadPersistence();
		const { persistDelta } = mod;

		// Both of these are the same IEEE double. Comparing through `Number`
		// makes them tie, and a tie reads as "not behind" — the direction that
		// lets a stale batch through (review round 2).
		await persistDelta(U, WS, [row('a', 1)], '9007199254740993', false, null);
		await persistDelta(U, WS, [row('behind', 2)], '9007199254740992', false, null);

		expect(await cursorOf(mod, U, WS)).toBe('9007199254740993');
		expect(await rawItem(U, WS, 'behind')).toBeUndefined();
	});

	it('reports whether the snapshot reached disk, so the caller can tell a refusal from a commit', async () => {
		const U = null;
		const WS = 'ws-2906-replace-reports';
		const mod = await loadPersistence();
		const { persistDelta, persistReplace } = mod;

		// The return value is what stops `ensureAccessScope` stamping a new
		// access epoch onto a rowset that was never replaced.
		expect(await persistReplace(U, WS, [row('a', 1)], '10', false, 'e1')).toBe(true);
		await persistDelta(U, WS, [row('b', 20)], '20', false, 'e1');
		expect(await persistReplace(U, WS, [row('a', 1)], '15', false, 'e2')).toBe(false);
	});

	it('accepts the first write, when there is no stored cursor to be behind', async () => {
		const U = null;
		const WS = 'ws-2906-first-write';
		const mod = await loadPersistence();
		const { persistReplace } = mod;

		// An absent meta row is not a cursor of 0 that everything ties with —
		// it is no claim at all, and the first write must land.
		await persistReplace(U, WS, [row('a', 7)], '7', false, null);

		expect(await cursorOf(mod, U, WS)).toBe('7');
		expect(await rawItem(U, WS, 'a')).toBeDefined();
	});
});
