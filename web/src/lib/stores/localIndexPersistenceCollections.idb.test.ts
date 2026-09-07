import { describe, it, expect } from 'vitest';
import type { Collection, ItemIndexRow } from '$lib/types';
import { loadPersistence } from '../../test/idbHarness';

/**
 * TASK-2946 — the cached collection list, and the fence that decides whether it
 * may be rendered.
 *
 * The board's rows already survive an offline cold load: they hydrate from IDB
 * before anything fetches. The collection METADATA did not, so the page failed
 * closed — "Couldn't load this collection" over a cache holding the rows the
 * user was looking at a minute ago.
 *
 * Caching it is not enough on its own, because a collection list is a SCOPE
 * CLAIM: served without the scope it was fetched under it becomes a second way
 * to show a collection the caller can no longer see, which is exactly the
 * disclosure TASK-2922 closed for rows. Hence the stamp, and hence the fence.
 *
 * BOTH SIDES OF THE FENCE ARE DURABLE. The caller reaching for this has just
 * failed to reach the server, so a live epoch is what it cannot have; the
 * question is whether the cached list and the cached ROWS describe the same
 * scope, and both answers are on disk.
 */

function coll(slug: string): Collection {
	return { id: `id-${slug}`, slug, name: slug } as unknown as Collection;
}

function row(id: string, seq: number): ItemIndexRow {
	return { id, seq, collection_slug: 'c' } as unknown as ItemIndexRow;
}

describe('TASK-2946 — a cached collection list is stamped and fenced', () => {
	it('round-trips the list when the stamp still matches the durable rows', async () => {
		const U = null;
		const WS = 'ws-2946-ok';
		const { persistDelta, persistCollections, hydrateCollections } = await loadPersistence();

		await persistDelta(U, WS, [row('a', 1)], '1', false, 'e1');
		await persistCollections(U, WS, [coll('tasks'), coll('ideas')], 'e1', 'e1');

		const list = await hydrateCollections(U, WS);
		expect(list?.map((c) => c.slug)).toEqual(['tasks', 'ideas']);
	});

	it('REFUSES a list whose stamp the durable rows have moved past', async () => {
		const U = null;
		const WS = 'ws-2946-moved';
		const { persistDelta, persistCollections, hydrateCollections, persistReplace } =
			await loadPersistence();

		await persistDelta(U, WS, [row('a', 1)], '1', false, 'e1');
		await persistCollections(U, WS, [coll('secret')], 'e1', 'e1');
		expect((await hydrateCollections(U, WS))?.length).toBe(1);

		// A resync under a NARROWED scope. The rows are replaced and the durable
		// epoch moves; the cached list still describes the old scope and may name
		// a collection this caller can no longer see.
		await persistReplace(U, WS, [row('a', 1)], '2', false, 'e2');

		expect(await hydrateCollections(U, WS)).toBeNull();
	});

	it('does NOT persist when the scope moved across the fetch', async () => {
		const U = null;
		const WS = 'ws-2946-inflight';
		const { persistDelta, persistCollections, hydrateCollections } = await loadPersistence();

		await persistDelta(U, WS, [row('a', 1)], '1', false, 'e2');
		// The list was requested while RAM said e1 and arrived after a resync had
		// moved it to e2 — so it may describe either scope, and a stamp would be
		// a guess. The whole point of taking two epochs.
		await persistCollections(U, WS, [coll('tasks')], 'e1', 'e2');

		expect(await hydrateCollections(U, WS)).toBeNull();
	});

	it('does NOT persist into a cache that has never synced, and the row cannot appear later', async () => {
		const U = null;
		const WS = 'ws-2946-nosync';
		const { persistDelta, persistCollections, hydrateCollections } = await loadPersistence();

		// No `sync` row: there is no epoch to BORROW, so any stamp would be
		// invented rather than observed.
		await persistCollections(U, WS, [coll('tasks')], 'e1', 'e1');
		expect(await hydrateCollections(U, WS)).toBeNull();

		// The discriminating half, and the reason this leg is not just restating
		// the hydrate fence: a list written with an invented `null` stamp would
		// become READABLE the moment a sync row landed carrying null — which is
		// every server predating `access_epoch`. The refusal at write time is
		// what stops a list nobody could vouch for from being served later by a
		// fence that has nothing to compare it against.
		//
		// (Found by mutating: removing the write-time guard left every other leg
		// in this file green, because the hydrate fence covers the same ground
		// up to exactly this case.)
		await persistDelta(U, WS, [row('a', 1)], '1', false, null);

		expect(await hydrateCollections(U, WS)).toBeNull();
	});

	it('treats null on both sides as a match — a server without access_epoch still caches', async () => {
		const U = null;
		const WS = 'ws-2946-null';
		const { persistDelta, persistCollections, hydrateCollections } = await loadPersistence();

		await persistDelta(U, WS, [row('a', 1)], '1', false, null);
		await persistCollections(U, WS, [coll('tasks')], null, null);

		expect((await hydrateCollections(U, WS))?.map((c) => c.slug)).toEqual(['tasks']);
	});

	it('CONTROL — an ordinary delta that does not move the epoch leaves the list readable', async () => {
		const U = null;
		const WS = 'ws-2946-control';
		const { persistDelta, persistCollections, hydrateCollections } = await loadPersistence();

		await persistDelta(U, WS, [row('a', 1)], '1', false, 'e1');
		await persistCollections(U, WS, [coll('tasks')], 'e1', 'e1');

		// Without this leg the refusal above would also pass against a fence
		// that refused everything — including every ordinary session, which
		// would make the whole feature inert while looking correct.
		await persistDelta(U, WS, [row('b', 2)], '2', false, undefined);

		expect((await hydrateCollections(U, WS))?.map((c) => c.slug)).toEqual(['tasks']);
	});

	it('returns null when nothing was ever cached, without inventing a list', async () => {
		const U = null;
		const WS = 'ws-2946-empty';
		const { persistDelta, hydrateCollections } = await loadPersistence();

		await persistDelta(U, WS, [row('a', 1)], '1', false, 'e1');

		expect(await hydrateCollections(U, WS)).toBeNull();
	});

	it('replaces the whole list rather than merging into it', async () => {
		const U = null;
		const WS = 'ws-2946-replace';
		const { persistDelta, persistCollections, hydrateCollections } = await loadPersistence();

		await persistDelta(U, WS, [row('a', 1)], '1', false, 'e1');
		await persistCollections(U, WS, [coll('tasks'), coll('gone')], 'e1', 'e1');
		await persistCollections(U, WS, [coll('tasks')], 'e1', 'e1');

		// A list is a snapshot of the visible set under one scope, true only as
		// a whole — a collection dropped by a narrowed scope must not survive as
		// a leftover. This is why it is ONE meta row and not a store of rows.
		expect((await hydrateCollections(U, WS))?.map((c) => c.slug)).toEqual(['tasks']);
	});
});
