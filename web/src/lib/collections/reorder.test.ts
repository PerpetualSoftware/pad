import { describe, it, expect } from 'vitest';
import type { Item } from '$lib/types';
import { reorderGroup, reorderedList, disabledDirections } from './reorder';

// Minimal Item stand-in — reorder only touches `id` and `sort_order`.
function item(id: string, sort_order: number): Item {
	return { id, sort_order } as unknown as Item;
}

// Build a group whose sort_order matches its index (the steady state after
// any prior reorder).
function dense(ids: string[]): Item[] {
	return ids.map((id, i) => item(id, i));
}

describe('reorderedList', () => {
	it('moves an item to the top', () => {
		const g = dense(['a', 'b', 'c', 'd']);
		expect(reorderedList(g, 'c', 'top').map((i) => i.id)).toEqual(['c', 'a', 'b', 'd']);
	});

	it('moves an item to the bottom', () => {
		const g = dense(['a', 'b', 'c', 'd']);
		expect(reorderedList(g, 'b', 'bottom').map((i) => i.id)).toEqual(['a', 'c', 'd', 'b']);
	});

	it('moves up and down by one', () => {
		const g = dense(['a', 'b', 'c', 'd']);
		expect(reorderedList(g, 'c', 'up').map((i) => i.id)).toEqual(['a', 'c', 'b', 'd']);
		expect(reorderedList(g, 'c', 'down').map((i) => i.id)).toEqual(['a', 'b', 'd', 'c']);
	});

	it('returns the same reference on a no-op (already at edge / unknown id)', () => {
		const g = dense(['a', 'b', 'c']);
		expect(reorderedList(g, 'a', 'up')).toBe(g);
		expect(reorderedList(g, 'a', 'top')).toBe(g);
		expect(reorderedList(g, 'c', 'down')).toBe(g);
		expect(reorderedList(g, 'zzz', 'top')).toBe(g);
	});
});

describe('reorderGroup', () => {
	it('returns only the rows whose sort_order changed', () => {
		const g = dense(['a', 'b', 'c', 'd']);
		// Move c up: c and b swap → exactly two rows change.
		const updates = reorderGroup(g, 'c', 'up');
		expect(updates.map((u) => [u.item.id, u.sort_order])).toEqual([
			['c', 1],
			['b', 2]
		]);
	});

	it('reindexes densely on move-to-top (every later row shifts)', () => {
		const g = dense(['a', 'b', 'c', 'd']);
		const updates = reorderGroup(g, 'd', 'top');
		// d→0, then a/b/c each shift down one.
		expect(updates.map((u) => [u.item.id, u.sort_order])).toEqual([
			['d', 0],
			['a', 1],
			['b', 2],
			['c', 3]
		]);
	});

	it('is collision-safe when every item shares a sort_order', () => {
		// A freshly-created collection: all sort_order === 0, displayed in
		// array order. A naive neighbour swap would be a visual no-op; the
		// dense reindex must still produce distinct, ordered values.
		const g = [item('a', 0), item('b', 0), item('c', 0)];
		const updates = reorderGroup(g, 'c', 'top');
		// New order [c, a, b] reindexes to c=0, a=1, b=2. c was already 0, so
		// it's (correctly) omitted; a and b shift down to make room. The net
		// persisted state is distinct & ordered — c floats to the top.
		expect(updates.map((u) => [u.item.id, u.sort_order])).toEqual([
			['a', 1],
			['b', 2]
		]);
	});

	it('returns no updates for a no-op move', () => {
		const g = dense(['a', 'b', 'c']);
		expect(reorderGroup(g, 'a', 'up')).toEqual([]);
		expect(reorderGroup(g, 'c', 'bottom')).toEqual([]);
	});
});

describe('disabledDirections', () => {
	it('disables up/top for the first item', () => {
		const d = disabledDirections(0, 4);
		expect([...d].sort()).toEqual(['top', 'up']);
	});

	it('disables down/bottom for the last item', () => {
		const d = disabledDirections(3, 4);
		expect([...d].sort()).toEqual(['bottom', 'down']);
	});

	it('disables every direction for a lone item', () => {
		const d = disabledDirections(0, 1);
		expect([...d].sort()).toEqual(['bottom', 'down', 'top', 'up']);
	});

	it('disables nothing for a middle item', () => {
		expect(disabledDirections(1, 4).size).toBe(0);
	});
});
