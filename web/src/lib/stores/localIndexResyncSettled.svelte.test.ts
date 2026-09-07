import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '$lib/api/client';
import type { ItemIndexRow } from '$lib/types';

/**
 * TASK-2909 (PLAN-2903 item 3) — nothing reports the resync settled until the
 * write it was waiting on has committed.
 *
 * `markCaughtUp`'s `scopeEpoch` guard answers "has a resync landed since I
 * looked". It cannot answer "has the resync I am inside of finished", because
 * the epoch is bumped at the START of a resync — so a reconcile loop that
 * captures it AFTER the bump matches happily while the snapshot is still in
 * flight or was refused on disk, and clears the ask over a cache that has not
 * been written. See TASK-2909 checkpoint 1 for the per-exit table these tests
 * are one-per-row of.
 *
 * Mocked persistence, like its sibling wiring file: in jsdom IDB is
 * unsupported and the real calls are no-ops, so the durable half has to be
 * asserted at the boundary.
 */

const persistence = vi.hoisted(() => ({
	hydrate: vi.fn(async () => ({
		items: [],
		cursor: '0',
		includesUnparentedMetadata: null,
		accessEpoch: null,
		durableRead: true,
		retags: {},
	})),
	persistDelta: vi.fn(async () => undefined),
	persistRemovals: vi.fn(async () => undefined),
	persistReplace: vi.fn(async () => true),
	persistAccessEpoch: vi.fn(async () => undefined),
	persistRetag: vi.fn(async () => undefined),
	persistUpserts: vi.fn(async () => undefined),
	wipe: vi.fn(async () => undefined),
}));
vi.mock('./localIndexPersistence', () => persistence);

const { localIndex } = await import('./localIndex.svelte');

const ws = 'resync-settled';

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

/** A cold bootstrap that leaves the workspace ready with one row. */
async function boot(): Promise<void> {
	vi.spyOn(api.items, 'listIndex').mockResolvedValueOnce({
		items: [row('cached', 1)],
		total: 1,
		cursor: '1',
		includes_unparented_metadata: false,
		access_epoch: 'e1',
	} as unknown as Awaited<ReturnType<typeof api.items.listIndex>>);
	vi.spyOn(api.items, 'changes').mockResolvedValue({
		changes: [],
		cursor: '1',
		includes_unparented_metadata: false,
		access_epoch: 'e1',
	});
	await localIndex.bootstrap(ws, { userId: null });
}

afterEach(() => {
	vi.restoreAllMocks();
	for (const fn of Object.values(persistence)) fn.mockClear();
	persistence.persistReplace.mockImplementation(async () => true);
	localIndex.reset(ws);
});

