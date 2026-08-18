import { describe, it, expect } from 'vitest';
import type { ItemIndexRow } from '$lib/types';
import {
	loadPersistence,
	rawItem,
	rawItems,
	rawTombstone,
	rawTombstones,
	rawRetags,
	seedV1Database,
} from '../../test/idbHarness';

/**
 * PLAN-2636 unit 2 — the cache's order-and-merge contract, end-to-end through a
 * real (fake-indexeddb) database. The pure decision is pinned by
 * itemRowMerge.test.ts; this file pins the TRANSACTIONAL outcomes the contract
 * on the PLAN-2636 trail specifies:
 *
 *   - BUG-2633: tombstones for hard-deletes — a stale snapshot can't resurrect
 *     an evicted id behind the cursor; a genuinely newer write supersedes.
 *   - BUG-2634: durable retag overlay — a rename survives a racing older-slug
 *     delta and a reload, reapplied at hydrate.
 *   - BUG-2635: equal-seq merge — a second tab's unmerged snapshot can't clobber
 *     a merged row's projection at the same seq.
 *
 * Per the lead's ruling these are SNAPSHOT-staleness races: plain SEQUENTIAL
 * calls through the module reproduce them (IDB serializes the transactions), so
 * there is no interleaving hook.
 */

function row(
	id: string,
	seq: number | undefined,
	extra: Record<string, unknown> = {},
): ItemIndexRow {
	return { id, seq, ...extra } as unknown as ItemIndexRow;
}

describe('BUG-2633 — tombstones stop a stale snapshot resurrecting an evicted row', () => {
	it('an eviction tombstones the id; a later stale snapshot is refused and hydrate agrees', async () => {
		const { persistUpserts, persistDelta, hydrate } = await loadPersistence();
		const U = null;
		const WS = 'ws-2633-evict';

		await persistUpserts(U, WS, [row('x', 5)]);
		// A delta evicts x (moved-out) and advances the cursor to 10 — atomic.
		await persistDelta(U, WS, [], '10', false, ['x']);
		// The stale RAM snapshot for x (seq 5, behind the cursor) lands last.
		await persistUpserts(U, WS, [row('x', 5)]);

		expect(await rawItem(U, WS, 'x')).toBeUndefined();
		expect((await hydrate(U, WS)).items.find((i) => i.id === 'x')).toBeUndefined();
		// The tombstone is stamped with the evicting delta's cursor.
		expect((await rawTombstone(U, WS, 'x'))?.deletedAtSeq).toBe(10);
	});

	it('a seq-less stale snapshot cannot resurrect a tombstoned id', async () => {
		const { persistUpserts, persistDelta } = await loadPersistence();
		const U = null;
		const WS = 'ws-2633-seqless';

		await persistUpserts(U, WS, [row('x', 5)]);
		await persistDelta(U, WS, [], '10', false, ['x']);
		await persistUpserts(U, WS, [row('x', undefined)]); // optimistic seq-less snapshot

		expect(await rawItem(U, WS, 'x')).toBeUndefined();
	});

	it('a genuinely newer write supersedes the tombstone AND clears it', async () => {
		const { persistUpserts, persistDelta } = await loadPersistence();
		const U = null;
		const WS = 'ws-2633-supersede';

		await persistUpserts(U, WS, [row('x', 5)]);
		await persistDelta(U, WS, [], '10', false, ['x']); // tombstone x @ 10
		await persistUpserts(U, WS, [row('x', 11)]); // newer than the tombstone

		expect((await rawItem(U, WS, 'x'))?.seq).toBe(11);
		expect(await rawTombstone(U, WS, 'x')).toBeUndefined(); // supersession cleared it
	});

	it('403-purge (persistRemovals) tombstones with the PERSISTED cursor', async () => {
		const { persistDelta, persistRemovals, persistUpserts } = await loadPersistence();
		const U = null;
		const WS = 'ws-2633-purge';

		// A prior sync established rows + a persisted cursor of 20.
		await persistDelta(U, WS, [row('y', 15)], '20', false);
		await persistRemovals(U, WS, ['y']); // no cursor in hand → reads meta.sync (20)

		expect((await rawTombstone(U, WS, 'y'))?.deletedAtSeq).toBe(20);
		// An in-flight stale upsert behind that cursor cannot bring y back.
		await persistUpserts(U, WS, [row('y', 15)]);
		expect(await rawItem(U, WS, 'y')).toBeUndefined();
	});

	it('tombstones an upsert-then-removed row even before any sync (codex F1)', async () => {
		const { persistUpserts, persistRemovals, hydrate } = await loadPersistence();
		const U = null;
		const WS = 'ws-2633-presync';

		// A row is optimistically upserted before any sync writes meta.sync, then
		// removed. With no persisted cursor the tombstone stamps the row's own seq.
		await persistUpserts(U, WS, [row('z', 5)]);
		await persistRemovals(U, WS, ['z']);
		expect((await rawTombstone(U, WS, 'z'))?.deletedAtSeq).toBe(5);

		// A delayed stale snapshot of the same row cannot bring it back...
		await persistUpserts(U, WS, [row('z', 5)]);
		expect(await rawItem(U, WS, 'z')).toBeUndefined();
		expect((await hydrate(U, WS)).items.find((i) => i.id === 'z')).toBeUndefined();

		// ...but a genuinely newer create still supersedes the tombstone.
		await persistUpserts(U, WS, [row('z', 9)]);
		expect((await rawItem(U, WS, 'z'))?.seq).toBe(9);
	});

	it('a lower-cursor eviction does not weaken a higher tombstone stamp (codex F4)', async () => {
		const { persistUpserts, persistDelta } = await loadPersistence();
		const U = null;
		const WS = 'ws-2633-f4';

		await persistUpserts(U, WS, [row('x', 5)]);
		await persistDelta(U, WS, [], '20', false, ['x']); // tombstone x @ 20
		// An out-of-order lower-cursor eviction for the same id must not lower it.
		await persistDelta(U, WS, [], '10', false, ['x']);
		expect((await rawTombstone(U, WS, 'x'))?.deletedAtSeq).toBe(20);

		// A snapshot at a seq between the two stamps is still refused.
		await persistUpserts(U, WS, [row('x', 15)]);
		expect(await rawItem(U, WS, 'x')).toBeUndefined();
	});
});

