import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '$lib/api/client';
import type { ItemIndexRow } from '$lib/types';
import { localIndex } from './localIndex.svelte';

/**
 * TASK-2921 — the reconcile loop is the store's, and both doors are the same
 * code.
 *
 * This file REPLACES `localIndexAccessEpochPageWiring.test.ts`, a source-text
 * pin on the collection route's own copy of the loop. That pin said in its own
 * header that it "should be DELETED the day the page's deltaSync grows a real
 * harness or moves into the store (IDEA-2901 would move it)" — which is this
 * unit. It was a tripwire because the call site lived in a ~3700-line component
 * with no harness and could not be reached behaviourally; the public
 * `localIndex.reconcile` door can be, so the coverage moves from matching text
 * to exercising behaviour, and the mutant the pin could NOT catch (short-
 * circuiting the call, which leaves the text in place) now dies.
 */

const ws = 'reconcile-shared';

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

/** Cold-boot the workspace under a known epoch, the way a real session does. */
async function boot(rows: ItemIndexRow[], epoch = 'epoch-1', cursor = '10'): Promise<void> {
	const listIndex = vi.spyOn(api.items, 'listIndex');
	listIndex.mockResolvedValueOnce({
		items: rows,
		total: rows.length,
		cursor,
		includes_unparented_metadata: false,
		access_epoch: epoch,
	});
	await localIndex.bootstrap(ws, { userId: null });
	listIndex.mockRestore();
}

afterEach(() => {
	vi.restoreAllMocks();
	localIndex.reset(ws);
});

