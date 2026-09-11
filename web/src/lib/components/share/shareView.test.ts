import { describe, it, expect } from 'vitest';
import {
	UNPARENTED_FILTER_FIELD,
	filterEvaluable,
	matchesFilter,
	parsePublicItem,
	resolveGroupField,
} from './shareView';

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

describe('resolveGroupField with a relation field (TASK-2998, codex round 1)', () => {
	function coll(fields: { key: string; type: string; options?: string[] }[], boardGroupBy?: string) {
		return {
			fields,
			settings: boardGroupBy ? { board_group_by: boardGroupBy } : {},
		} as unknown as Parameters<typeof resolveGroupField>[0];
	}

	it('does NOT group a public board by a relation field', () => {
		// The authenticated board resolves the value against the local index and
		// labels the lane with the target's ref and title. A share has neither —
		// the payload carries field VALUES — so grouping by a relation renders a
		// lane per stored id, formatted: a wall of title-cased UUIDs, which is
		// the exact thing this unit exists to stop showing.
		const c = coll(
			[
				{ key: 'car_color', type: 'relation' },
				{ key: 'status', type: 'select', options: ['open'] },
			],
			'car_color',
		);
		expect(resolveGroupField(c)).toBe('status');
	});

	it('falls all the way through to ungrouped when there is nothing else', () => {
		const c = coll([{ key: 'car_color', type: 'relation' }], 'car_color');
		expect(resolveGroupField(c)).toBe('');
	});

	it('still honours an ordinary group field — the counterfactual', () => {
		// A refusal that fired for every field would silently ungroup every
		// shared board on the instance.
		const c = coll(
			[
				{ key: 'phase', type: 'select', options: ['a'] },
				{ key: 'status', type: 'select', options: ['open'] },
			],
			'phase',
		);
		expect(resolveGroupField(c)).toBe('phase');
	});
});
