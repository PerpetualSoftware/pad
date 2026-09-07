import { afterEach, describe, expect, it, vi } from 'vitest';
import { api, PadApiError } from '$lib/api/client';
import type { ItemIndexRow } from '$lib/types';
import { localIndex } from './localIndex.svelte';

/**
 * TASK-2921 — what the error banner reads, now that the page no longer owns it.
 *
 * The collection route used to keep a private `deltaSyncFailed` flag because its
 * auth-error reaction was `localIndex.reset(ws)`, which DELETES the state entry
 * and with it the `'error'` bootstrapState the banner reads. The reaction is now
 * `dropCacheForAuthError`, shared by both doors, which clears in place — so
 * `indexError` (`bootstrapState === 'error'`) covers the case on its own, from
 * every route rather than one.
 *
 * The banner is `indexError && items.length === 0`, so these tests assert
 * `bootstrapStateFor` and the row count: they are the banner, one layer down.
 *
 * TWO NEGATIVE LEGS ARE THE POINT. A cap hit and a transient failure must NOT
 * raise it — that is today's behaviour and this change deliberately does not
 * widen it. (The ruling anticipated the banner deriving from `pendingResyncFor`,
 * which is set in both those cases; that would have made the banner appear where
 * it never has. See the trail — the divergence is deliberate and declared.)
 */

const ws = 'auth-drop';

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

async function boot(): Promise<void> {
	const listIndex = vi.spyOn(api.items, 'listIndex');
	listIndex.mockResolvedValueOnce({
		items: [row('keeper', 1, 'kept')],
		total: 1,
		cursor: '10',
		includes_unparented_metadata: false,
		access_epoch: 'epoch-1',
	});
	await localIndex.bootstrap(ws, { userId: null });
	listIndex.mockRestore();
}

afterEach(() => {
	vi.restoreAllMocks();
	localIndex.reset(ws);
});

