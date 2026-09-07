import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '$lib/api/client';
import type { ItemIndexRow } from '$lib/types';
import { localIndex } from './localIndex.svelte';
import { localSearch } from './localSearch.svelte';

/**
 * PLAN-2903 item 6 / TASK-2920 — the INVERSE cold check.
 *
 * The covered direction is "RAM holds a row the snapshot omits", which
 * `resyncProjectionScope` closes by dropping every absent id. This file is the
 * other direction: the snapshot holds a row whose eviction RAM has ALREADY
 * CONSUMED, because a `moved_out` delta was applied while `/items-index` was in
 * flight.
 *
 * Why the cold path and not the resync path: a resync PINS the cursor to the
 * snapshot's (`state.cursor = resp.cursor`), so the eviction it reinstates is
 * re-delivered by the caller's very next `/items-changes` and the row goes away
 * again. `bootstrap`'s cold branch keeps the HIGHER cursor and then sets
 * `pendingResync = false`, so the eviction is never replayed — the reinstated
 * row is durable, and the same branch persists it to IDB, so it survives a
 * reload.
 */

const ws = 'inverse-cold-test';

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

/** Both of ItemPicker's read paths, asked about one id — as the sibling suites do. */
function canSee(id: string, collection: string): boolean {
	const listed = localIndex.getByCollection(ws, collection).some((r) => r.id === id);
	const searched = localSearch
		.search(ws, `Title ${id}`, { limit: 20 })
		.some((hit) => hit.id === id);
	return listed || searched;
}

function deferred<T>(): { promise: Promise<T>; resolve: (v: T) => void } {
	let resolve!: (v: T) => void;
	const promise = new Promise<T>((r) => {
		resolve = r;
	});
	return { promise, resolve };
}

/** Let the bootstrap body run up to (and into) its `/items-index` await. */
async function settle(): Promise<void> {
	for (let i = 0; i < 20; i++) await Promise.resolve();
}

afterEach(() => {
	vi.restoreAllMocks();
	localIndex.reset(ws);
});

describe('cold bootstrap vs. an eviction that raced the snapshot', () => {
	it('does not reinstate a row whose moved_out was applied while /items-index was in flight', async () => {
		const gate = deferred<Awaited<ReturnType<typeof api.items.listIndex>>>();
		vi.spyOn(api.items, 'listIndex').mockReturnValueOnce(gate.promise);

		// Cold boot begins: empty IDB, so this takes the /items-index branch.
		const boot = localIndex.bootstrap(ws, { userId: null });
		await settle();

		// While that request is outstanding, the route's own reconcile applies a
		// delta carrying the eviction. This is the exact call `deltaSync` makes
		// (`web/src/routes/[username]/[workspace]/[collection]/+page.svelte`),
		// with the same argument shape; the SSE handler that drives it is
		// registered in `onMount` and is NOT gated on bootstrapState, so it can
		// and does run inside this window.
		localIndex.applyDelta(ws, [{ id: 'secret', seq: 100, moved_out: true }], '100', false);
		expect(canSee('secret', 'revoked')).toBe(false);

		// The snapshot was taken BEFORE the move, so it still lists the row, and
		// its cursor is behind the eviction's seq.
		gate.resolve({
			items: [row('keeper', 1, 'kept'), row('secret', 40, 'revoked')],
			total: 2,
			cursor: '95',
			includes_unparented_metadata: false,
			access_epoch: 'epoch-1',
		});
		await boot;

		// The property under test.
		expect(canSee('secret', 'revoked')).toBe(false);
	});

	it('leaves no replay that could heal it: the cursor stays past the eviction and nothing is pending', async () => {
		const gate = deferred<Awaited<ReturnType<typeof api.items.listIndex>>>();
		vi.spyOn(api.items, 'listIndex').mockReturnValueOnce(gate.promise);

		const boot = localIndex.bootstrap(ws, { userId: null });
		await settle();
		localIndex.applyDelta(ws, [{ id: 'secret', seq: 100, moved_out: true }], '100', false);
		gate.resolve({
			items: [row('keeper', 1, 'kept'), row('secret', 40, 'revoked')],
			total: 2,
			cursor: '95',
			includes_unparented_metadata: false,
			access_epoch: 'epoch-1',
		});
		await boot;

		// This is the half that makes the first assertion's failure PERMANENT
		// rather than transient, and it is asserted separately so a fix that
		// merely re-fires a sync is not mistaken for a fix that holds the
		// property. A later `/items-changes?since=100` cannot re-deliver a
		// change at seq 100.
		expect(localIndex.cursorFor(ws)).toBe('100');
		expect(localIndex.pendingResyncFor(ws)).toBe(false);
	});
});
