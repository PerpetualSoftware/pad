import { describe, it, expect } from 'vitest';
import {
	UNPARENTED_FILTER_FIELD,
	buildUnparentedViewFilter,
	clearParentFilter,
	filtersSetParent,
	readUnparentedParam,
	resolveParentUnparentedMutex,
	unparentedConfirmedRestricted,
	unparentedEffective,
	viewHasUnparentedFilter,
	writeUnparentedParam,
} from './unparentedFilter';

describe('filtersSetParent', () => {
	it('is true when the patch carries a non-empty parent value', () => {
		expect(filtersSetParent({ parent: 'plan-1' })).toBe(true);
	});

	// The legacy `phase` key is a backward-compat alias for `parent` that
	// `filteredItems` resolves identically (both hit `parent_link_id`).
	it('is true for the legacy phase alias too', () => {
		expect(filtersSetParent({ phase: 'plan-1' })).toBe(true);
	});

	it('is false when parent/phase are absent or empty', () => {
		expect(filtersSetParent({})).toBe(false);
		expect(filtersSetParent({ parent: '' })).toBe(false);
		expect(filtersSetParent({ phase: '' })).toBe(false);
		expect(filtersSetParent({ status: 'open' })).toBe(false);
	});
});

describe('clearParentFilter', () => {
	it('drops the parent key, preserving other filters', () => {
		const result = clearParentFilter({ parent: 'plan-1', status: 'open' });
		expect(result).toEqual({ status: 'open' });
	});

	it('drops the legacy phase alias too', () => {
		const result = clearParentFilter({ phase: 'plan-1', status: 'open' });
		expect(result).toEqual({ status: 'open' });
	});

	it('drops both when a filter set somehow carries both aliases', () => {
		const result = clearParentFilter({ parent: 'a', phase: 'b', status: 'open' });
		expect(result).toEqual({ status: 'open' });
	});

	it('returns the same reference when there is nothing to clear (no-op)', () => {
		const filters = { status: 'open' };
		expect(clearParentFilter(filters)).toBe(filters);
	});
});

describe('resolveParentUnparentedMutex', () => {
	it('is a no-op when unparented is false', () => {
		const filters = { parent: 'plan-1' };
		expect(resolveParentUnparentedMutex(filters, false)).toEqual({ filters, unparented: false });
	});

	it('drops parent/phase when unparented is true (unparented wins)', () => {
		expect(resolveParentUnparentedMutex({ parent: 'plan-1', status: 'open' }, true)).toEqual({
			filters: { status: 'open' },
			unparented: true,
		});
		expect(resolveParentUnparentedMutex({ phase: 'plan-1' }, true)).toEqual({
			filters: {},
			unparented: true,
		});
	});
});

describe('unparentedConfirmedRestricted', () => {
	it('is true only when metadata is confirmed false AND no resync is in flight', () => {
		expect(unparentedConfirmedRestricted(true, false)).toBe(true);
	});

	// Codex review round 3, P1: mere "unavailable" (metadata unknown/pending)
	// must NOT be treated as confirmed restriction — that would destroy a
	// legitimate intent that loaded mid-resync, moments before the resync
	// confirms the caller unrestricted.
	it('is false while a resync is in flight, even if metadata currently reads false', () => {
		expect(unparentedConfirmedRestricted(true, true)).toBe(false);
	});

	it('is false when metadata is not confirmed false (unknown/true)', () => {
		expect(unparentedConfirmedRestricted(false, false)).toBe(false);
		expect(unparentedConfirmedRestricted(false, true)).toBe(false);
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
	it('reads $unparented=true from search params', () => {
		const params = new URLSearchParams();
		params.set(UNPARENTED_FILTER_FIELD, 'true');
		expect(readUnparentedParam(params)).toBe(true);
	});

	it('treats any other value (or absence) as false', () => {
		expect(readUnparentedParam(new URLSearchParams(''))).toBe(false);
		const falseParams = new URLSearchParams();
		falseParams.set(UNPARENTED_FILTER_FIELD, 'false');
		expect(readUnparentedParam(falseParams)).toBe(false);
	});

	it('writes the reserved param when active and removes it when not', () => {
		const params = new URLSearchParams();
		writeUnparentedParam(params, true);
		expect(params.get(UNPARENTED_FILTER_FIELD)).toBe('true');

		writeUnparentedParam(params, false);
		expect(params.has(UNPARENTED_FILTER_FIELD)).toBe(false);
	});

	// Collision guard (Codex review round 1): a real field literally named
	// `unparented` (no `$`) round-trips through the plain `unparented`
	// query param independently — untouched by the reserved-param helpers.
	it('does not read/write the plain "unparented" param (real-field collision guard)', () => {
		const params = new URLSearchParams('unparented=some-real-field-value');
		expect(readUnparentedParam(params)).toBe(false);
		writeUnparentedParam(params, true);
		// The reserved param is added under its own (distinct) key; the
		// pre-existing plain `unparented` param is untouched.
		expect(params.get('unparented')).toBe('some-real-field-value');
		expect(params.get(UNPARENTED_FILTER_FIELD)).toBe('true');
	});
});