describe('the reconcile’s auth-error reaction', () => {
	it('APPEARS: a 403 clears the rows and leaves the state in error', async () => {
		await boot();
		vi.spyOn(api.items, 'changes').mockRejectedValue(
			new PadApiError({ code: 'forbidden', message: 'forbidden' }),
		);

		await expect(localIndex.reconcile(ws)).rejects.toThrow();

		// Both halves of the banner's condition.
		expect(localIndex.bootstrapStateFor(ws)).toBe('error');
		expect(localIndex.getByCollection(ws, 'kept')).toHaveLength(0);
	});

	it('survives the GLOBAL 403 handler having already reset the workspace', async () => {
		// THE CASE THE OTHER TESTS IN THIS FILE CANNOT SEE, because they mock
		// `api.items.changes` and so never go through `api.request()`.
		//
		// On a 403, `request()` fires the registered access-revoked handler
		// BEFORE the error reaches any caller, and that handler (registered in
		// the root layout) calls `localIndex.reset(scope.workspace)` — which
		// DELETES the workspace state entry. So by the time the reconcile's catch
		// runs, the `state` it holds is detached: every field it writes lands on
		// an object nobody will ever read, and `bootstrapStateFor` answers
		// 'cold'. A UI keying the banner off that state stays on "Loading…"
		// forever. Found by review, not by this suite, and this is the test that
		// closes that gap.
		await boot();
		vi.spyOn(api.items, 'changes').mockImplementation(async () => {
			// Exactly what request() does, in order.
			localIndex.reset(ws);
			throw new PadApiError({ code: 'forbidden', message: 'forbidden' });
		});

		await expect(localIndex.reconcile(ws)).rejects.toThrow();

		// The signal has to live somewhere the reset cannot take with it.
		expect(localIndex.accessRevokedFor(ws)).toBe(true);
	});

	it('BOOTSTRAP’s cold path: a 403 on /items-index still records the revocation', async () => {
		// The likeliest path of all — the first load after a revocation — and the
		// one the round-1 fix did NOT cover. Both of bootstrap's catches checked
		// `isStale()` BEFORE the auth branch, and the global handler's reset is
		// exactly what makes `isStale()` true, so the revocation was swallowed
		// into a cold state with no banner. Found by review round 2.
		vi.spyOn(api.items, 'listIndex').mockImplementation(async () => {
			localIndex.reset(ws);
			throw new PadApiError({ code: 'forbidden', message: 'forbidden' });
		});

		await expect(localIndex.bootstrap(ws, { userId: null })).rejects.toThrow();

		expect(localIndex.accessRevokedFor(ws)).toBe(true);
	});

	it('BOOTSTRAP’s reconcile loop: a 403 mid-catch-up still records the revocation', async () => {
		// This must drive BOOTSTRAP, not `reconcile` — they have separate
		// catches, and an earlier draft of this test called `reconcile` while
		// claiming to cover bootstrap's. A mutant restoring the old ordering in
		// bootstrap's inner catch SURVIVED it, which is how the mislabelling
		// surfaced.
		await boot();

		// Reach bootstrap's REENTRY path, the one that runs the reconcile loop:
		// an outstanding ask makes `bootstrap` proceed instead of no-opping, and
		// a reentry skips the hydrate and goes straight to the loop.
		vi.spyOn(api.items, 'listIndex').mockResolvedValue({
			items: [row('keeper', 1, 'kept')],
			total: 1,
			cursor: '10',
			includes_unparented_metadata: false,
			access_epoch: 'epoch-2',
		});
		vi.spyOn(api.items, 'changes').mockResolvedValue({
			changes: [],
			cursor: '10',
			includes_unparented_metadata: false,
			access_epoch: 'epoch-2',
		});
		await localIndex.ensureAccessScope(ws, 'epoch-2');
		expect(localIndex.pendingResyncFor(ws)).toBe(true);

		vi.spyOn(api.items, 'changes').mockImplementation(async () => {
			localIndex.reset(ws);
			throw new PadApiError({ code: 'forbidden', message: 'forbidden' });
		});

		await expect(localIndex.bootstrap(ws, { userId: null })).rejects.toThrow();

		expect(localIndex.accessRevokedFor(ws)).toBe(true);
	});

	it('reacts exactly ONCE when the error crosses two catches', async () => {
		// bootstrap's inner catch rethrows an auth error and its outer catch owns
		// the reaction. An earlier draft reacted in both, so one 403 ran
		// `markWorkspaceDropped`, `localSearch.reset` and an async `persistWipe`
		// TWICE — and a retry could race the second wipe against freshly
		// persisted rows (codex round 3 P2).
		//
		// `resetGenerationFor` counts drops, so it is the observable: one
		// revocation, one drop of ours. The global handler's own `reset` is the
		// other one, and it is fired by the test's mock exactly once, so the
		// delta across the call is what discriminates.
		await boot();
		vi.spyOn(api.items, 'listIndex').mockResolvedValue({
			items: [row('keeper', 1, 'kept')],
			total: 1,
			cursor: '10',
			includes_unparented_metadata: false,
			access_epoch: 'epoch-2',
		});
		vi.spyOn(api.items, 'changes').mockResolvedValue({
			changes: [],
			cursor: '10',
			includes_unparented_metadata: false,
			access_epoch: 'epoch-2',
		});
		await localIndex.ensureAccessScope(ws, 'epoch-2');

		const before = localIndex.resetGenerationFor(ws);
		vi.spyOn(api.items, 'changes').mockImplementation(async () => {
			localIndex.reset(ws); // the global handler's half: one drop
			throw new PadApiError({ code: 'forbidden', message: 'forbidden' });
		});
		await expect(localIndex.bootstrap(ws, { userId: null })).rejects.toThrow();

		// One drop from the global handler + one from our single reaction.
		expect(localIndex.resetGenerationFor(ws) - before).toBe(2);
	});

	it('a fresh bootstrap clears the revoked marker, so the Retry CTA works', async () => {
		await boot();
		vi.spyOn(api.items, 'changes').mockImplementation(async () => {
			localIndex.reset(ws);
			throw new PadApiError({ code: 'forbidden', message: 'forbidden' });
		});
		await expect(localIndex.reconcile(ws)).rejects.toThrow();
		expect(localIndex.accessRevokedFor(ws)).toBe(true);

		vi.restoreAllMocks();
		localIndex.reset(ws);
		await boot();

		expect(localIndex.accessRevokedFor(ws)).toBe(false);
	});

	it('rethrows, so the caller’s /login redirect still fires', async () => {
		await boot();
		vi.spyOn(api.items, 'changes').mockRejectedValue(
			new PadApiError({ code: 'unauthorized', message: 'unauthorized' }),
		);
		await expect(localIndex.reconcile(ws)).rejects.toMatchObject({ code: 'unauthorized' });
	});

	it('DOES NOT APPEAR: a transient failure leaves the cache standing and the ask set', async () => {
		await boot();
		vi.spyOn(api.items, 'changes').mockRejectedValue(new Error('network'));

		await expect(localIndex.reconcile(ws)).rejects.toThrow();

		expect(localIndex.bootstrapStateFor(ws)).toBe('ready');
		expect(localIndex.getByCollection(ws, 'kept')).toHaveLength(1);
	});

	it('DOES NOT APPEAR: a page-cap hit leaves the cache standing', async () => {
		await boot();
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
		expect(localIndex.bootstrapStateFor(ws)).toBe('ready');
	});

	it('CLEARS: a successful bootstrap after the drop returns the state to ready', async () => {
		await boot();
		vi.spyOn(api.items, 'changes').mockRejectedValue(
			new PadApiError({ code: 'forbidden', message: 'forbidden' }),
		);
		await expect(localIndex.reconcile(ws)).rejects.toThrow();
		expect(localIndex.bootstrapStateFor(ws)).toBe('error');

		// The banner's Retry CTA: reset, then bootstrap.
		vi.restoreAllMocks();
		localIndex.reset(ws);
		await boot();

		expect(localIndex.bootstrapStateFor(ws)).toBe('ready');
		expect(localIndex.getByCollection(ws, 'kept')).toHaveLength(1);
	});
});
