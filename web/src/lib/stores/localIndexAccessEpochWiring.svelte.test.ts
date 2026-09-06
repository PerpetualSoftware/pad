import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '$lib/api/client';
import type { ItemIndexRow } from '$lib/types';

/**
 * IDEA-2898 review round 2 — the WIRING assertions (CONVE-19).
 *
 * Three of round 2's findings are not about what a function decides but about
 * what it HANDS ON: the cold path repairing RAM while leaving the dropped row
 * in IDB, a resync writing `null` to the durable epoch while deliberately
 * keeping a known one in RAM, and a delta being applied without declaring the
 * epoch it was built under. Each is invisible to a behavioural test in jsdom,
 * because IDB is unsupported there and the persistence calls are no-ops — the
 * store's own tests would pass with every one of these arguments dropped.
 *
 * So this file mocks the persistence module and asserts the ARGUMENTS. That is
 * a weaker kind of test and it is the right one here: the question is whether
 * the caller passes the value, not whether the callee honours it (the callee is
 * pinned by localIndexPersistenceAccessEpoch.idb.test.ts against a real
 * database).
 */

const persistence = vi.hoisted(() => ({
	hydrate: vi.fn(async () => ({
		items: [],
		cursor: '0',
		includesUnparentedMetadata: null,
		accessEpoch: null,
		retags: {},
	})),
	persistDelta: vi.fn(async () => undefined),
	persistRemovals: vi.fn(async () => undefined),
	persistReplace: vi.fn(async () => undefined),
	persistRetag: vi.fn(async () => undefined),
	persistUpserts: vi.fn(async () => undefined),
	wipe: vi.fn(async () => undefined),
}));
vi.mock('./localIndexPersistence', () => persistence);

const { localIndex } = await import('./localIndex.svelte');

const ws = 'access-epoch-wiring';

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

afterEach(() => {
	vi.restoreAllMocks();
	for (const fn of Object.values(persistence)) fn.mockClear();
	localIndex.reset(ws);
});

