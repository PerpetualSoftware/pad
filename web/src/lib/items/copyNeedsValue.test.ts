import { describe, it, expect } from 'vitest';
import { isCollectable, COLLECTABLE_TYPES } from './copyNeedsValue';
import type { ItemCopyPreflightNeedsValue } from '$lib/types';

const row = (over: Partial<ItemCopyPreflightNeedsValue>): ItemCopyPreflightNeedsValue => ({
	key: 'owner_ref',
	required: true,
	reason: 'missing_required',
	...over,
});

describe('isCollectable', () => {
	it('accepts a relation that names its target collection', () => {
		expect(isCollectable(row({ type: 'relation', collection: 'people-b' }))).toBe(true);
	});

	// THE NEGATIVE LEG of TASK-2869's proving test, and the reason this module
	// exists. FieldEditor gates its relation branch on wsSlug AND
	// field.collection, so a relation row with no collection cannot be filled
	// in — offering it would render a dead control, or an unscoped picker
	// listing SOURCE-workspace items the copy cannot point at.
	it('refuses a relation with no target collection', () => {
		expect(isCollectable(row({ type: 'relation' }))).toBe(false);
		expect(isCollectable(row({ type: 'relation', collection: '' }))).toBe(false);
	});

	it('is unaffected for every other type', () => {
		for (const type of COLLECTABLE_TYPES) {
			expect(isCollectable(row({ type })), `${type} should stay collectable`).toBe(true);
		}
		// The two FieldEditor cannot safely collect, named individually rather
		// than as "everything else" so a new dangerous type does not join them
		// silently.
		expect(isCollectable(row({ type: 'multi_select' }))).toBe(false);
		expect(isCollectable(row({ type: 'json' }))).toBe(false);
	});

	it('treats a missing type as text, which is collectable', () => {
		expect(isCollectable(row({}))).toBe(true);
	});
});
