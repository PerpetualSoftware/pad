import { afterEach, describe, expect, it, vi } from 'vitest';
import { api, PadApiError } from '$lib/api/client';
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
		// The pin (IDEA-2924) makes an overtaken cold boot REPLAY from the
		// snapshot's cursor, so this door is now reached where it never used to
		// be. The server re-delivers the eviction, because `ListMovedOutSince` is
		// a `seq > since` query and the pinned cursor is below it.
		vi.spyOn(api.items, 'changes').mockResolvedValue({
			changes: [{ id: 'secret', seq: 100, moved_out: true }],
			cursor: '100',
			includes_unparented_metadata: false,
			access_epoch: 'epoch-1',
		});

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

	it('pins the cursor to the snapshot and replays from it, rather than skipping the gap', async () => {
		// THIS TEST REPLACES ONE THAT ASSERTED THE OPPOSITE. Under TASK-2920 the
		// cold branch kept the HIGHER cursor and set `pendingResync = false`, and
		// a test here pinned that — deliberately, because it was what made the
		// reinstatement permanent rather than cosmetic. IDEA-2924 removes the
		// permanence, so the old assertions are now assertions about a defect
		// that is gone; they are replaced rather than deleted, and this comment
		// is the record of why a green test was changed.
		const gate = deferred<Awaited<ReturnType<typeof api.items.listIndex>>>();
		vi.spyOn(api.items, 'listIndex').mockReturnValueOnce(gate.promise);
		const changes = vi.spyOn(api.items, 'changes').mockResolvedValue({
			changes: [],
			cursor: '95',
			includes_unparented_metadata: false,
			access_epoch: 'epoch-1',
		});

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

		// THE PIN, observable: the replay was issued FROM the snapshot's cursor,
		// not from the higher one the delta had left behind. Asserting the
		// request's `since` rather than the final cursor, because the loop
		// advances the cursor again immediately and the end state cannot tell
		// the two builds apart.
		expect(changes).toHaveBeenCalled();
		expect(changes.mock.calls[0][1]).toBe('95');
	});

	it('keeps the ask set when the replay fails, and does NOT fail the boot over it', async () => {
		// Two properties in one leg because they are one decision. A mutation run
		// found `pendingResync = behind` undetectable — the replay clears the ask
		// on success, so the only moment it is observable is when the replay does
		// NOT succeed. Writing that test then showed the replay's failure was
		// taking the whole bootstrap down with it, discarding a snapshot that was
		// already installed and usable.
		const gate = deferred<Awaited<ReturnType<typeof api.items.listIndex>>>();
		vi.spyOn(api.items, 'listIndex').mockReturnValueOnce(gate.promise);
		vi.spyOn(api.items, 'changes').mockRejectedValue(new Error('network'));

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

		// The boot RESOLVES — the snapshot is good.
		await expect(boot).resolves.toBeUndefined();
		expect(localIndex.bootstrapStateFor(ws)).toBe('ready');
		expect(canSee('keeper', 'kept')).toBe(true);
		// ...and the ask is still outstanding, so a later bootstrap resumes.
		expect(localIndex.pendingResyncFor(ws)).toBe(true);
	});

	it('does NOT swallow a 403 from the replay — the purge still happens', async () => {
		// The auth carve-out in the replay's catch. A mutant that swallowed
		// everything survived the suite: a revocation surfacing on the REPLAY
		// rather than on the snapshot would have left the cache serving rows the
		// caller can no longer see, which is the whole thing TASK-1360 exists to
		// stop. Non-fatal for a network blip is right; non-fatal for a 403 is the
		// bug wearing the same clothes.
		const gate = deferred<Awaited<ReturnType<typeof api.items.listIndex>>>();
		vi.spyOn(api.items, 'listIndex').mockReturnValueOnce(gate.promise);
		vi.spyOn(api.items, 'changes').mockImplementation(async () => {
			// What api.request() does, in order: the global handler resets first.
			localIndex.reset(ws);
			throw new PadApiError({ code: 'forbidden', message: 'forbidden' });
		});

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

		await expect(boot).rejects.toThrow();
		expect(localIndex.accessRevokedFor(ws)).toBe(true);
	});

	it('makes NO replay when the cold boot was not overtaken', async () => {
		// The control leg. Without it the assertions above would also pass on a
		// build that replayed unconditionally, which would be a different and
		// worse change — an extra round-trip on every cold boot.
		vi.spyOn(api.items, 'listIndex').mockResolvedValueOnce({
			items: [row('keeper', 1, 'kept')],
			total: 1,
			cursor: '95',
			includes_unparented_metadata: false,
			access_epoch: 'epoch-1',
		});
		const changes = vi.spyOn(api.items, 'changes');

		await localIndex.bootstrap(ws, { userId: null });

		expect(changes).not.toHaveBeenCalled();
		expect(localIndex.pendingResyncFor(ws)).toBe(false);
	});
});
