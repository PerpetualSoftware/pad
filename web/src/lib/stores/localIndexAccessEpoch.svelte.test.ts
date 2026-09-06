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
 * Establish a baseline epoch on an EMPTY cache, then populate — the order a
 * real session takes (a cold snapshot carries the epoch that describes it).
 * Adopting a baseline onto an already-populated cache is not a no-op: it is
 * the unknown-provenance case, and it resyncs by design (see the fourth test).
 */
async function seedUnder(epoch: string, rows: ItemIndexRow[]): Promise<void> {
	await localIndex.ensureAccessScope(ws, epoch);
	for (const r of rows) localIndex.upsert(ws, r);
	localIndex.applyDelta(ws, [], String(rows.length), false);
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

	it('drops rows the cold snapshot omits before adopting its epoch', async () => {
		// Review round 1, F1. The cold path MERGES, which is right when RAM is
		// empty and wrong when a concurrent delta or optimistic write populated
		// it while /items-index was in flight: the omitted row survives and the
		// snapshot's epoch is stamped over it. The cache then advertises a
		// scope it does not hold, and because the next delta agrees with that
		// epoch, no resync ever fires again.
		//
		// The assertion that matters is the PAIR: the row is gone AND the epoch
		// is the snapshot's. Asserting only the epoch would pass on the
		// unfixed code, which is exactly how this got past four mutants.
		// ROUND 3: the mechanism changed, the property did not. The cold path no
		// longer repairs itself in place — a snapshot that omits rows RAM holds
		// is not a description of where we are, so it is discarded and the
		// authoritative resync runs instead. Hence TWO index responses: the
		// cold one, then the resync's.
		localIndex.upsert(ws, row('secret', 2, 'revoked'));
		expect(pickerCanSee('secret', 'revoked')).toBe(true);

		vi.spyOn(api.items, 'listIndex')
			.mockResolvedValueOnce({
				items: [row('keeper', 1, 'kept')],
				total: 1,
				cursor: '1',
				includes_unparented_metadata: false,
				access_epoch: 'cold-epoch',
			})
			.mockResolvedValueOnce({
				items: [row('keeper', 1, 'kept')],
				total: 1,
				cursor: '1',
				includes_unparented_metadata: false,
				access_epoch: 'cold-epoch',
			});
		await localIndex.bootstrap(ws, { userId: null });

		expect(pickerCanSee('secret', 'revoked')).toBe(false);
		expect(pickerCanSee('keeper', 'kept')).toBe(true);
		expect(localIndex.accessEpochFor(ws)).toBe('cold-epoch');
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

	it('refuses a delta whose epoch disagrees with the baseline, rather than applying it', async () => {
		// Review round 1, P3. Both of today's callers compare before applying,
		// so this guard is unreachable in production — it exists so that a
		// future caller handing an ItemChangesResponse straight in gets a
		// dropped batch and a pending resync instead of rows persisted under a
		// baseline that no longer describes them.
		await seedUnder('current', [row('keeper', 1, 'kept')]);

		localIndex.applyDelta(
			ws,
			[{ ...row('smuggled', 9, 'revoked'), deleted: false } as never],
			'9',
			false,
			'stale',
		);

		expect(pickerCanSee('smuggled', 'revoked')).toBe(false);
		expect(localIndex.cursorFor(ws)).toBe('1');
		expect(localIndex.pendingResyncFor(ws)).toBe(true);
	});

	it('pins the cursor back when the cold snapshot drops a row, so the row can come back', async () => {
		// Round 2. F1 made the cold path drop rows the snapshot omits. Forward-
		// only cursor adoption then loses a row that was legitimately ADDED by
		// a delta while /items-index was in flight: dropped from RAM, and the
		// cursor already past the seq that carried it, so no future
		// `?since=cursor` can return it. A visible item disappears until a full
		// resync — a data-loss regression introduced by a security fix.
		//
		// The assertion is the cursor, not the row: the row is correctly gone
		// at this instant either way, and only the cursor decides whether it
		// can come back.
		// ROUND 3: the pin now comes from the resync this case routes to, which
		// has pinned rather than advanced since long before this unit. Same
		// property, one fewer implementation of it.
		localIndex.upsert(ws, row('added-while-in-flight', 2, 'kept'));
		localIndex.applyDelta(ws, [], '2', false);
		expect(localIndex.cursorFor(ws)).toBe('2');

		vi.spyOn(api.items, 'listIndex').mockResolvedValue({
			items: [row('keeper', 1, 'kept')],
			total: 1,
			cursor: '1',
			includes_unparented_metadata: false,
			access_epoch: 'e1',
		});
		await localIndex.bootstrap(ws, { userId: null });

		expect(localIndex.cursorFor(ws)).toBe('1');
		expect(localIndex.pendingResyncFor(ws)).toBe(true);
	});

	it('does not let a delayed cold snapshot walk the baseline backwards', async () => {
		// Round 2. A cold /items-index can be in flight while a concurrent
		// delta learns a NEWER scope — and `ensureAccessScope` records one
		// without resyncing when RAM is empty, which is exactly a cold boot's
		// state. Adopting the older snapshot's epoch there would reopen the
		// window this whole change closes, and silently: the cache would then
		// hold the snapshot's rows under a baseline the server has already
		// moved past.
		// The delta must land AFTER the request is ISSUED, not merely after
		// bootstrap is called — bootstrap awaits its IDB hydrate first, so a
		// delta racing that earlier await is a different (and benign) ordering.
		// Getting this wrong is what made the first version of this test fail
		// against correct code, which is the useful kind of test failure.
		let resolveIndex: ((v: never) => void) | null = null;
		let requestIssued: () => void = () => {};
		const issued = new Promise<void>((res) => {
			requestIssued = res;
		});
		vi.spyOn(api.items, 'listIndex').mockImplementationOnce(() => {
			requestIssued();
			return new Promise((res) => {
				resolveIndex = res as (v: never) => void;
			}) as never;
		});

		const boot = localIndex.bootstrap(ws, { userId: null });
		await issued;
		// The concurrent delta, landing while the snapshot is in flight.
		await localIndex.ensureAccessScope(ws, 'newer');
		expect(localIndex.accessEpochFor(ws)).toBe('newer');

		// The resync this now routes to fetches its own snapshot under the
		// CURRENT scope. Round 2 kept the superseded snapshot's rows while
		// refusing its epoch, which round 3 showed was the worse half of the
		// bargain: the rows were the stale part.
		vi.spyOn(api.items, 'listIndex').mockResolvedValue({
			items: [row('current', 2, 'kept')],
			total: 1,
			cursor: '2',
			includes_unparented_metadata: false,
			access_epoch: 'newer',
		});
		resolveIndex!({
			items: [row('stale', 1, 'kept')],
			total: 1,
			cursor: '1',
			includes_unparented_metadata: false,
			access_epoch: 'older',
		} as never);
		await boot;

		expect(localIndex.accessEpochFor(ws)).toBe('newer');
		// The superseded snapshot's ROWS are gone too, not merely its epoch.
		expect(localIndex.findByIdOrSlug(ws, 'stale')).toBeNull();
		expect(localIndex.pendingResyncFor(ws)).toBe(true);
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

	it('discards a cold snapshot whose cursor is behind what RAM has already applied', async () => {
		// Round 4. The two checks above ask what RAM holds that the snapshot
		// omits. The inverse — a row the snapshot holds that RAM deliberately
		// REMOVED, via a moved_out applied while /items-index was in flight —
		// is invisible to both, and merging puts the hidden row back with the
		// cursor already past the move, so no later delta re-sends it and it
		// survives a reload.
		//
		// RAM cannot answer "did I remove this", but the cursor answers the
		// general question, which is the better instrument: it covers every
		// mutation applied during the request, not the one shape I thought of.
		localIndex.applyDelta(ws, [], '9', false);
		expect(localIndex.cursorFor(ws)).toBe('9');

		const stale = {
			items: [row('moved-out', 3, 'revoked')],
			total: 1,
			cursor: '4',
			includes_unparented_metadata: false,
			access_epoch: 'e1',
		};
		const fresh = {
			items: [row('keeper', 9, 'kept')],
			total: 1,
			cursor: '9',
			includes_unparented_metadata: false,
			access_epoch: 'e1',
		};
		vi.spyOn(api.items, 'listIndex')
			.mockResolvedValueOnce(stale)
			.mockResolvedValueOnce(fresh);

		await localIndex.bootstrap(ws, { userId: null });

		// The behind-snapshot's row never entered the store; the resync's did.
		expect(pickerCanSee('moved-out', 'revoked')).toBe(false);
		expect(pickerCanSee('keeper', 'kept')).toBe(true);
	});

	it('adopts silently when there is no baseline and nothing cached', async () => {
		// Nothing to evict, so a resync would buy nothing and cost a full
		// index fetch on every cold start.
		const listIndex = vi.spyOn(api.items, 'listIndex');
		expect(await localIndex.ensureAccessScope(ws, 'first')).toBe(false);
		expect(listIndex).not.toHaveBeenCalled();
		expect(localIndex.accessEpochFor(ws)).toBe('first');
	});
});
