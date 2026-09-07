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