describe('BUG-2634 — durable retag overlay survives a racing delta and a reload', () => {
	function collRow(id: string, seq: number, collectionId: string, slug: string): ItemIndexRow {
		return { id, seq, collection_id: collectionId, collection_slug: slug } as unknown as ItemIndexRow;
	}

	it('a rename is reapplied at hydrate even after a racing older-slug delta wins the stored row', async () => {
		const { persistUpserts, persistRetag, persistDelta, hydrate } = await loadPersistence();
		const U = null;
		const WS = 'ws-2634-race';

		await persistUpserts(U, WS, [collRow('a1', 1, 'coll-a', 'old-a')]);
		// Collection renamed old-a → new-a: rows rewritten in place + overlay persisted.
		await persistRetag(U, WS, 'coll-a', ['a1'], 'new-a');
		expect((await rawItem(U, WS, 'a1'))?.collection_slug).toBe('new-a');
		expect(await rawRetags(U, WS)).toEqual({ 'coll-a': 'new-a' });

		// A delta captured BEFORE the rename commits AFTER it, at a newer seq,
		// carrying the dead slug — no seq compare can arbitrate an out-of-band field.
		await persistDelta(U, WS, [collRow('a1', 2, 'coll-a', 'old-a')], '2', false);
		expect((await rawItem(U, WS, 'a1'))?.collection_slug).toBe('old-a'); // stored row regressed...

		// ...but hydrate reapplies the overlay, so the reader sees the rename.
		const h = await hydrate(U, WS);
		expect(h.items.find((i) => i.id === 'a1')?.collection_slug).toBe('new-a');
		expect(h.retags).toEqual({ 'coll-a': 'new-a' });
	});

	it('the overlay does not chase a row that has since MOVED out of the renamed collection (codex F5)', async () => {
		const { persistUpserts, persistRetag, persistDelta, hydrate } = await loadPersistence();
		const U = null;
		const WS = 'ws-2634-moved';

		await persistUpserts(U, WS, [collRow('a1', 1, 'coll-a', 'old-a')]);
		// Rename coll-a → new-a: a1 rewritten in place + overlay persisted.
		await persistRetag(U, WS, 'coll-a', ['a1'], 'new-a');
		// a1 then genuinely MOVES to coll-b via an authoritative delta at a newer seq.
		await persistDelta(U, WS, [collRow('a1', 2, 'coll-b', 'b-slug')], '2', false);
		expect((await rawItem(U, WS, 'a1'))?.collection_id).toBe('coll-b');

		// Membership is by collection_id: the coll-a overlay must NOT rewrite a1's
		// slug now that a1 lives in coll-b. (This discriminates the membership rule —
		// a blanket overlay-apply would wrongly stamp a1 with new-a.)
		const h = await hydrate(U, WS);
		const a1 = h.items.find((i) => i.id === 'a1');
		expect(a1?.collection_id).toBe('coll-b');
		expect(a1?.collection_slug).toBe('b-slug');
	});

	it('latest rename per collection wins the overlay', async () => {
		const { persistUpserts, persistRetag } = await loadPersistence();
		const U = null;
		const WS = 'ws-2634-latest';

		await persistUpserts(U, WS, [collRow('a1', 1, 'coll-a', 'v0')]);
		await persistRetag(U, WS, 'coll-a', ['a1'], 'v1');
		await persistRetag(U, WS, 'coll-a', ['a1'], 'v2');
		expect(await rawRetags(U, WS)).toEqual({ 'coll-a': 'v2' });
	});

	it('persistReplace drops the overlay (authoritative snapshot carries live slugs)', async () => {
		const { persistUpserts, persistRetag, persistReplace, hydrate } = await loadPersistence();
		const U = null;
		const WS = 'ws-2634-replace';

		await persistUpserts(U, WS, [collRow('a1', 1, 'coll-a', 'old-a')]);
		await persistRetag(U, WS, 'coll-a', ['a1'], 'new-a');
		expect(await rawRetags(U, WS)).toEqual({ 'coll-a': 'new-a' });

		// A projection resync installs an authoritative snapshot (live slugs).
		await persistReplace(U, WS, [collRow('a1', 3, 'coll-a', 'authoritative')], '3', true);
		expect(await rawRetags(U, WS)).toBeUndefined();
		// Hydrate no longer rewrites the slug — the overlay is gone.
		expect((await hydrate(U, WS)).items.find((i) => i.id === 'a1')?.collection_slug).toBe(
			'authoritative',
		);
	});
});

