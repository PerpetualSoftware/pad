import { describe, expect, it } from 'vitest';
import type { Activity } from '$lib/types';
import { collapseReorderBatches } from './reorderBatches';

const row = (id: string, action: string, batch?: string): Activity =>
	({
		id,
		action,
		metadata: batch === undefined ? '{}' : JSON.stringify({ reorder_batch: batch, sort_order_from: '1', sort_order_to: '2' })
	}) as unknown as Activity;

describe('collapseReorderBatches (TASK-3517)', () => {
	it('folds a run of one batch into its first row with a count', () => {
		const out = collapseReorderBatches([row('a', 'reordered', 'b1'), row('b', 'reordered', 'b1'), row('c', 'reordered', 'b1')]);
		expect(out.map((r) => [r.id, r.reorder_count])).toEqual([['a', 3]]);
	});

	it('keeps a single reorder row plain, and separate batches separate', () => {
		const out = collapseReorderBatches([row('a', 'reordered', 'b1'), row('b', 'reordered', 'b2'), row('c', 'reordered', 'b2')]);
		expect(out.map((r) => [r.id, r.reorder_count])).toEqual([
			['a', undefined],
			['b', 2]
		]);
	});

	it('does not merge across another row, and never touches other actions', () => {
		const out = collapseReorderBatches([
			row('a', 'reordered', 'b1'),
			row('x', 'updated'),
			row('b', 'reordered', 'b1'),
			row('y', 'updated'),
			row('z', 'updated')
		]);
		expect(out.map((r) => [r.id, r.reorder_count])).toEqual([
			['a', undefined],
			['x', undefined],
			['b', undefined],
			['y', undefined],
			['z', undefined]
		]);
	});

	it('does not mutate its input', () => {
		const input = [row('a', 'reordered', 'b1'), row('b', 'reordered', 'b1')];
		collapseReorderBatches(input);
		expect((input[0] as { reorder_count?: number }).reorder_count).toBeUndefined();
	});
});
