import { describe, it, expect } from 'vitest';
import { UNPARENTED_FILTER_FIELD, filterEvaluable, matchesFilter, parsePublicItem } from './shareView';

function item(fields: Record<string, unknown>) {
	return parsePublicItem({ title: 'x', ref: 'TASK-1', fields });
}

describe('filterEvaluable', () => {
	it('is evaluable when the field is present in the schema', () => {
		const schemaKeys = new Set(['status', 'priority']);
		expect(filterEvaluable(schemaKeys, { field: 'status', op: 'eq', value: 'open' })).toBe(true);
	});

	it('is not evaluable when the field is absent from the schema', () => {
		const schemaKeys = new Set(['status']);
		expect(filterEvaluable(schemaKeys, { field: 'nope', op: 'eq', value: 'x' })).toBe(false);
	});

	// PLAN-2095 DR-5 / TASK-2099: public shares never carry `is_unparented`,
	// so the reserved pseudo-filter must always be excluded — regardless of
	// what the schema happens to contain.
	it('always excludes the reserved $unparented field, even if it were somehow "in" the schema', () => {
		const schemaKeys = new Set([UNPARENTED_FILTER_FIELD, 'status']);
		expect(filterEvaluable(schemaKeys, { field: UNPARENTED_FILTER_FIELD, op: 'eq', value: true })).toBe(
			false,
		);
	});

	// Grandfathered-schema guard: a REAL field literally named `unparented`
	// (no `$`) is a different string and must evaluate normally.
	it('does not confuse a grandfathered "unparented" (no $) field with the reserved one', () => {
		const schemaKeys = new Set(['unparented']);
		expect(filterEvaluable(schemaKeys, { field: 'unparented', op: 'eq', value: 'true' })).toBe(true);
	});

	it('treats malformed/fieldless filters as evaluable (matchesFilter no-ops them)', () => {
		const schemaKeys = new Set<string>();
		expect(filterEvaluable(schemaKeys, null)).toBe(true);
		expect(filterEvaluable(schemaKeys, {})).toBe(true);
	});
});

describe('matchesFilter', () => {
	it('evaluates eq against the item field', () => {
		expect(matchesFilter(item({ status: 'open' }), { field: 'status', op: 'eq', value: 'open' })).toBe(
			true,
		);
		expect(matchesFilter(item({ status: 'done' }), { field: 'status', op: 'eq', value: 'open' })).toBe(
			false,
		);
	});

	it('evaluates in against array and scalar item fields', () => {
		expect(
			matchesFilter(item({ tags: ['a', 'b'] }), { field: 'tags', op: 'in', value: ['b', 'c'] }),
		).toBe(true);
		expect(matchesFilter(item({ status: 'open' }), { field: 'status', op: 'in', value: ['open'] })).toBe(
			true,
		);
	});

	// Defense in depth: even called directly (bypassing filterEvaluable), the
	// reserved field must never actually filter anything.
	it('never filters on the reserved $unparented field, even called directly', () => {
		expect(
			matchesFilter(item({}), { field: UNPARENTED_FILTER_FIELD, op: 'eq', value: true }),
		).toBe(true);
		expect(
			matchesFilter(item({ [UNPARENTED_FILTER_FIELD]: false }), {
				field: UNPARENTED_FILTER_FIELD,
				op: 'eq',
				value: true,
			}),
		).toBe(true);
	});

	it('does not confuse a grandfathered "unparented" (no $) field with the reserved one', () => {
		expect(
			matchesFilter(item({ unparented: 'true' }), { field: 'unparented', op: 'eq', value: 'true' }),
		).toBe(true);
		expect(
			matchesFilter(item({ unparented: 'false' }), { field: 'unparented', op: 'eq', value: 'true' }),
		).toBe(false);
	});
});