describe('TASK-2909 — the ask outlives an unwritten resync', () => {
	it('clears the ask when the resync COMMITTED (E4) — the flag must not just wedge', async () => {
		await boot();
		vi.spyOn(api.items, 'listIndex').mockResolvedValue({
			items: [row('cached', 1)],
			total: 1,
			cursor: '1',
			includes_unparented_metadata: true,
			access_epoch: 'e2',
		} as unknown as Awaited<ReturnType<typeof api.items.listIndex>>);
		await localIndex.ensureProjectionScope(ws, true);
		expect(localIndex.pendingResyncFor(ws)).toBe(true);

		localIndex.markCaughtUp(ws, localIndex.reconcileTokenFor(ws));
		expect(localIndex.pendingResyncFor(ws)).toBe(false);
	});

	it('bootstrap\u2019s own loop obeys the same guard as markCaughtUp', async () => {
		// Review round 1 P1b: that loop cleared the flag directly, so every
		// condition added to the public entry point silently did not reach it —
		// the fourth time in PLAN-2903 a rule landed at one door and not its
		// sibling. Reproduced by starting a resync from ANOTHER driver while
		// bootstrap's loop is between its delta response and its clear, which is
		// what the `changes` side effect below does deterministically.
		persistence.hydrate.mockResolvedValueOnce({
			items: [row('cached', 1)],
			cursor: '1',
			includesUnparentedMetadata: false,
			accessEpoch: 'e1',
			durableRead: true,
			retags: {},
		} as unknown as Awaited<ReturnType<typeof persistence.hydrate>>);
		// Never resolves: the concurrent resync stays in flight past the clear.
		vi.spyOn(api.items, 'listIndex').mockReturnValue(
			new Promise(() => {}) as unknown as ReturnType<typeof api.items.listIndex>,
		);
		vi.spyOn(api.items, 'changes').mockImplementation(async () => {
			// A second driver's resync, started but not awaited here.
			void localIndex.ensureProjectionScope(ws, true);
			return {
				changes: [],
				cursor: '1',
				// MATCHES, so bootstrap's own ensureProjectionScope is a no-op
				// and does not await the resync away before the clear.
				includes_unparented_metadata: false,
				access_epoch: 'e1',
			};
		});

		await localIndex.bootstrap(ws, { userId: null });

		expect(localIndex.pendingResyncFor(ws)).toBe(true);
	});

	it('a delta during the persistReplace await does not vouch for the epoch', async () => {
		// The install-point clear of `durableSnapshotCommitted` earns its place
		// here and only here. The two generation guards have no await between
		// them, so the snapshot install is synchronous and the early-return
		// window is unreachable; what IS reachable is the persist await, during
		// which the flag would otherwise still carry the PREVIOUS resync's
		// verdict and let a delta stamp RAM's epoch onto rows this replace has
		// not written yet.
		await boot();
		let settleReplace: ((v: boolean) => void) | undefined;
		persistence.persistReplace.mockImplementation(
			() =>
				new Promise<boolean>((resolve) => {
					settleReplace = resolve;
				}) as unknown as ReturnType<typeof persistence.persistReplace>,
		);
		vi.spyOn(api.items, 'listIndex').mockResolvedValue({
			items: [row('cached', 1)],
			total: 1,
			cursor: '1',
			includes_unparented_metadata: true,
			access_epoch: 'e2',
		} as unknown as Awaited<ReturnType<typeof api.items.listIndex>>);

		const inFlight = localIndex.ensureProjectionScope(ws, true);
		await Promise.resolve();
		await Promise.resolve();

		persistence.persistDelta.mockClear();
		localIndex.applyDelta(ws, [], '2', true);
		const args = persistence.persistDelta.mock.calls.at(-1) as unknown[];
		expect(args[5]).toBeUndefined();

		settleReplace?.(true);
		await inFlight;
	});

	it('an optimistic write issued after the snapshot installed is NOT fenced (round 3 P2)', async () => {
		// The counter split earns its place here. `scopeEpoch` is also the fence
		// `upsert` uses to reject a write authorised under a superseded scope,
		// so bumping it when a resync merely FINISHES PERSISTING rejected writes
		// issued after the new snapshot was already installed: a create that the
		// server accepted would never reach the store, and the field editing it
		// would never see its own result. Two questions, two counters.
		await boot();
		let settleReplace: ((v: boolean) => void) | undefined;
		persistence.persistReplace.mockImplementation(
			() =>
				new Promise<boolean>((resolve) => {
					settleReplace = resolve;
				}) as unknown as ReturnType<typeof persistence.persistReplace>,
		);
		vi.spyOn(api.items, 'listIndex').mockResolvedValue({
			items: [row('cached', 1)],
			total: 1,
			cursor: '1',
			includes_unparented_metadata: true,
			access_epoch: 'e2',
		} as unknown as Awaited<ReturnType<typeof api.items.listIndex>>);

		const inFlight = localIndex.ensureProjectionScope(ws, true);
		await Promise.resolve();
		await Promise.resolve();

		// The scope the write is authorised under is the one now installed.
		const authorisedUnder = localIndex.scopeEpochFor(ws);
		settleReplace?.(true);
		await inFlight;

		localIndex.upsert(ws, row('created', 9), authorisedUnder);
		expect(
			localIndex.getAll(ws, { includeArchived: true }).some((r) => r.id === 'created'),
		).toBe(true);
	});

	it('bootstrap refuses the clear when a resync settles during its post-delta awaits', async () => {
		// Review round 3 P1, and the interleaving matters: the resync must
		// settle AFTER bootstrap's own epoch re-check and BEFORE its clear, or
		// the loop simply re-polls and the test proves nothing. The first
		// version of this test hit the 50-iteration cap instead — it passed, and
		// a mutant that read a fresh token at the clear survived it, which is
		// how the wrong reason showed up.
		//
		// So: the `changes` mock STARTS a resync without awaiting it, and the
		// loop's own no-op access/projection checks are where it settles.
		persistence.hydrate.mockResolvedValueOnce({
			items: [row('cached', 1)],
			cursor: '1',
			includesUnparentedMetadata: false,
			accessEpoch: 'e1',
			durableRead: true,
			retags: {},
		} as unknown as Awaited<ReturnType<typeof persistence.hydrate>>);
		vi.spyOn(api.items, 'listIndex').mockResolvedValue({
			items: [row('cached', 1)],
			total: 1,
			cursor: '1',
			includes_unparented_metadata: false,
			access_epoch: 'e1',
		} as unknown as Awaited<ReturnType<typeof api.items.listIndex>>);
		let call = 0;
		vi.spyOn(api.items, 'changes').mockImplementation(async () => {
			call += 1;
			if (call === 2) {
				// A DIFFERENT driver's resync runs to completion while this
				// request is outstanding. By the time the response is read the
				// resync has settled — so the in-flight check sees nothing —
				// and the token bootstrap captured at the top of THIS iteration
				// is stale. Only comparing that captured value catches it.
				await localIndex.ensureAccessScope(ws, 'e2');
			}
			if (call === 1) {
				// Advance the cursor so iteration 1 is NOT caught up and the
				// loop runs again — otherwise it clears on the first pass and
				// the interleaving below never happens.
				return {
					changes: [row('cached', 2) as unknown as never],
					cursor: '2',
					includes_unparented_metadata: false,
					access_epoch: 'e1',
				};
			}
			return {
				changes: [],
				cursor: '2',
				// Matches what the concurrent resync installed, so neither
				// ensure* fires and the loop actually reaches its clear.
				includes_unparented_metadata: true,
				access_epoch: 'e2',
			};
		});

		await localIndex.bootstrap(ws, { userId: null });

		expect(localIndex.pendingResyncFor(ws)).toBe(true);
	});

	it('does not CONSUME the settled signal — the next delta still stamps the real epoch', async () => {
		await boot();
		vi.spyOn(api.items, 'listIndex').mockResolvedValue({
			items: [row('cached', 1)],
			total: 1,
			cursor: '1',
			includes_unparented_metadata: true,
			access_epoch: 'e2',
		} as unknown as Awaited<ReturnType<typeof api.items.listIndex>>);
		await localIndex.ensureProjectionScope(ws, true);

		localIndex.markCaughtUp(ws, localIndex.reconcileTokenFor(ws));
		expect(localIndex.pendingResyncFor(ws)).toBe(false);

		// The guard READS the settled state; it must not spend it. Asserting
		// that a second `markCaughtUp` still clears proves nothing — the ask is
		// already clear, so the assertion passes either way (it did, and the
		// mutant survived it). The observable that discriminates is the OTHER
		// reader of the same flag: `durableEpochFor` turns a delta's epoch into
		// null the moment the flag goes false, so a clear that consumed the
		// signal would silently downgrade every subsequent delta to "cannot know
		// what this was authorised for" and buy a full resync on the next reload.
		persistence.persistDelta.mockClear();
		localIndex.applyDelta(ws, [], '2', true);
		expect(persistence.persistDelta).toHaveBeenCalled();
		const args = persistence.persistDelta.mock.calls.at(-1) as unknown[];
		expect(args[5]).toBe('e2');
	});

	it('DOES clear the ask when the replace was refused — the repair is not this flag (E5)', async () => {
		await boot();
		persistence.persistReplace.mockImplementation(
			async () => false as unknown as Awaited<ReturnType<typeof persistence.persistReplace>>,
		);
		vi.spyOn(api.items, 'listIndex').mockResolvedValue({
			items: [row('cached', 1)],
			total: 1,
			cursor: '1',
			includes_unparented_metadata: true,
			access_epoch: 'e2',
		} as unknown as Awaited<ReturnType<typeof api.items.listIndex>>);
		await localIndex.ensureProjectionScope(ws, true);

		// The plan called this the defect. It names a real state, but its implied
		// harm — "nothing retries" — stopped being true at TASK-2906, and
		// holding the ask here would buy no repair while costing a wedge:
		// `pendingResyncFor` gates a destructive decision, and a storage failure
		// would make it permanently un-clearable (review round 1 P1).
		localIndex.markCaughtUp(ws, localIndex.reconcileTokenFor(ws));
		expect(localIndex.pendingResyncFor(ws)).toBe(false);

		// The repair lives in the epoch channel instead: the session stops
		// vouching for the durable baseline, so the delta carries it over rather
		// than asserting a scope disk does not hold.
		persistence.persistDelta.mockClear();
		localIndex.applyDelta(ws, [], '2', true);
		const args = persistence.persistDelta.mock.calls.at(-1) as unknown[];
		expect(args[5]).toBeUndefined();
	});

	it('does NOT clear the ask while the resync is still IN FLIGHT (the E2 window)', async () => {
		await boot();
		let release: (() => void) | undefined;
		vi.spyOn(api.items, 'listIndex').mockReturnValue(
			new Promise((resolve) => {
				release = () =>
					resolve({
						items: [row('cached', 1)],
						total: 1,
						cursor: '1',
						includes_unparented_metadata: true,
						access_epoch: 'e2',
					} as unknown as Awaited<ReturnType<typeof api.items.listIndex>>);
			}) as unknown as ReturnType<typeof api.items.listIndex>,
		);

		const inFlight = localIndex.ensureProjectionScope(ws, true);
		// The epoch is bumped at the START of a resync, so a loop capturing it
		// HERE matches the guard and would have cleared the ask over a cache
		// nothing has written yet.
		const epochDuring = localIndex.reconcileTokenFor(ws);
		localIndex.markCaughtUp(ws, epochDuring);
		expect(localIndex.pendingResyncFor(ws)).toBe(true);

		release?.();
		await inFlight;
		// ...and once it has landed, the same call is allowed through.
		localIndex.markCaughtUp(ws, localIndex.reconcileTokenFor(ws));
		expect(localIndex.pendingResyncFor(ws)).toBe(false);
	});

	it('still clears the ask when the resync never INSTALLED anything (E3, the fetch threw)', async () => {
		await boot();
		vi.spyOn(api.items, 'listIndex').mockRejectedValue(new Error('network'));
		await expect(localIndex.ensureProjectionScope(ws, true)).rejects.toThrow('network');

		// RAM and disk still agree — the snapshot was never applied — so the
		// hold has nothing to protect, and over-holding has its own cost:
		// `pendingResyncFor` gates a destructive decision (TASK-2099), and a
		// flag that can only ever be set is a wedge.
		localIndex.markCaughtUp(ws, localIndex.reconcileTokenFor(ws));
		expect(localIndex.pendingResyncFor(ws)).toBe(false);
	});

	it('a caller JOINING a refused resync is bound by the same answer as the starter', async () => {
		await boot();
		persistence.persistReplace.mockImplementation(
			async () => false as unknown as Awaited<ReturnType<typeof persistence.persistReplace>>,
		);
		let release: (() => void) | undefined;
		vi.spyOn(api.items, 'listIndex').mockReturnValue(
			new Promise((resolve) => {
				release = () =>
					resolve({
						items: [row('cached', 1)],
						total: 1,
						cursor: '1',
						includes_unparented_metadata: true,
						access_epoch: 'e2',
					} as unknown as Awaited<ReturnType<typeof api.items.listIndex>>);
			}) as unknown as ReturnType<typeof api.items.listIndex>,
		);

		// Resyncs dedupe per workspace: the second caller gets the first's
		// promise, so it never sees the persist result directly. It must still
		// be bound by it.
		const started = localIndex.ensureProjectionScope(ws, true);
		const joined = localIndex.ensureAccessScope(ws, 'e2');
		release?.();
		await Promise.all([started, joined]);

		// Same answer as the starter gets — the point is that joining does not
		// produce a DIFFERENT verdict, not that the verdict is a hold.
		localIndex.markCaughtUp(ws, localIndex.reconcileTokenFor(ws));
		expect(localIndex.pendingResyncFor(ws)).toBe(false);
	});

	it('a capture that OVERLAPPED the resync stays refused; a fresh one clears at once', async () => {
		// The lead's hold — a refusal must not outlive its resync — plus review
		// round 2's P1, which showed the first version answered it at the wrong
		// moment. A loop that captured the epoch during a resync and whose
		// `/items-changes` response arrives AFTER that resync settles computed
		// its answer against the PRE-snapshot cursor. `projectionResyncs` no
		// longer holds the promise by then, so an in-flight check at clear time
		// cannot see the overlap; the epoch changing at BOTH ends of a resync
		// can, because any capture taken during one then mismatches.
		//
		// So the refusal is scoped to the stale VERDICT, not to the caller and
		// not to the workspace: the very next poll captures a fresh epoch and
		// clears immediately, which is what keeps this a guard rather than a
		// wedge.
		await boot();
		let release: (() => void) | undefined;
		vi.spyOn(api.items, 'listIndex').mockReturnValue(
			new Promise((resolve) => {
				release = () =>
					resolve({
						items: [row('cached', 1)],
						total: 1,
						cursor: '1',
						includes_unparented_metadata: true,
						access_epoch: 'e2',
					} as unknown as Awaited<ReturnType<typeof api.items.listIndex>>);
			}) as unknown as ReturnType<typeof api.items.listIndex>,
		);

		const inFlight = localIndex.ensureProjectionScope(ws, true);
		const capturedDuring = localIndex.reconcileTokenFor(ws);
		localIndex.markCaughtUp(ws, capturedDuring);
		expect(localIndex.pendingResyncFor(ws)).toBe(true);

		release?.();
		await inFlight;

		// The delayed response lands. Its verdict predates the pinned cursor and
		// must not clear the ask, even though nothing is in flight any more.
		localIndex.markCaughtUp(ws, capturedDuring);
		expect(localIndex.pendingResyncFor(ws)).toBe(true);

		// ...and the loop's next poll, capturing now, clears without ceremony.
		localIndex.markCaughtUp(ws, localIndex.reconcileTokenFor(ws));
		expect(localIndex.pendingResyncFor(ws)).toBe(false);
	});
});