describe('BUG-2635 — equal-seq merge keeps a merged projection cross-tab', () => {
	it('an unmerged equal-seq snapshot does NOT clobber a merged rows projection bit', async () => {
		const { persistUpserts } = await loadPersistence();
		const U = null;
		const WS = 'ws-2635-merge';

		// Tab B has the merged row: is_unparented projected in at seq 7.
		await persistUpserts(U, WS, [row('x', 7, { is_unparented: true })]);
		// Tab A holds a stale RAM snapshot at the SAME seq that never merged the
		// projection, and persists it. Under the old blind-accept-at-equal-seq it
		// would drop is_unparented; the merge refuses it.
		await persistUpserts(U, WS, [row('x', 7)]);

		expect((await rawItem(U, WS, 'x'))?.is_unparented).toBe(true);
	});

	it('a newer-seq write that omits the projection preserves the stored bit', async () => {
		const { persistUpserts } = await loadPersistence();
		const U = null;
		const WS = 'ws-2635-preserve';

		await persistUpserts(U, WS, [row('x', 5, { is_unparented: true })]);
		await persistUpserts(U, WS, [row('x', 8)]); // mutation snapshot omits the projection

		const stored = await rawItem(U, WS, 'x');
		expect(stored?.seq).toBe(8);
		expect(stored?.is_unparented).toBe(true);
	});
});

describe('persistReplace clears the tombstone store too', () => {
	it('an authoritative snapshot supersedes every tombstone', async () => {
		const { persistUpserts, persistDelta, persistReplace } = await loadPersistence();
		const U = null;
		const WS = 'ws-replace-tombstones';

		await persistUpserts(U, WS, [row('x', 5)]);
		await persistDelta(U, WS, [], '10', false, ['x']); // tombstone x
		expect(await rawTombstones(U, WS)).toHaveLength(1);

		await persistReplace(U, WS, [row('y', 1)], '10', false);
		expect(await rawTombstones(U, WS)).toHaveLength(0);
	});
});

describe('v1 → v2 migration through the real module', () => {
	it('a v1 database migrates on the module open, retaining items and starting tombstones empty', async () => {
		const U = null;
		const WS = 'ws-migrate-module';
		// Seed a pre-unit-2 (format v1) database with an item + meta.
		await seedV1Database(U, WS, [row('keep', 3)], {
			cursor: '3',
			schemaVersion: 3,
			includesUnparentedMetadata: true,
		});

		// The module opens at IDB_FORMAT_VERSION (2) → upgrade runs, tombstones
		// store is created, existing data retained.
		const { persistUpserts, hydrate } = await loadPersistence();
		await persistUpserts(U, WS, [row('fresh', 4)]);

		expect((await rawItem(U, WS, 'keep'))?.seq).toBe(3); // survived the format bump
		expect(await rawTombstones(U, WS)).toHaveLength(0); // new store, empty
		const items = (await hydrate(U, WS)).items;
		expect(items.map((i) => i.id).sort()).toEqual(['fresh', 'keep']);
	});
});

// Guard the vacuous-read failure mode for the new stores: assert the module
// actually writes what the raw readers claim to read, under the module's own
// namespace, not an empty sibling.
describe('raw readers address the module database', () => {
	it('a persisted tombstone + retag are readable back under the module name', async () => {
		const { persistUpserts, persistDelta, persistRetag } = await loadPersistence();
		const U = 'user-9';
		const WS = 'Name With Spaces';

		await persistUpserts(U, WS, [
			{ id: 'r', seq: 1, collection_id: 'c', collection_slug: 's' } as unknown as ItemIndexRow,
		]);
		await persistRetag(U, WS, 'c', ['r'], 's2');
		await persistDelta(U, WS, [], '9', false, ['r']);

		expect(await rawRetags(U, WS)).toEqual({ c: 's2' });
		expect((await rawTombstone(U, WS, 'r'))?.deletedAtSeq).toBe(9);
		expect(await rawItems(U, WS)).toHaveLength(0);
	});
});
