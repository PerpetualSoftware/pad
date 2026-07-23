import { describe, it, expect } from 'vitest';
import type { Item } from '$lib/types';
import { bucketByColumn, UNCATEGORIZED } from './boardColumns';

// Minimal Item-shaped fixture — bucketByColumn only reads `.fields` (via
// parseFields, which JSON.parses it) and `.id`.
const item = (id: string, fields: Record<string, unknown>): Item =>
	({ id, fields: JSON.stringify(fields) }) as unknown as Item;

const columns = ['open', 'in_progress', 'done'];

describe('bucketByColumn', () => {
	it('buckets items into their matching column', () => {
		const items = [
			item('a', { status: 'open' }),
			item('b', { status: 'done' }),
			item('c', { status: 'open' })
		];
		const result = bucketByColumn(items, 'status', columns);
		expect(result['open'].map((i) => i.id)).toEqual(['a', 'c']);
		expect(result['done'].map((i) => i.id)).toEqual(['b']);
		expect(result['in_progress']).toEqual([]);
	});

	it('always seeds every known column and the uncategorized bucket', () => {
		const result = bucketByColumn([], 'status', columns);
		expect(Object.keys(result).sort()).toEqual(
			[UNCATEGORIZED, 'done', 'in_progress', 'open'].sort()
		);
		expect(result[UNCATEGORIZED]).toEqual([]);
	});

	it('collects items with an empty group value into UNCATEGORIZED', () => {
		const items = [item('a', { status: '' }), item('b', { status: 'open' })];
		const result = bucketByColumn(items, 'status', columns);
		expect(result[UNCATEGORIZED].map((i) => i.id)).toEqual(['a']);
		expect(result['open'].map((i) => i.id)).toEqual(['b']);
	});

	it('treats a missing/null group value as uncategorized', () => {
		const items = [
			item('a', {}), // key absent
			item('b', { status: null }),
			item('c', { priority: 'high' }) // unrelated field only
		];
		const result = bucketByColumn(items, 'status', columns);
		expect(result[UNCATEGORIZED].map((i) => i.id)).toEqual(['a', 'b', 'c']);
	});

	it('collects items whose value is not a known option (stale/removed) into UNCATEGORIZED', () => {
		const items = [item('a', { status: 'archived' }), item('b', { status: 'open' })];
		const result = bucketByColumn(items, 'status', columns);
		expect(result[UNCATEGORIZED].map((i) => i.id)).toEqual(['a']);
		expect(result['open'].map((i) => i.id)).toEqual(['b']);
	});

	it('leaves the uncategorized bucket empty when every item is categorized', () => {
		const items = [item('a', { status: 'open' }), item('b', { status: 'done' })];
		const result = bucketByColumn(items, 'status', columns);
		expect(result[UNCATEGORIZED]).toEqual([]);
	});

	it('honours the chosen group field, not always status', () => {
		const items = [
			item('a', { status: 'open', impact: 'high' }),
			item('b', { status: 'open' }) // no impact → uncategorized under impact grouping
		];
		const result = bucketByColumn(items, 'impact', ['low', 'medium', 'high']);
		expect(result['high'].map((i) => i.id)).toEqual(['a']);
		expect(result[UNCATEGORIZED].map((i) => i.id)).toEqual(['b']);
	});
});
