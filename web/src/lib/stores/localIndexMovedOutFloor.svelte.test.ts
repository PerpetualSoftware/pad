import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '$lib/api/client';
import type { ItemIndexRow } from '$lib/types';

/**
 * TASK-2920 / PLAN-2903 item 6 — the eviction floor at the doors OTHER than the
 * cold `/items-index` merge.
 *
 * The cold door and its permanence are pinned by
 * `localIndexInverseColdCheck.svelte.test.ts`. This file covers the rest of the
 * population, because the failure this plan keeps repeating is a rule landing at
 * one door and not its sibling:
 *
 *   - the WARM IDB hydrate (the other `mergeRow` caller),
 *   - `upsert` (the optimistic write path),
 *   - `resyncProjectionScope`'s snapshot merge.
 *
 * Mocked persistence, like its sibling wiring files: jsdom has no IndexedDB, so
 * the hydrate has to be driven at the boundary.
 */

const persistence = vi.hoisted(() => {
	let gate: Promise<unknown> | null = null;
	let payload = {
		items: [] as ItemIndexRow[],
		cursor: '0',
		includesUnparentedMetadata: null as boolean | null,
		accessEpoch: null as string | null,
		durableRead: true,
		retags: {} as Record<string, string>,
	};
	return {
		__setHydrate(next: typeof payload, gatedOn: Promise<unknown> | null) {
			payload = next;
			gate = gatedOn;
		},
		hydrate: vi.fn(async () => {
			if (gate) await gate;
			return payload;
		}),
		persistDelta: vi.fn(async () => undefined),
		persistRemovals: vi.fn(async () => undefined),
		persistReplace: vi.fn(async () => true),
		persistAccessEpoch: vi.fn(async () => undefined),
		persistRetag: vi.fn(async () => undefined),
		persistUpserts: vi.fn(async () => undefined),
		wipe: vi.fn(async () => undefined),
	};
});
vi.mock('./localIndexPersistence', () => persistence);

const { localIndex } = await import('./localIndex.svelte');
const { localSearch } = await import('./localSearch.svelte');

const ws = 'moved-out-floor';

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

async function settle(): Promise<void> {
	for (let i = 0; i < 20; i++) await Promise.resolve();
}

/** A populated warm cache whose read is held open until the returned gate resolves. */
function gatedWarmCache(items: ItemIndexRow[], cursor: string) {
	const gate = deferred<void>();
	persistence.__setHydrate(
		{
			items,
			cursor,
			includesUnparentedMetadata: false,
			accessEpoch: 'epoch-1',
			durableRead: true,
			retags: {},
		},
		gate.promise,
	);
	return gate;
}

afterEach(() => {
	vi.restoreAllMocks();
	localIndex.reset(ws);
	persistence.__setHydrate(
		{
			items: [],
			cursor: '0',
			includesUnparentedMetadata: null,
			accessEpoch: null,
			durableRead: true,
			retags: {},
		},
		null,
	);
});

describe('the warm IDB hydrate — mergeRow’s other caller', () => {
	it('does not reinstate a row whose moved_out was applied while the durable cache was being read', async () => {
		const gate = gatedWarmCache([row('keeper', 1, 'kept'), row('secret', 40, 'revoked')], '95');
		// The reconcile that follows the hydrate must not itself resurrect
		// anything; it is not what this test is about.
		vi.spyOn(api.items, 'changes').mockResolvedValue({
			changes: [],
			cursor: '100',
			includes_unparented_metadata: false,
			access_epoch: 'epoch-1',
		});

		const boot = localIndex.bootstrap(ws, { userId: null });
		await settle();

		// The eviction lands while `hydrate` is still awaiting.
		localIndex.applyDelta(ws, [{ id: 'secret', seq: 100, moved_out: true }], '100', false);
		gate.resolve();
		await boot;

		expect(canSee('secret', 'revoked')).toBe(false);
		// The cached row that was NOT evicted still hydrates — without this the
		// assertion above would also pass on a hydrate that dropped everything.
		expect(canSee('keeper', 'kept')).toBe(true);
	});

	it('control: with no eviction in the window, the same cached row hydrates normally', async () => {
		const gate = gatedWarmCache([row('keeper', 1, 'kept'), row('secret', 40, 'revoked')], '95');
		vi.spyOn(api.items, 'changes').mockResolvedValue({
			changes: [],
			cursor: '100',
			includes_unparented_metadata: false,
			access_epoch: 'epoch-1',
		});

		const boot = localIndex.bootstrap(ws, { userId: null });
		await settle();
		// Same shape, same timing — an unrelated cursor advance rather than an
		// eviction, so any failure above is attributable to the eviction and not
		// to the gating.
		localIndex.applyDelta(ws, [], '100', false);
		gate.resolve();
		await boot;

		expect(canSee('secret', 'revoked')).toBe(true);
	});
});

