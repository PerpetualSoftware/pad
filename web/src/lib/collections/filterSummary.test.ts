import { describe, it, expect } from 'vitest';
import { filterSummaryParts, filteredCountLabel } from './filterSummary';

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
