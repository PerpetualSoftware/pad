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