describe('IDEA-2898 round 2 — what the callers hand on', () => {
	it('mirrors the cold snapshot into IDB with a REPLACE, not an append', async () => {
		// Round 3 replaced round 2's removeIds repair with the shape the resync
		// uses. `persistDelta` writes the rows it is given and leaves the rest
		// of the store alone, so a row that reached IDB before any `meta.sync`
		// row existed — an optimistic write on a first visit — is invisible to
		// any RAM-derived drop set and survives into the next warm boot under
		// the new epoch. A replace cannot have that hole, because the durable
		// copy simply becomes the snapshot.
		vi.spyOn(api.items, 'listIndex').mockResolvedValueOnce({
			items: [row('keeper', 1, 'kept')],
			total: 1,
			cursor: '1',
			includes_unparented_metadata: false,
			access_epoch: 'e2',
		});

		await localIndex.bootstrap(ws, { userId: null });

		expect(persistence.persistReplace).toHaveBeenCalled();
		expect(persistence.persistDelta).not.toHaveBeenCalled();
		const args = persistence.persistReplace.mock.calls.at(-1) as unknown[];
		expect((args[2] as { id: string }[]).map((r) => r.id)).toEqual(['keeper']);
		expect(args[5]).toBe('e2');
	});

	it('writes the PRESERVED baseline to IDB when a resync snapshot carries no epoch', async () => {
		// F3 keeps a known baseline in RAM when the snapshot has none. Writing
		// `null` to the durable copy would contradict it — and a null meta
		// epoch disables the cross-tab fence for every writer that follows,
		// because nobody can be stale relative to nothing.
		await localIndex.ensureAccessScope(ws, 'e1');
		localIndex.upsert(ws, row('keeper', 1, 'kept'));
		localIndex.applyDelta(ws, [], '1', false);

		vi.spyOn(api.items, 'listIndex').mockResolvedValueOnce({
			items: [row('keeper', 1, 'kept')],
			total: 1,
			cursor: '1',
			includes_unparented_metadata: false,
			// No access_epoch — an older server answering the snapshot.
		});
		await localIndex.ensureAccessScope(ws, 'e2');

		expect(persistence.persistReplace).toHaveBeenCalled();
		const args = persistence.persistReplace.mock.calls.at(-1) as unknown[];
		expect(args[5]).not.toBeNull();
		expect(args[5]).toBe(localIndex.accessEpochFor(ws));
	});

	it('turns a persistence layer that could not confirm the scope into a pending resync', async () => {
		// Round 4, and the wiring half of the ruling (CONVE-19). The
		// persistence layer decides it cannot confirm the cache's scope and
		// says so by returning true; that value is worth nothing unless the
		// store acts on it. A mutation run proved the gap rather than assuming
		// it — making `noteUnconfirmedWrite` a no-op left every other test in
		// this unit green, because they all assert what the DATABASE does.
		//
		// The ask is the whole repair under this design: the write left the
		// meta row alone, so the cache is honest but may be holding rows nobody
		// local can adjudicate, and only a resync can.
		persistence.persistUpserts.mockResolvedValueOnce(true as never);

		localIndex.upsert(ws, row('optimistic', 3, 'kept'));
		// The persistence call is fire-and-forget; let its promise settle.
		await Promise.resolve();
		await Promise.resolve();

		expect(localIndex.pendingResyncFor(ws)).toBe(true);
	});

	it('does NOT schedule a resync when the persistence layer confirmed the scope', async () => {
		// The control leg. Without it, a `noteUnconfirmedWrite` that set
		// `pendingResync` unconditionally would pass the test above and drag a
		// full authoritative resync behind every optimistic write in the app.
		persistence.persistUpserts.mockResolvedValueOnce(false as never);

		localIndex.upsert(ws, row('optimistic', 3, 'kept'));
		await Promise.resolve();
		await Promise.resolve();

		expect(localIndex.pendingResyncFor(ws)).toBe(false);
	});

	it('keeps the resync ask alive through a quiet poll, and answers it with a resync', async () => {
		// Round 5. The ask was `pendingResync`, which `markCaughtUp` clears the
		// moment a delta comes back empty — and an empty delta is not evidence
		// about whether a write that could not confirm the cache's scope was
		// right to land. The ask was being swallowed by an unrelated quiet
		// poll, after which a reload could treat the durable snapshot as
		// authoritative with the unconfirmed write still in it.
		// Bootstrap FIRST, so the workspace is in the `ready` state a real
		// session is in when this happens. Without that the second bootstrap
		// below never reaches its early-return guard, and the guard is then
		// untested — which a mutation run caught: removing `scopeUnconfirmed`
		// from that condition left this test green.
		const listIndex = vi.spyOn(api.items, 'listIndex').mockResolvedValue({
			items: [],
			total: 0,
			cursor: '3',
			includes_unparented_metadata: false,
			access_epoch: 'e1',
		});
		vi.spyOn(api.items, 'changes').mockResolvedValue({
			changes: [],
			cursor: '3',
			includes_unparented_metadata: false,
			access_epoch: 'e1',
		});
		await localIndex.bootstrap(ws, { userId: null });
		listIndex.mockClear();

		persistence.persistUpserts.mockResolvedValueOnce(true as never);
		localIndex.upsert(ws, row('optimistic', 3, 'kept'));
		await Promise.resolve();
		await Promise.resolve();
		expect(localIndex.scopeUnconfirmedFor(ws)).toBe(true);

		// A quiet poll clears the replay flag and MUST NOT clear the ask.
		localIndex.markCaughtUp(ws, localIndex.scopeEpochFor(ws));
		expect(localIndex.pendingResyncFor(ws)).toBe(false);
		expect(localIndex.scopeUnconfirmedFor(ws)).toBe(true);

		// And the ask is answered by an authoritative snapshot, not a replay —
		// which also proves bootstrap did not early-return on a `ready`
		// workspace whose replay flag is clear.
		await localIndex.bootstrap(ws, { userId: null });

		expect(listIndex).toHaveBeenCalled();
		expect(localIndex.scopeUnconfirmedFor(ws)).toBe(false);
	});

	it("declares the delta's epoch when the bootstrap loop applies one", async () => {
		// The scope check in that loop runs BEFORE `ensureAccessScope` awaits,
		// so a resync can land in between and the rows would then be applied
		// under a scope that no longer holds. The guard inside `applyDelta`
		// exists for that; this asserts the loop actually reaches it.
		//
		// Spying on the exported object is the point rather than a shortcut:
		// the loop calls `localIndex.applyDelta(...)` through that object, so
		// this observes exactly the call the guard would receive.
		persistence.hydrate.mockResolvedValueOnce({
			items: [row('keeper', 1, 'kept')],
			cursor: '1',
			includesUnparentedMetadata: false,
			accessEpoch: 'e1',
			retags: {},
		});
		vi.spyOn(api.items, 'changes')
			.mockResolvedValueOnce({
				changes: [{ ...row('fresh', 2, 'kept'), deleted: false } as never],
				cursor: '2',
				includes_unparented_metadata: false,
				access_epoch: 'e1',
			})
			.mockResolvedValue({
				changes: [],
				cursor: '2',
				includes_unparented_metadata: false,
				access_epoch: 'e1',
			});
		const applySpy = vi.spyOn(localIndex, 'applyDelta');

		await localIndex.bootstrap(ws, { userId: null });

		expect(applySpy).toHaveBeenCalled();
		expect(applySpy.mock.calls.at(-1)?.[4]).toBe('e1');
	});
});
