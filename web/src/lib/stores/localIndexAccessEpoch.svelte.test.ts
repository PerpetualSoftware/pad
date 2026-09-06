import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '$lib/api/client';
import type { ItemIndexRow } from '$lib/types';
import { localIndex } from './localIndex.svelte';
import { localSearch } from './localSearch.svelte';

/**
 * IDEA-2898 — a revocation that writes no item.
 *
 * The whole class turns on a case the delta stream cannot express: the
 * caller's visible set narrows, no row changes, and the warm local index goes
 * on serving the revoked collection. `ItemPicker` reads that index through
 * exactly two functions — `getByCollection` for its empty-query listing and
 * `localSearch.search` (+ `findByIdOrSlug`) for its warm query — so every
 * eviction assertion here is made against BOTH. Asserting only the store map
 * would leave the MiniSearch index free to keep answering with a title the
 * caller may no longer see, which is the disclosure this closes.
 */

const ws = 'access-epoch-test';

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

/** Both of ItemPicker's read paths, asked about one id. */
function pickerCanSee(id: string, collection: string): boolean {
	const listed = localIndex
		.getByCollection(ws, collection)
		.some((r) => r.id === id);
	const searched = localSearch
		.search(ws, `Title ${id}`, { limit: 20 })
		.some((hit) => hit.id === id);
	return listed || searched;
}

/**
 * Establish a baseline epoch and populate the index, the order a real session
 * takes: a cold snapshot carries the epoch that describes it. Adopting a
 * baseline onto an already-populated cache is not a no-op — it is the
 * unknown-provenance case, and it resyncs by design (see "resyncs a populated
 * cache that carries no baseline epoch").
 */
async function seedUnder(epoch: string, rows: ItemIndexRow[]): Promise<void> {
	// Through a COLD BOOTSTRAP, which is how a real session acquires its first
	// baseline: the snapshot carries the epoch that describes it. The earlier
	// shortcut — calling `ensureAccessScope` on a never-bootstrapped workspace —
	// stopped working when the silent adopt was gated on the durable cache
	// having answered, and it was the less faithful of the two anyway.
	const listIndex = vi.spyOn(api.items, 'listIndex');
	listIndex.mockResolvedValueOnce({
		items: rows,
		total: rows.length,
		cursor: String(rows.length),
		includes_unparented_metadata: false,
		access_epoch: epoch,
	});
	await localIndex.bootstrap(ws, { userId: null });
	listIndex.mockClear();
}

afterEach(() => {
	vi.restoreAllMocks();
	localIndex.reset(ws);
});