describe('localIndex.reconcile — the door the collection route now uses', () => {
	it('routes a changed access epoch through a resync instead of applying the delta', async () => {
		await boot([row('keeper', 1, 'kept'), row('secret', 2, 'revoked')]);

		// The delta arrives under a DIFFERENT epoch. The rows it carries are not
		// the point — the scope change is, and it must be acted on before
		// anything in this response is trusted.
		vi.spyOn(api.items, 'changes')
			.mockResolvedValueOnce({
				changes: [row('secret', 11, 'revoked')],
				cursor: '11',
				includes_unparented_metadata: false,
				access_epoch: 'epoch-2',
			})
			.mockResolvedValue({
				changes: [],
				cursor: '20',
				includes_unparented_metadata: false,
				access_epoch: 'epoch-2',
			});
		// The authoritative snapshot under the new scope omits the revoked row.
		const listIndex = vi.spyOn(api.items, 'listIndex').mockResolvedValue({
			items: [row('keeper', 1, 'kept')],
			total: 1,
			cursor: '20',
			includes_unparented_metadata: false,
			access_epoch: 'epoch-2',
		});

		await localIndex.reconcile(ws);

		// A resync ran — this is the behaviour the deleted source pin could only
		// assert the TEXT of, and it dies to the short-circuit mutant the pin
		// survived.
		expect(listIndex).toHaveBeenCalled();
		expect(localIndex.getByCollection(ws, 'revoked').some((r) => r.id === 'secret')).toBe(false);
		// And the ask the resync RAISED is cleared by the same loop that drained
		// it. This assertion lives here rather than in the catch-up test below
		// because a resync is what actually SETS `pendingResync` — after a cold
		// boot it is already false, so asserting the clear there would pass
		// whether or not the clear happened. A mutation run caught exactly that:
		// removing `clearAskIfSettled` left the suite green.
		expect(localIndex.pendingResyncFor(ws)).toBe(false);
	});

	it('reports catch-up and clears the outstanding ask', async () => {
		await boot([row('keeper', 1, 'kept')]);
		vi.spyOn(api.items, 'changes').mockResolvedValue({
			changes: [],
			cursor: '10',
			includes_unparented_metadata: false,
			access_epoch: 'epoch-1',
		});

		expect(await localIndex.reconcile(ws)).toBe(true);
	});

	it('reports NOT caught up when the page cap is hit, leaving the ask set', async () => {
		await boot([row('keeper', 1, 'kept')]);
		// A server that always advances the cursor and never drains: the
		// pathological case the 50-iteration cap exists for.
		let seq = 10;
		vi.spyOn(api.items, 'changes').mockImplementation(async () => {
			seq += 1;
			return {
				changes: [row(`r${seq}`, seq, 'kept')],
				cursor: String(seq),
				includes_unparented_metadata: false,
				access_epoch: 'epoch-1',
			};
		});

		expect(await localIndex.reconcile(ws)).toBe(false);
		// The cap is what bounds it; without one this would not return.
		expect(seq).toBe(60);
	});

	it('does not report catch-up on a response a resync overtook', async () => {
		// TASK-2909's rule, now pinned at the ONE door instead of at neither: the
		// reconcile token must be captured when the request is ISSUED, because
		// the staleness it guards against is decided then. A mutation run found
		// this unpinned — moving the capture to after the await left the suite
		// green, since both orderings end in the same state once the loop's next
		// iteration catches up honestly.
		//
		// The discriminator is therefore the REQUEST COUNT, not the end state: a
		// response the resync overtook must send the loop round again, so the
		// correct code issues two requests where the broken code issues one.
		await boot([row('keeper', 1, 'kept')]);

		let started!: () => void;
		const firstCallStarted = new Promise<void>((r) => {
			started = r;
		});
		let release!: () => void;
		const gate = new Promise<void>((r) => {
			release = r;
		});
		const caughtUpResponse = {
			changes: [],
			cursor: '10',
			includes_unparented_metadata: false,
			access_epoch: 'epoch-1',
		};
		const changes = vi.spyOn(api.items, 'changes');
		changes.mockImplementationOnce(async () => {
			started();
			await gate;
			return caughtUpResponse;
		});
		// Subsequent responses carry the NEW epoch, as a real server would once
		// the scope has changed — otherwise the loop keeps re-resyncing on a
		// stale epoch and the request count stops measuring what this test is
		// about.
		changes.mockResolvedValue({ ...caughtUpResponse, access_epoch: 'epoch-2' });

		const running = localIndex.reconcile(ws);
		await firstCallStarted;

		// A resync settles while that request is in flight, bumping the token.
		vi.spyOn(api.items, 'listIndex').mockResolvedValue({
			items: [row('keeper', 1, 'kept')],
			total: 1,
			cursor: '10',
			includes_unparented_metadata: false,
			access_epoch: 'epoch-2',
		});
		await localIndex.ensureAccessScope(ws, 'epoch-2');

		release();
		await running;

		expect(changes).toHaveBeenCalledTimes(2);
	});

	it('abandons a response whose workspace was reset mid-flight', async () => {
		// A sign-out, user switch or 403 purge while `/items-changes` is in
		// flight. Without a generation fence the loop keeps using the DETACHED
		// state's cursor and `applyDelta` — which calls `ensureState(ws)` —
		// writes the old response into the REPLACEMENT state: one user's rows in
		// another's cache. The reconcile token does not cover this; it makes the
		// loop re-poll rather than abort (codex round 4 P1).
		await boot([row('keeper', 1, 'kept')]);

		let started!: () => void;
		const inFlight = new Promise<void>((r) => {
			started = r;
		});
		let release!: () => void;
		const gate = new Promise<void>((r) => {
			release = r;
		});
		vi.spyOn(api.items, 'changes').mockImplementation(async () => {
			started();
			await gate;
			return {
				changes: [row('from-the-old-session', 11, 'kept')],
				cursor: '11',
				includes_unparented_metadata: false,
				access_epoch: 'epoch-1',
			};
		});

		const running = localIndex.reconcile(ws);
		await inFlight;

		// The workspace is dropped and a fresh one takes its place.
		localIndex.reset(ws);
		localIndex.upsert(ws, row('new-session-row', 1, 'kept'));

		release();
		expect(await running).toBe(false);

		// The stale response must not have landed in the replacement state.
		const kept = localIndex.getByCollection(ws, 'kept');
		expect(kept.some((r) => r.id === 'from-the-old-session')).toBe(false);
		expect(kept.some((r) => r.id === 'new-session-row')).toBe(true);
	});

	it('two concurrent reconciles cannot regress the cursor', async () => {
		// IDEA-2901 asked for this to be PINNED rather than assumed, and codex
		// round 11 proposed it as a defect: two loops both start at cursor 10,
		// the newer response applies 20, then the older applies 15 and progress
		// goes backwards. It cannot happen — `applyDelta` reads `state.cursor`,
		// compares and assigns with NO await between them, so a non-advancing
		// batch is dropped whole by guard 1 and nothing can interleave.
		//
		// MEASURED DURING THE WINDOW, NOT AT THE END. The first version of this
		// test asserted the final cursor, and a mutant deleting guard 1 SURVIVED
		// it: the older loop regresses the cursor to 15 and then immediately
		// polls again and re-advances to 20, so the end state is identical and
		// the stale row is skipped by guard 2 either way. The only thing that can
		// see it is the cursor each request is ISSUED from.
		await boot([row('keeper', 1, 'kept')]);

		const issuedFrom: number[] = [];
		let releaseOld!: () => void;
		const oldGate = new Promise<void>((r) => {
			releaseOld = r;
		});
		let call = 0;
		vi.spyOn(api.items, 'changes').mockImplementation(async () => {
			issuedFrom.push(Number(localIndex.cursorFor(ws)));
			call += 1;
			if (call === 1) {
				// The OLDER request: issued first, resolves last, lower cursor.
				await oldGate;
				return {
					changes: [row('stale', 15, 'kept')],
					cursor: '15',
					includes_unparented_metadata: false,
					access_epoch: 'epoch-1',
				};
			}
			return {
				changes: [row('fresh', 20, 'kept')],
				cursor: '20',
				includes_unparented_metadata: false,
				access_epoch: 'epoch-1',
			};
		});

		const older = localIndex.reconcile(ws);
		await Promise.resolve();
		const newer = localIndex.reconcile(ws);
		await newer;
		expect(localIndex.cursorFor(ws)).toBe('20');

		releaseOld();
		await older;

		// THE PROPERTY: no request was ever issued from a cursor lower than one
		// an earlier request had already been issued from.
		const highWater = issuedFrom.map((_, i) => Math.max(...issuedFrom.slice(0, i + 1)));
		expect(issuedFrom).toEqual(highWater);

		expect(localIndex.cursorFor(ws)).toBe('20');
		expect(localIndex.getByCollection(ws, 'kept').some((r) => r.id === 'fresh')).toBe(true);
	});

	it('is a no-op reporting catch-up for a workspace that was never hydrated', async () => {
		const changes = vi.spyOn(api.items, 'changes');
		expect(await localIndex.reconcile('never-hydrated')).toBe(true);
		expect(changes).not.toHaveBeenCalled();
	});
});

