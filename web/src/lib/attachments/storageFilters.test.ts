import { describe, it, expect } from 'vitest';
import { buildStorageFilters, hasActiveStorageFilters } from './storageFilters';

// PLAN-2392 DR-18. The item attachment strip's "View all (N)" link hands off
// `?attachment_item=<uuid>`; StorageTab seeds `item_id` from it. These pin the
// receiving end's one non-obvious rule.

const base = { limit: 50, offset: 0 };

describe('buildStorageFilters', () => {
	it('passes an item scope through as item_id', () => {
		expect(buildStorageFilters({ ...base, itemId: 'item-uuid' })).toEqual({
			limit: 50,
			offset: 0,
			item_id: 'item-uuid',
		});
	});

	it('drops the attached/unattached selector while an item scope is set', () => {
		// item_id + item=unattached is a contradiction server-side and returns
		// an empty set — the scope must win rather than silently empty the list.
		const f = buildStorageFilters({ ...base, itemId: 'item-uuid', item: 'unattached' });
		expect(f.item_id).toBe('item-uuid');
		expect(f.item).toBeUndefined();
	});

	it('honours the attached/unattached selector once the scope is cleared', () => {
		const f = buildStorageFilters({ ...base, itemId: '', item: 'unattached' });
		expect(f.item).toBe('unattached');
		expect(f.item_id).toBeUndefined();
	});

	it('keeps the orthogonal filters alongside an item scope', () => {
		const f = buildStorageFilters({
			...base,
			itemId: 'item-uuid',
			category: 'image',
			collection: 'coll-1',
			sort: 'size_desc',
		});
		expect(f).toEqual({
			limit: 50,
			offset: 0,
			item_id: 'item-uuid',
			category: 'image',
			collection: 'coll-1',
			sort: 'size_desc',
		});
	});

	it('omits every empty selection', () => {
		expect(
			buildStorageFilters({ ...base, category: '', item: '', itemId: '', collection: '', sort: '' })
		).toEqual({ limit: 50, offset: 0 });
	});
});

describe('hasActiveStorageFilters', () => {
	it('is false with nothing selected', () => {
		expect(hasActiveStorageFilters(base)).toBe(false);
	});

	it('is true for an item scope, so the empty state says "no match" not "none exist"', () => {
		expect(hasActiveStorageFilters({ ...base, itemId: 'item-uuid' })).toBe(true);
	});

	it('is true for the orthogonal filters too', () => {
		expect(hasActiveStorageFilters({ ...base, category: 'image' })).toBe(true);
		expect(hasActiveStorageFilters({ ...base, collection: 'c' })).toBe(true);
		expect(hasActiveStorageFilters({ ...base, item: 'attached' })).toBe(true);
	});
});