describe('upsert — the optimistic write path', () => {
	async function bootWithEviction(): Promise<void> {
		vi.spyOn(api.items, 'listIndex').mockResolvedValueOnce({
			items: [row('secret', 40, 'revoked')],
			total: 1,
			cursor: '40',
			includes_unparented_metadata: false,
			access_epoch: 'epoch-1',
		});
		await localIndex.bootstrap(ws, { userId: null });
		expect(canSee('secret', 'revoked')).toBe(true);
		localIndex.applyDelta(ws, [{ id: 'secret', seq: 100, moved_out: true }], '100', false);
		expect(canSee('secret', 'revoked')).toBe(false);
	}

	it('refuses a stale optimistic response that predates the eviction', async () => {
		await bootWithEviction();
		// A create/update issued before the move, resolving after it. Neither
		// existing guard covers this: `fencedIds` holds only ids a RESYNC
		// dropped, and `sinceEpoch` bumps only on a resync.
		localIndex.upsert(ws, row('secret', 40, 'revoked'));
		expect(canSee('secret', 'revoked')).toBe(false);
	});

	it('admits a genuine re-add above the floor — this is a seq floor, not a blocklist', async () => {
		await bootWithEviction();
		// The discriminator for the whole design. If the fence were a blocklist,
		// this row would be refused too and the item could never come back.
		localIndex.upsert(ws, row('secret', 150, 'revoked'));
		expect(canSee('secret', 'revoked')).toBe(true);
	});

	it('admits a row carrying NO seq — there is no basis to order it against the floor', async () => {
		await bootWithEviction();
		// The deliberate choice in `refusedByMovedOut`, pinned so it stays a
		// choice rather than drifting. A server that stamps no `seq` (legacy /
		// mixed deployment) offers nothing the floor can compare against;
		// refusing on that evidence would drop the row permanently, which is the
		// worse of the two failures. `upsert` already resolves the same
		// ambiguity the same way for its own guard.
		const noSeq = {
			id: 'secret',
			title: 'Title secret',
			collection_slug: 'revoked',
			created_at: '2026-01-01T00:00:00Z',
			updated_at: '2026-01-01T00:00:00Z',
		} as ItemIndexRow;
		localIndex.upsert(ws, noSeq);
		expect(canSee('secret', 'revoked')).toBe(true);
	});

	it('refuses a row AT the floor: the eviction is the row’s change at that seq, not older news', async () => {
		await bootWithEviction();
		localIndex.upsert(ws, row('secret', 100, 'revoked'));
		expect(canSee('secret', 'revoked')).toBe(false);
	});
});