describe('localIndex access-epoch scope', () => {
	it('evicts a revoked collection from both ItemPicker read paths when the epoch changes', async () => {
		await seedUnder('epoch-before', [row('keeper', 1, 'kept'), row('secret', 2, 'revoked')]);
		expect(pickerCanSee('secret', 'revoked')).toBe(true);

		// The authoritative snapshot under the narrowed scope omits the revoked
		// collection entirely — which is what makes the eviction exact rather
		// than a client-side guess about what the server would allow.
		vi.spyOn(api.items, 'listIndex').mockResolvedValueOnce({
			items: [row('keeper', 1, 'kept')],
			total: 1,
			cursor: '2',
			includes_unparented_metadata: false,
			access_epoch: 'epoch-after',
		});

		expect(await localIndex.ensureAccessScope(ws, 'epoch-after')).toBe(true);
		expect(pickerCanSee('secret', 'revoked')).toBe(false);
		// The control leg: eviction is scoped to what the snapshot dropped, not
		// a blanket wipe that would look identical on the assertion above.
		expect(pickerCanSee('keeper', 'kept')).toBe(true);
		expect(localIndex.accessEpochFor(ws)).toBe('epoch-after');
	});

	it('does not resync when the epoch is unchanged', async () => {
		await seedUnder('steady', [row('keeper', 1, 'kept')]);

		const listIndex = vi.spyOn(api.items, 'listIndex');
		expect(await localIndex.ensureAccessScope(ws, 'steady')).toBe(false);
		// A resync on every poll would be strictly worse than the defect: it
		// re-fetches the whole index each time. Asserting the REQUEST was never
		// issued is the only thing that can tell "no resync" apart from "a
		// resync that happened to change nothing".
		expect(listIndex).not.toHaveBeenCalled();
	});

	it('does not resync when the server sends no epoch at all', async () => {
		await seedUnder('known', [row('keeper', 1, 'kept')]);

		const listIndex = vi.spyOn(api.items, 'listIndex');
		// An older server mid-deploy. Absence is not a value: reading it as a
		// changed set would make every poll against such a server a full
		// resync, and reading it as an unchanged set is what we do instead.
		expect(await localIndex.ensureAccessScope(ws, undefined)).toBe(false);
		expect(listIndex).not.toHaveBeenCalled();
		expect(localIndex.accessEpochFor(ws)).toBe('known');
	});

	it('resyncs a populated cache that carries no baseline epoch', async () => {
		// The offline-revocation case, and the reason the epoch is persisted at
		// all: this cache was written under some scope nobody recorded, so it
		// cannot be shown to be current. Adopting the incoming epoch here would
		// mark stale rows as authorised without ever checking them.
		localIndex.upsert(ws, row('unknown-provenance', 1, 'kept'));
		localIndex.applyDelta(ws, [], '1', false);
		expect(localIndex.accessEpochFor(ws)).toBeNull();

		const listIndex = vi.spyOn(api.items, 'listIndex').mockResolvedValueOnce({
			items: [],
			total: 0,
			cursor: '1',
			includes_unparented_metadata: false,
			access_epoch: 'fresh',
		});
		expect(await localIndex.ensureAccessScope(ws, 'fresh')).toBe(true);
		expect(listIndex).toHaveBeenCalled();
		expect(pickerCanSee('unknown-provenance', 'kept')).toBe(false);
	});

	it("reaches the comparison from bootstrap's own reconcile loop, not just the page's poll", async () => {
		// CONVE-19: the comparison being CORRECT is proved by the tests above;
		// this proves it is REACHED from the path every route shares. It matters
		// because the page-driven poll lives in the collection route component,
		// so item detail and the copy dialog — the picker's actual home — only
		// ever reconcile through here. A version of this change that wired only
		// the page would pass every other test in this file.
		const listIndex = vi.spyOn(api.items, 'listIndex');

		// Cold boot: one authoritative snapshot, two collections visible.
		listIndex.mockResolvedValueOnce({
			items: [row('keeper', 1, 'kept'), row('secret', 2, 'revoked')],
			total: 2,
			cursor: '2',
			includes_unparented_metadata: false,
			access_epoch: 'e1',
		});
		await localIndex.bootstrap(ws, { userId: null });
		expect(pickerCanSee('secret', 'revoked')).toBe(true);

		// Put the workspace into the reentry state the reconcile loop runs in
		// (ready + pendingResync), the way a real resync does.
		listIndex.mockResolvedValueOnce({
			items: [row('keeper', 1, 'kept'), row('secret', 2, 'revoked')],
			total: 2,
			cursor: '2',
			includes_unparented_metadata: false,
			access_epoch: 'e2',
		});
		expect(await localIndex.ensureAccessScope(ws, 'e2')).toBe(true);
		expect(pickerCanSee('secret', 'revoked')).toBe(true);

		// Now the revocation, visible ONLY as a changed epoch on the delta.
		vi.spyOn(api.items, 'changes').mockResolvedValue({
			changes: [],
			cursor: '5',
			includes_unparented_metadata: false,
			access_epoch: 'e3',
		});
		listIndex.mockResolvedValueOnce({
			items: [row('keeper', 1, 'kept')],
			total: 1,
			cursor: '5',
			includes_unparented_metadata: false,
			access_epoch: 'e3',
		});

		await localIndex.bootstrap(ws, { userId: null });

		expect(pickerCanSee('secret', 'revoked')).toBe(false);
		expect(pickerCanSee('keeper', 'kept')).toBe(true);
		expect(localIndex.accessEpochFor(ws)).toBe('e3');
	});

	it('adopts the COLD snapshot\'s epoch, so the next quiet poll does not resync', async () => {
		// The cold bootstrap path is the only place a first-ever session gets a
		// baseline. Dropping it there is invisible to every eviction test in
		// this file — the cache would simply have no baseline, and a
		// null baseline over a populated cache resyncs, which LOOKS like the
		// change working. The discriminating case is the QUIET one: an
		// unchanged scope must cost nothing.
		const listIndex = vi.spyOn(api.items, 'listIndex');
		listIndex.mockResolvedValueOnce({
			items: [row('keeper', 1, 'kept')],
			total: 1,
			cursor: '1',
			includes_unparented_metadata: false,
			access_epoch: 'cold-e1',
		});
		await localIndex.bootstrap(ws, { userId: null });
		expect(localIndex.accessEpochFor(ws)).toBe('cold-e1');

		listIndex.mockClear();
		// Same scope as the snapshot just installed. Nothing to do.
		expect(await localIndex.ensureAccessScope(ws, 'cold-e1')).toBe(false);
		expect(listIndex).not.toHaveBeenCalled();
	});

	it('keeps a known baseline when a snapshot carries no epoch, and stops asking', async () => {
		// Review round 1, F3. Mixed deployment: the delta comes from a server
		// that sends the field, the resync's snapshot from one that does not.
		// `?? null` read absent as "no baseline", so the null-baseline branch
		// resynced again on the next poll — a full snapshot per iteration up to
		// the 50-page cap.
		//
		// The counterfactual is the whole test: ONE resync, not two. Asserting
		// only that the first resync happened would pass on the unfixed code.
		await seedUnder('old', [row('keeper', 1, 'kept')]);

		const listIndex = vi.spyOn(api.items, 'listIndex').mockResolvedValue({
			items: [row('keeper', 1, 'kept')],
			total: 1,
			cursor: '1',
			includes_unparented_metadata: false,
			// No access_epoch: an older server answering the snapshot.
		});

		expect(await localIndex.ensureAccessScope(ws, 'new')).toBe(true);
		// The baseline is what the server TOLD us, not null — a true statement
		// about what we were last told, which is all the baseline ever claims.
		expect(localIndex.accessEpochFor(ws)).toBe('new');

		expect(await localIndex.ensureAccessScope(ws, 'new')).toBe(false);
		expect(listIndex).toHaveBeenCalledTimes(1);
	});

	it('records the told-epoch even when it JOINS a resync that was already running', async () => {
		// Round 3, P2. Resyncs are deduplicated per workspace, so a resync
		// already in flight — started by a PROJECTION mismatch, which passes no
		// fallback — returns its promise and the access caller's fallback is
		// never seen. The baseline then stays unchanged and the next poll asks
		// for the same resync again.
		//
		// The instrument has to hold the first resync OPEN across the second
		// call, or the two never overlap and the test passes against code that
		// has the bug. That timing is the test.
		await seedUnder('old', [row('keeper', 1, 'kept')]);

		let release: (() => void) | null = null;
		let inFlight: () => void = () => {};
		const started = new Promise<void>((res) => {
			inFlight = res;
		});
		const snapshot = {
			items: [row('keeper', 1, 'kept')],
			total: 1,
			cursor: '1',
			includes_unparented_metadata: true,
			// No access_epoch — the mixed-deployment case, so only a fallback
			// can move the baseline.
		};
		vi.spyOn(api.items, 'listIndex').mockImplementationOnce(() => {
			inFlight();
			return new Promise((res) => {
				release = () => res(snapshot as never);
			}) as never;
		});

		// Projection mismatch starts the resync; it passes no fallback.
		const projection = localIndex.ensureProjectionScope(ws, true);
		await started;
		// The access caller joins the SAME promise.
		const access = localIndex.ensureAccessScope(ws, 'told');
		release!();
		await Promise.all([projection, access]);

		expect(localIndex.accessEpochFor(ws)).toBe('told');
	});

	it('adopts silently when there is no baseline and the cache is known empty', async () => {
		// Nothing to evict, so a resync would buy nothing and cost a full
		// index fetch on every cold start.
		//
		// PROPERTY CHANGED in review of the reduced tip, and said out loud
		// rather than quietly rewritten: this used to hold for a workspace that
		// had never been bootstrapped at all. It no longer does, because RAM
		// being empty was standing in for "the cache is empty" and those are
		// different claims while a hydrate is still in flight. The cold
		// bootstrap below is what makes the cache's emptiness KNOWN; the
		// unbootstrapped case is the next test.
		const listIndex = vi.spyOn(api.items, 'listIndex');
		listIndex.mockResolvedValueOnce({
			items: [],
			total: 0,
			cursor: '0',
			includes_unparented_metadata: false,
		});
		await localIndex.bootstrap(ws, { userId: null });
		listIndex.mockClear();

		expect(await localIndex.ensureAccessScope(ws, 'first')).toBe(false);
		expect(listIndex).not.toHaveBeenCalled();
		expect(localIndex.accessEpochFor(ws)).toBe('first');
	});

	it('does NOT adopt before the durable cache has answered', async () => {
		// The race this closes: `bootstrap` awaits `hydrate` before it merges
		// anything, but an SSE-driven `deltaSync` runs on its own subscription
		// rather than behind that await — so it can reach the comparison with
		// RAM empty and IDB holding rows from a scope nobody has checked.
		// Adopting there stamps the new epoch onto the durable cache through
		// the delta that follows, and the stale row then hydrates under an
		// epoch that agrees with the server forever: the signal is not merely
		// wrong once, it is permanently inert.
		//
		// Declining costs nothing. The delta persists with a null epoch, and a
		// null baseline over a populated cache resyncs on the next reconcile.
		const listIndex = vi.spyOn(api.items, 'listIndex');

		expect(await localIndex.ensureAccessScope(ws, 'e2')).toBe(false);
		expect(listIndex).not.toHaveBeenCalled();
		expect(localIndex.accessEpochFor(ws)).toBeNull();
	});
});
