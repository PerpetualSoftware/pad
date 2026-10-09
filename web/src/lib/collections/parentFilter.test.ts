import { describe, it, expect } from 'vitest';
import { parentFilterOptions } from './parentFilter';

const names = (slug: string) => ({ plans: 'Plans', requisitions: 'Requisitions' })[slug];

describe('parentFilterOptions (TASK-2215)', () => {
	it('lists the parents the items have, once each, ordered by label', () => {
		const opts = parentFilterOptions(
			[
				{ parent_link_id: 'p2', parent_ref: 'PLAN-10', parent_title: 'Later', parent_collection_slug: 'plans' },
				{ parent_link_id: 'p1', parent_ref: 'PLAN-2', parent_title: 'Sooner', parent_collection_slug: 'plans' },
				{ parent_link_id: 'p2', parent_ref: 'PLAN-10', parent_title: 'Later', parent_collection_slug: 'plans' },
				{}
			],
			names
		);
		expect(Object.entries(opts.labels)).toEqual([
			['p1', 'PLAN-2: Sooner'],
			['p2', 'PLAN-10: Later']
		]);
		expect(opts.noun).toBe('plans');
	});

	it('names the parents by their collection when they share one, else "parents"', () => {
		expect(parentFilterOptions([{ parent_link_id: 'r', parent_collection_slug: 'requisitions' }], names).noun).toBe('requisitions');
		expect(
			parentFilterOptions(
				[
					{ parent_link_id: 'a', parent_collection_slug: 'plans' },
					{ parent_link_id: 'b', parent_collection_slug: 'requisitions' }
				],
				names
			).noun
		).toBe('parents');
		expect(parentFilterOptions([{ parent_link_id: 'a', parent_collection_slug: 'gone' }], names).noun).toBe('parents');
	});

	it('offers nothing when no item has a parent', () => {
		expect(parentFilterOptions([{}, { parent_link_id: '' }], names).labels).toEqual({});
	});

	it('falls back to the ref-less title, then the id', () => {
		const { labels } = parentFilterOptions([{ parent_link_id: 'x', parent_title: 'Untitled ref' }, { parent_link_id: 'y' }], names);
		expect(labels).toEqual({ x: 'Untitled ref', y: 'y' });
	});
});
