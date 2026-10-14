import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from '$lib/api/client';
import type { ItemIndexRow } from '$lib/types';
import { localIndex } from './localIndex.svelte';

/**
 * BUG-3699 (Heron's ruling): a reorder whose read-backs all fail marks the
 * scope stale, and the next reconcile, which every focus, reconnect and SSE
 * nudge already runs, installs a FULL snapshot instead of a delta from a
 * cursor that may be past the rows in question. Against the real store.
 */

const ws = 'scope-stale';

function row(id: string, seq: number | undefined, sort_order: number): ItemIndexRow {
	return {
		id,
		seq,
		sort_order,
		title: id,
		collection_slug: 'tasks',
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
	} as ItemIndexRow;
}

function snapshot(rows: ItemIndexRow[]) {
	return { items: rows, total: rows.length, cursor: '10', includes_unparented_metadata: false, access_epoch: 'e1' };
}

const caughtUp = { changes: [], cursor: '10', includes_unparented_metadata: false, access_epoch: 'e1' };

async function boot() {
	vi.spyOn(api.items, 'listIndex').mockResolvedValueOnce(snapshot([row('a', 1, 0), row('b', 2, 1)]));
	await localIndex.bootstrap(ws, { userId: null });
	vi.mocked(api.items.listIndex).mockRestore();
	vi.spyOn(api.items, 'changes').mockResolvedValue(caughtUp);
}

const order = () =>
	localIndex
		.getByCollection(ws, 'tasks')
		.map((r) => `${r.id}:${r.sort_order}:${r.seq ?? '-'}`)
		.sort();

afterEach(() => {
	vi.restoreAllMocks();
	localIndex.reset(ws);
});

describe('a stale scope resyncs in full on the next reconcile (BUG-3699)', () => {
	it('control: without a stale ask, a reconcile reads only the delta', async () => {
		await boot();
		const index = vi.spyOn(api.items, 'listIndex');
		expect(await localIndex.reconcile(ws)).toBe(true);
		expect(index).not.toHaveBeenCalled();
		expect(localIndex.scopeStaleFor(ws)).toBe(false);
	});

	it('a stale ask makes the next reconcile install the server order, and is then answered', async () => {
		await boot();
		// An unconfirmed local order the delta cannot fix: the cursor is past it.
		localIndex.upsert(ws, row('b', undefined, -5));
		localIndex.markScopeStale(ws);
		expect(localIndex.scopeStaleFor(ws)).toBe(true);
		const index = vi.spyOn(api.items, 'listIndex').mockResolvedValueOnce(snapshot([row('a', 1, 0), row('b', 12, 3)]));
		expect(await localIndex.reconcile(ws)).toBe(true);
		expect(index).toHaveBeenCalledTimes(1);
		expect(order(), 'the reconcile did not converge on the server order').toEqual(['a:0:1', 'b:3:12']);
		expect(localIndex.scopeStaleFor(ws), 'the ask outlived its resync').toBe(false);
		// Answered: the next reconcile is a delta again.
		expect(await localIndex.reconcile(ws)).toBe(true);
		expect(index).toHaveBeenCalledTimes(1);
	});

	it('a resync already in flight when the ask is made does not answer it', async () => {
		await boot();
		let first!: (v: unknown) => void;
		const index = vi
			.spyOn(api.items, 'listIndex')
			.mockImplementationOnce(() => new Promise((r) => (first = r)) as never)
			.mockResolvedValueOnce(snapshot([row('a', 1, 0), row('b', 12, 3)]));
		// A projection resync starts for its own reason (a capability change).
		const earlier = localIndex.ensureProjectionScope(ws, true);
		await vi.waitFor(() => {
			if (!first) throw new Error('resync not started');
		});
		// The uncertain reorder asks while that snapshot is in flight.
		localIndex.markScopeStale(ws);
		const pass = localIndex.reconcile(ws);
		first(snapshot([row('a', 1, 0), row('b', 2, 1)]));
		await earlier;
		expect(await pass).toBe(true);
		expect(index, 'the pre-ask snapshot answered the ask').toHaveBeenCalledTimes(2);
		expect(order()).toEqual(['a:0:1', 'b:3:12']);
		expect(localIndex.scopeStaleFor(ws)).toBe(false);
	});

	it('a failed snapshot keeps the ask for the next reconcile', async () => {
		await boot();
		localIndex.markScopeStale(ws);
		const index = vi.spyOn(api.items, 'listIndex').mockRejectedValueOnce(new Error('network'));
		await expect(localIndex.reconcile(ws)).rejects.toThrow('network');
		expect(localIndex.scopeStaleFor(ws), 'a failed resync answered the ask').toBe(true);
		index.mockResolvedValueOnce(snapshot([row('a', 1, 0), row('b', 12, 3)]));
		expect(await localIndex.reconcile(ws)).toBe(true);
		expect(localIndex.scopeStaleFor(ws)).toBe(false);
		expect(order()).toEqual(['a:0:1', 'b:3:12']);
	});
});
