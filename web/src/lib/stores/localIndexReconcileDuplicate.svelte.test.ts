import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '$lib/api/client';
import type { ItemIndexRow } from '$lib/types';
import { localIndex } from './localIndex.svelte';

/**
 * BUG-3207 — the sync cursor is now stamped BEFORE a reload's reads, so a
 * change committed during the reload is DELIVERED TWICE: once in the reload,
 * once in the next incremental. The layout answers every sync result with
 * `localIndex.reconcile`, so the claim that makes the duplicate harmless is
 * this store's: a row it already holds, arriving again, changes nothing, and an
 * OLDER copy arriving late does not roll it back. Checked here rather than
 * assumed from the "per-row seq guards" comment in the layout.
 */

const ws = 'reconcile-duplicate';

function row(id: string, seq: number, title: string): ItemIndexRow {
	return {
		id,
		seq,
		title,
		collection_slug: 'tasks',
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
	} as ItemIndexRow;
}

function delta(rows: ItemIndexRow[], cursor: string) {
	return { changes: rows, cursor, includes_unparented_metadata: false, access_epoch: 'e1' };
}

async function boot() {
	vi.spyOn(api.items, 'listIndex').mockResolvedValueOnce({
		items: [row('a', 1, 'original'), row('b', 2, 'other')],
		total: 2,
		cursor: '10',
		includes_unparented_metadata: false,
		access_epoch: 'e1',
	});
	await localIndex.bootstrap(ws, { userId: null });
	vi.mocked(api.items.listIndex).mockRestore();
}

const view = () =>
	localIndex
		.getByCollection(ws, 'tasks')
		.map((r) => `${r.id}:${r.seq}:${r.title}`)
		.sort();

afterEach(() => {
	vi.restoreAllMocks();
	localIndex.reset(ws);
});

describe('a duplicate delivery leaves the index unchanged (BUG-3207)', () => {
	// Idempotent by CONTENT: re-applying an identical row changes nothing, so no
	// guard mutant can turn this red (measured). It pins the outcome the
	// duplicate produces, not a mechanism.
	it('the same row twice applies once', async () => {
		await boot();
		// Caught up by default: a pass keeps reading until a delta is empty.
		const changes = vi.spyOn(api.items, 'changes').mockResolvedValue(delta([], '11'));
		changes.mockResolvedValueOnce(delta([row('a', 11, 'renamed')], '11'));
		expect(await localIndex.reconcile(ws)).toBe(true);
		const once = view();
		expect(once).toEqual(['a:11:renamed', 'b:2:other']);

		// The duplicate: the same row, delivered again.
		changes.mockResolvedValueOnce(delta([row('a', 11, 'renamed')], '11'));
		expect(await localIndex.reconcile(ws)).toBe(true);
		expect(view()).toEqual(once);
	});

	it('an OLDER copy of a row arriving after the newer one does not roll it back', async () => {
		await boot();
		// Caught up by default: a pass keeps reading until a delta is empty.
		const changes = vi.spyOn(api.items, 'changes').mockResolvedValue(delta([], '11'));
		changes.mockResolvedValueOnce(delta([row('a', 11, 'renamed')], '11'));
		await localIndex.reconcile(ws);
		// An ADVANCED cursor, so the batch is not dropped whole. The row then
		// meets applyDelta's per-row guards, the cursor floor and the existing
		// row's seq; each alone holds it (measured: removing both turns this red,
		// removing either one does not).
		changes.mockResolvedValueOnce(delta([row('a', 1, 'original')], '12'));
		await localIndex.reconcile(ws);
		expect(view()).toEqual(['a:11:renamed', 'b:2:other']);
	});
});
