import { describe, it, expect } from 'vitest';
import type { Collection, Item } from '$lib/types';
import { itemComparator } from './itemSort';

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
	it('breaks a sort_order tie by created_at ascending, ignoring updated_at entirely', () => {
		const comparator = itemComparator('manual', collection);
		const older = { ...item('older', 0, '2026-01-01T00:00:00Z'), updated_at: '2026-07-04T00:00:00Z' } as Item;
		const newer = { ...item('newer', 0, '2026-02-01T00:00:00Z'), updated_at: '2026-01-01T00:00:00Z' } as Item;
		// `newer`'s updated_at is EARLIER than `older`'s, proving the
		// comparator ignores updated_at and still orders by created_at.
		expect([newer, older].sort(comparator).map((i) => i.id)).toEqual(['older', 'newer']);
		expect([older, newer].sort(comparator).map((i) => i.id)).toEqual(['older', 'newer']);
	});

	it('falls back to id when sort_order and created_at both tie', () => {
		const comparator = itemComparator('manual', collection);
		const b = item('b-item', 0, '2026-01-01T00:00:00Z');
		const a = item('a-item', 0, '2026-01-01T00:00:00Z');
		expect([b, a].sort(comparator).map((i) => i.id)).toEqual(['a-item', 'b-item']);
	});

	it('treats a missing/unparseable created_at as epoch (sorts first among ties)', () => {
		const comparator = itemComparator('manual', collection);
		const withDate = item('with-date', 0, '2026-01-01T00:00:00Z');
		const withoutDate = { ...item('without-date', 0, ''), created_at: undefined } as unknown as Item;
		expect([withDate, withoutDate].sort(comparator).map((i) => i.id)).toEqual([
			'without-date',
			'with-date',
		]);
	});
});