describe('the floor map is bounded (codex round 1 P2)', () => {
	/**
	 * The lift on the authoritative re-add paths prunes only ids that came BACK,
	 * which is never the case for an item moved out permanently — so the cap in
	 * `noteMovedOut` is the only thing standing between a long-lived tab and one
	 * entry per eviction forever. These two tests pin the cap and the direction
	 * it evicts in; without them the constant is a comment.
	 */
	const CAP = 5000;

	function evictions(from: number, count: number, idOf: (i: number) => string) {
		return Array.from({ length: count }, (_, i) => ({
			id: idOf(from + i),
			seq: from + i,
			moved_out: true as const,
		}));
	}

	async function bootEmpty(): Promise<void> {
		vi.spyOn(api.items, 'listIndex').mockResolvedValueOnce({
			items: [],
			total: 0,
			cursor: '0',
			includes_unparented_metadata: false,
			access_epoch: 'epoch-1',
		});
		await localIndex.bootstrap(ws, { userId: null });
	}

	it('evicts the OLDEST floor once the cap is exceeded, and keeps the rest', async () => {
		await bootEmpty();
		localIndex.applyDelta(ws, [{ id: 'oldest', seq: 1, moved_out: true }], '1', false);
		localIndex.applyDelta(ws, [{ id: 'second', seq: 2, moved_out: true }], '2', false);
		// Fill to exactly the cap, then push one past it.
		localIndex.applyDelta(ws, evictions(3, CAP - 2, (i) => `bulk-${i}`), String(CAP), false);
		localIndex.applyDelta(ws, [{ id: 'newest', seq: CAP + 1, moved_out: true }], String(CAP + 1), false);

		// The oldest floor is gone, so its id is admitted again — this is the
		// bound, observable from outside.
		localIndex.upsert(ws, row('oldest', 1, 'revoked'));
		expect(canSee('oldest', 'revoked')).toBe(true);
		// ...and nothing else went with it. Without this leg the assertion above
		// would also pass on a map that dropped everything.
		localIndex.upsert(ws, row('second', 2, 'revoked'));
		expect(canSee('second', 'revoked')).toBe(false);
		localIndex.upsert(ws, row('newest', CAP + 1, 'revoked'));
		expect(canSee('newest', 'revoked')).toBe(false);
	});

	it('an authoritative re-add frees its slot, so the cap evicts one fewer old floor', async () => {
		await bootEmpty();
		localIndex.applyDelta(ws, [{ id: 'oldest', seq: 1, moved_out: true }], '1', false);
		// Fill to EXACTLY the cap.
		localIndex.applyDelta(ws, evictions(2, CAP - 1, (i) => `bulk-${i}`), String(CAP), false);

		// The server says one of them is visible again. That lift is what this
		// test is about: it is otherwise unobservable — after a re-add the row's
		// own `existing.seq` guard subsumes everything the floor could refuse —
		// and it is precisely what makes room here.
		localIndex.applyDelta(
			ws,
			[row('bulk-5', CAP + 1, 'kept')],
			String(CAP + 1),
			false,
		);
		// One more eviction. With the lift the map is back at the cap and nothing
		// is dropped; without it the map would be one over and `oldest` would go.
		localIndex.applyDelta(ws, [{ id: 'newest', seq: CAP + 2, moved_out: true }], String(CAP + 2), false);

		localIndex.upsert(ws, row('oldest', 1, 'revoked'));
		expect(canSee('oldest', 'revoked')).toBe(false);
	});

	it('a floor RAISED in place keeps its position — a re-raise is not a fresh lease', async () => {
		await bootEmpty();
		localIndex.applyDelta(ws, [{ id: 'oldest', seq: 1, moved_out: true }], '1', false);
		localIndex.applyDelta(ws, [{ id: 'second', seq: 2, moved_out: true }], '2', false);
		// The same id evicted again at a higher seq. If this moved it to the end
		// of the map, `second` would become the oldest and be evicted below.
		localIndex.applyDelta(ws, [{ id: 'oldest', seq: 3, moved_out: true }], '3', false);
		localIndex.applyDelta(ws, evictions(4, CAP - 2, (i) => `bulk-${i}`), String(CAP + 1), false);
		localIndex.applyDelta(ws, [{ id: 'newest', seq: CAP + 2, moved_out: true }], String(CAP + 2), false);

		// `oldest` was still the oldest entry despite the re-raise, so it went.
		localIndex.upsert(ws, row('oldest', 3, 'revoked'));
		expect(canSee('oldest', 'revoked')).toBe(true);
		// `second` did not.
		localIndex.upsert(ws, row('second', 2, 'revoked'));
		expect(canSee('second', 'revoked')).toBe(false);
	});
});

describe('resyncProjectionScope — the same door under a different roof', () => {
	it('does not reinstate a row whose moved_out was applied while its snapshot was in flight', async () => {
		vi.spyOn(api.items, 'listIndex').mockResolvedValueOnce({
			items: [row('keeper', 1, 'kept'), row('secret', 40, 'revoked')],
			total: 2,
			cursor: '40',
			includes_unparented_metadata: false,
			access_epoch: 'epoch-1',
		});
		await localIndex.bootstrap(ws, { userId: null });

		const gate = deferred<Awaited<ReturnType<typeof api.items.listIndex>>>();
		vi.spyOn(api.items, 'listIndex').mockReturnValueOnce(gate.promise);
		const resync = localIndex.ensureAccessScope(ws, 'epoch-2');
		await settle();

		localIndex.applyDelta(ws, [{ id: 'secret', seq: 100, moved_out: true }], '100', false);
		gate.resolve({
			items: [row('keeper', 1, 'kept'), row('secret', 40, 'revoked')],
			total: 2,
			cursor: '95',
			includes_unparented_metadata: false,
			access_epoch: 'epoch-2',
		});
		await resync;

		// A resync self-heals this case via its cursor pin, so what this pins is
		// the absence of the FLICKER — the row is never reinstated in the first
		// place, rather than reinstated and re-evicted a round-trip later.
		expect(canSee('secret', 'revoked')).toBe(false);
		expect(canSee('keeper', 'kept')).toBe(true);
	});
});
