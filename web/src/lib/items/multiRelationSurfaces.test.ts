import { describe, expect, it } from 'vitest';
import { isPublicGroupable } from '$lib/components/share/shareView';
import { isCollectable, uncollectableReason } from '$lib/items/copyNeedsValue';
import { FIELD_TYPES } from '$lib/components/collections/field-editor-types';

/**
 * The widened web surfaces, asserted through their OWN functions rather than
 * through the predicate they call (PLAN-2857 U4).
 *
 * A test on `isRelationType` proves the predicate; it proves nothing about
 * whether a given site calls it. These legs are the wiring half (CONVE-19), and
 * each is paired with the scalar control that would catch a change going too
 * far.
 */

describe('a public share refuses to group by either relation type', () => {
	const collection = (type: string) =>
		({
			fields: [{ key: 'owners', label: 'Owners', type }]
		}) as never;

	it('refuses multi_relation, as it already refused relation', () => {
		// A share payload carries field VALUES with no index behind them, which
		// is why grouping by a relation is refused at all. A multi_relation is
		// strictly worse — each value is an ARRAY of ids — so a shared view
		// grouped by one would render a group per stringified array.
		expect(isPublicGroupable(collection('multi_relation'), 'owners')).toBe(false);
		expect(isPublicGroupable(collection('relation'), 'owners')).toBe(false);
	});

	it('still ALLOWS an ordinary field — the control', () => {
		// Without this leg, a predicate that refused everything would pass the
		// test above and silently break every public board in the product.
		expect(isPublicGroupable(collection('select'), 'owners')).toBe(true);
		expect(isPublicGroupable(collection('multi_select'), 'owners')).toBe(true);
	});

	it('refuses a key the collection does not declare', () => {
		expect(isPublicGroupable(collection('select'), 'nope')).toBe(false);
	});
});

describe('the copy dialog treats a multi_relation as collectable', () => {
	it('offers a picker when the row names a target', () => {
		const row = { key: 'owners', type: 'multi_relation', collection: 'people' } as never;
		expect(isCollectable(row)).toBe(true);
		expect(uncollectableReason(row)).toBeNull();
	});

	it('refuses one naming NO target, exactly as a scalar relation does', () => {
		// Naming no target is a type-shaped failure: there is nothing to point
		// the user at, so the row is not collectable and the reason is `type`.
		const row = { key: 'owners', type: 'multi_relation' } as never;
		expect(isCollectable(row)).toBe(false);
		expect(uncollectableReason(row)).toBe('type');
	});

	it('reports an UNAVAILABLE target distinctly from a missing one', () => {
		// IDEA-2899's distinction has to survive the widening: naming a target
		// the caller cannot use is a different message from naming none.
		const row = {
			key: 'owners',
			type: 'multi_relation',
			collection: 'people',
			collection_unavailable: true
		} as never;
		expect(isCollectable(row)).toBe(false);
		expect(uncollectableReason(row)).toBe('unavailable_target');
	});

	it('leaves a non-relation row alone — the control', () => {
		const row = { key: 'labels', type: 'multi_select', options: ['a'] } as never;
		expect(uncollectableReason(row)).not.toBe('unavailable_target');
	});
});

describe('the schema editor can declare a multi_relation', () => {
	it('offers the type', () => {
		// A type the API accepts and the editor cannot declare is a half-shipped
		// surface: the only way to get one would be the CLI's `fields` DSL or a
		// hand-written schema.
		expect(FIELD_TYPES).toContain('multi_relation');
		// The control: the list did not lose anything while gaining one.
		expect(FIELD_TYPES).toContain('relation');
		expect(FIELD_TYPES).toContain('select');
	});
});
