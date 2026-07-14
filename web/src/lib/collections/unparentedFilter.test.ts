import { describe, it, expect } from 'vitest';
import {
	UNPARENTED_FILTER_FIELD,
	buildUnparentedViewFilter,
	clearParentFilter,
	filtersSetParent,
	readUnparentedParam,
	unparentedEffective,
	viewHasUnparentedFilter,
	writeUnparentedParam,
} from './unparentedFilter';

describe('filtersSetParent', () => {
	it('is true when the patch carries a non-empty parent value', () => {
		expect(filtersSetParent({ parent: 'plan-1' })).toBe(true);
	});

	it('is false when parent is absent or empty', () => {
		expect(filtersSetParent({})).toBe(false);
		expect(filtersSetParent({ parent: '' })).toBe(false);
		expect(filtersSetParent({ status: 'open' })).toBe(false);
	});
});

describe('clearParentFilter', () => {
	it('drops the parent key, preserving other filters', () => {
		const result = clearParentFilter({ parent: 'plan-1', status: 'open' });
		expect(result).toEqual({ status: 'open' });
	});

	it('returns the same reference when there is nothing to clear (no-op)', () => {
		const filters = { status: 'open' };
		expect(clearParentFilter(filters)).toBe(filters);
	});
});

describe('unparentedEffective', () => {
	it('applies only when both intent and metadata availability are true', () => {
		expect(unparentedEffective(true, true)).toBe(true);
		expect(unparentedEffective(true, false)).toBe(false);
		expect(unparentedEffective(false, true)).toBe(false);
		expect(unparentedEffective(false, false)).toBe(false);
	});
});

describe('buildUnparentedViewFilter', () => {
	it('emits the reserved $unparented condition when active and available', () => {
		expect(buildUnparentedViewFilter(true, true)).toEqual({
			field: UNPARENTED_FILTER_FIELD,
			op: 'eq',
			value: true,
		});
	});

	it('is null when inactive', () => {
		expect(buildUnparentedViewFilter(false, true)).toBeNull();
	});

	// DR-2: a restricted caller can't legitimately have flipped the chip, but
	// a stale intent carried over from a URL/prior session must never be
	// persisted into a saved view.
	it('is null when metadata is unavailable, even if the intent flag is stuck true', () => {
		expect(buildUnparentedViewFilter(true, false)).toBeNull();
	});
});

describe('viewHasUnparentedFilter', () => {
	it('detects the reserved condition among other filters', () => {
		expect(
			viewHasUnparentedFilter([
				{ field: 'status', op: 'eq', value: 'open' },
				{ field: UNPARENTED_FILTER_FIELD, op: 'eq', value: true },
			]),
		).toBe(true);
	});

	it('is false when absent, undefined, or malformed (wrong op/value)', () => {
		expect(viewHasUnparentedFilter(undefined)).toBe(false);
		expect(viewHasUnparentedFilter([])).toBe(false);
		expect(viewHasUnparentedFilter([{ field: UNPARENTED_FILTER_FIELD, op: 'in', value: true }])).toBe(
			false,
		);
		expect(
			viewHasUnparentedFilter([{ field: UNPARENTED_FILTER_FIELD, op: 'eq', value: 'true' }]),
		).toBe(false);
	});

	// Grandfathered-schema guard: a REAL field literally named `unparented`
	// (no `$`) must never be mistaken for the reserved discriminator.
	it('does not match a grandfathered field literally named "unparented" (no $)', () => {
		expect(viewHasUnparentedFilter([{ field: 'unparented', op: 'eq', value: true }])).toBe(false);
	});
});

describe('URL round-trip', () => {
	it('reads unparented=true from search params', () => {
		expect(readUnparentedParam(new URLSearchParams('unparented=true'))).toBe(true);
	});

	it('treats any other value (or absence) as false', () => {
		expect(readUnparentedParam(new URLSearchParams(''))).toBe(false);
		expect(readUnparentedParam(new URLSearchParams('unparented=false'))).toBe(false);
		expect(readUnparentedParam(new URLSearchParams('unparented=1'))).toBe(false);
	});

	it('writes the param when active and removes it when not', () => {
		const params = new URLSearchParams();
		writeUnparentedParam(params, true);
		expect(params.get('unparented')).toBe('true');

		writeUnparentedParam(params, false);
		expect(params.has('unparented')).toBe(false);
	});
});
