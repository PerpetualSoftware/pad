import { describe, it, expect } from 'vitest';
import type { Collection, Item } from '$lib/types';
import { itemComparator, isReorderable, type SortMode } from './itemSort';

// Minimal Item stand-in — the manual comparator only touches sort_order,
// created_at, and id.
function item(id: string, sort_order: number, created_at: string): Item {
	return { id, sort_order, created_at } as unknown as Item;
}

// Manual mode doesn't read the collection's schema, so an empty stand-in
// satisfies the comparator factory's signature.
const collection = {} as Collection;

describe('itemComparator manual mode', () => {
	it('orders primarily by sort_order', () => {
		const comparator = itemComparator('manual', collection);
		const items = [
			item('a', 2, '2026-01-01T00:00:00Z'),
			item('b', 0, '2026-01-01T00:00:00Z'),
			item('c', 1, '2026-01-01T00:00:00Z'),
		];
		expect([...items].sort(comparator).map((i) => i.id)).toEqual(['b', 'c', 'a']);
	});

	// BUG-1941: every un-dragged card in a lane shares sort_order = 0. A
	// stray updated_at bump (e.g. a spurious collab-flush PATCH) must not
	// reorder them — the tiebreak has to be independent of updated_at.
	// The tie direction is created_at ASC (oldest first): the server's order
	// for child lists and the dashboard, which render it as delivered
	// (BUG-3527). A new item joins the end of an un-dragged lane.
	it('breaks a sort_order tie by created_at ascending, ignoring updated_at entirely', () => {
		const comparator = itemComparator('manual', collection);
		const older = { ...item('older', 0, '2026-01-01T00:00:00Z'), updated_at: '2026-01-01T00:00:00Z' } as Item;
		const newer = { ...item('newer', 0, '2026-02-01T00:00:00Z'), updated_at: '2026-07-04T00:00:00Z' } as Item;
		// `newer`'s updated_at is LATER than `older`'s here, but the ordering
		// is driven purely by created_at (oldest first) — the older item
		// sorts ahead regardless of the updated_at values.
		expect([older, newer].sort(comparator).map((i) => i.id)).toEqual(['older', 'newer']);
		expect([newer, older].sort(comparator).map((i) => i.id)).toEqual(['older', 'newer']);
	});

	it('falls back to id when sort_order and created_at both tie', () => {
		const comparator = itemComparator('manual', collection);
		const b = item('b-item', 0, '2026-01-01T00:00:00Z');
		const a = item('a-item', 0, '2026-01-01T00:00:00Z');
		expect([b, a].sort(comparator).map((i) => i.id)).toEqual(['a-item', 'b-item']);
	});

	it('treats a missing/unparseable created_at as epoch (sorts first among ties under oldest-first)', () => {
		const comparator = itemComparator('manual', collection);
		const withDate = item('with-date', 0, '2026-01-01T00:00:00Z');
		const withoutDate = { ...item('without-date', 0, ''), created_at: undefined } as unknown as Item;
		expect([withDate, withoutDate].sort(comparator).map((i) => i.id)).toEqual([
			'without-date',
			'with-date',
		]);
	});
});


describe('reversed sorting (BUG-3994)', () => {
	const collection = { schema: JSON.stringify({ fields: [
		{ key: 'priority', type: 'select', options: ['high', 'medium', 'low'] },
	] }) } as Collection;
	const items = ['a', 'b', 'c'].map((id, i) => ({
		...item(id, i, `2026-0${i + 1}-01T00:00:00Z`), title: id,
		updated_at: `2026-0${i + 1}-01T00:00:00Z`,
		fields: JSON.stringify({ priority: ['high', 'medium', 'low'][i] }),
	}));
	for (const mode of ['manual', 'priority', 'updated', 'created', 'title'] as SortMode[]) {
		it(`reverses ${mode}`, () => {
			const normal = [...items].sort(itemComparator(mode, collection));
			expect([...items].sort(itemComparator(mode, collection, true))).toEqual(normal.reverse());
		});
	}
	it('reverses the creation tiebreak for never-dragged manual items', () => {
		const tied = items.map((i) => ({ ...i, sort_order: 0 }));
		expect(tied.sort(itemComparator('manual', collection, true)).map((i) => i.id)).toEqual(['c', 'b', 'a']);
	});
	it('keeps missing and unknown priorities last in both directions', () => {
		const missing = { ...items[0], id: 'missing', fields: '{}' };
		const unknown = { ...missing, id: 'unknown', fields: '{"priority":"other"}' };
		for (const reversed of [false, true]) {
			const sorted = [missing, ...items, unknown].sort(itemComparator('priority', collection, reversed));
			expect(sorted.slice(0, 3).map((i) => i.id)).toEqual(reversed ? ['c', 'b', 'a'] : ['a', 'b', 'c']);
			expect(sorted.slice(3).map((i) => i.id)).toEqual(['missing', 'unknown']);
		}
	});
	for (const mode of ['updated', 'created'] as const) {
		it(`keeps missing and invalid ${mode} timestamps last in both directions`, () => {
			const missing = { ...items[0], id: 'missing', [`${mode}_at`]: '' };
			const invalid = { ...missing, id: 'invalid', [`${mode}_at`]: 'invalid' };
			for (const reversed of [false, true]) {
				const sorted = [missing, invalid, ...items].sort(itemComparator(mode, collection, reversed));
				expect(sorted.slice(0, 3).map((i) => i.id)).toEqual(reversed ? ['a', 'b', 'c'] : ['c', 'b', 'a']);
				expect(new Set(sorted.slice(3).map((i) => i.id))).toEqual(new Set(['missing', 'invalid']));
			}
		});
	}
	it('only allows reordering in forward manual order', () => {
		expect(isReorderable('manual', false)).toBe(true);
		for (const mode of ['manual', 'priority', 'updated', 'created', 'title'] as SortMode[]) {
			expect(isReorderable(mode, true)).toBe(false);
			if (mode !== 'manual') expect(isReorderable(mode, false)).toBe(false);
		}
	});
});
