import { describe, it, expect } from 'vitest';
import { filterSummaryParts, filteredCountLabel, relationSummaryLabel } from './filterSummary';
import type { ItemIndexRow } from '$lib/types';

const base = {
	filters: {},
	tags: [],
	search: '',
	unparented: false,
	fieldLabel: (k: string) => ({ status: 'Status', owner: 'Owner', parent: 'Parent' })[k] ?? k,
	valueLabel: (k: string, v: string) => (k === 'owner' || k === 'parent' ? ({ i1: 'Alice', p1: 'Launch' } as Record<string, string>)[v] ?? v : v),
};

describe('filterSummaryParts (TASK-3592)', () => {
	it('is empty when nothing is filtered', () => {
		expect(filterSummaryParts(base)).toEqual([]);
	});

	it('names each field by its label and each value by what it means', () => {
		expect(
			filterSummaryParts({ ...base, filters: { status: 'open', owner: 'i1', parent: 'p1' } }),
		).toEqual(['status: open', 'owner: Alice', 'parent: Launch']);
	});

	it('keeps an unresolvable id as the id rather than dropping the filter', () => {
		expect(filterSummaryParts({ ...base, filters: { owner: 'gone' } })).toEqual(['owner: gone']);
	});

	it('lists tags, unparented and a trimmed search after the fields', () => {
		expect(
			filterSummaryParts({ ...base, filters: { status: 'open' }, tags: ['ui', 'web'], unparented: true, search: '  login  ' }),
		).toEqual(['status: open', 'tags: ui, web', 'unparented only', 'search: “login”']);
	});

	it('a whitespace-only search is not a filter', () => {
		expect(filterSummaryParts({ ...base, search: '   ' })).toEqual([]);
	});
});

describe('filteredCountLabel (TASK-3592)', () => {
	it('reads "N of M" when filtered', () => {
		expect(filteredCountLabel(12, 84, true)).toBe('12 of 84');
	});
	it('reads the plain total when not', () => {
		expect(filteredCountLabel(84, 84, false)).toBe('84');
	});
});

// TASK-3599 (retro of #2072): the summary names a relation value the way the
// filter chip does, by id and within the field's target collection.
describe('relationSummaryLabel', () => {
	const rows: Record<string, Partial<ItemIndexRow>> = {
		'id-red': { id: 'id-red', title: 'Red', collection_slug: 'colors' },
		red: { id: 'id-other', title: 'Some task slugged red', collection_slug: 'tasks' },
		'id-shared': { id: 'id-shared', title: 'A task', collection_slug: 'tasks' },
	};
	const lookup = (id: string) => (rows[id] as ItemIndexRow | undefined) ?? null;
	const known = new Set(['colors', 'tasks']);

	it('names an id in the target collection by its title', () => {
		expect(relationSummaryLabel('id-red', lookup, 'colors', known)).toBe('Red');
	});

	it('does not name a legacy slug value after whatever item has that slug', () => {
		expect(relationSummaryLabel('red', lookup, 'colors', known)).not.toBe('Some task slugged red');
	});

	it('does not name a same-id row from another collection', () => {
		expect(relationSummaryLabel('id-shared', lookup, 'colors', known)).not.toBe('A task');
	});
});