describe('the behaviour change the shared path brings to bootstrap', () => {
	it('a null projection scope over a populated cache now RESYNCS rather than adopting', async () => {
		// bootstrap's inline test was `includesUnparentedMetadata !== null && !==`,
		// so a null scope fell through and silently adopted the incoming value.
		// `ensureProjectionScope` — which both doors now go through — resyncs
		// instead, for the same reason the access epoch's null baseline does.
		// Reaching a null scope over a populated cache without a test-only hook:
		// an SSE `upsert` lands rows before any response has declared the scope.
		// `upsert` populates `items` and deliberately touches neither the cursor
		// nor `includesUnparentedMetadata`, which is exactly the state the null
		// branch exists for.
		localIndex.upsert(ws, row('early', 1, 'kept'));
		expect(localIndex.getByCollection(ws, 'kept').some((r) => r.id === 'early')).toBe(true);

		vi.spyOn(api.items, 'changes').mockResolvedValue({
			changes: [],
			cursor: '10',
			includes_unparented_metadata: true,
			access_epoch: 'epoch-1',
		});
		const listIndex = vi.spyOn(api.items, 'listIndex').mockResolvedValue({
			items: [row('keeper', 1, 'kept')],
			total: 1,
			cursor: '10',
			includes_unparented_metadata: true,
			access_epoch: 'epoch-1',
		});

		await localIndex.reconcile(ws);
		expect(listIndex).toHaveBeenCalled();
	});
});
