import { describe, expect, it } from 'vitest';
import type { SearchResult } from '$lib/types';
import { groupResultsByCollection, inRenderedOrder } from './groupResults';

// BUG-3054: grouped by a user-chosen collection slug. A collection named
// "Constructor" (slug `constructor`) found Object's constructor on a plain
// object, skipped creating its group, and the push threw.
function hit(id: string, collection_slug: string): SearchResult {
	return { item: { id, collection_slug } as SearchResult['item'], snippet: '', rank: 0 };
}

describe('groupResultsByCollection (BUG-3054)', () => {
	it('groups a slug that names an Object.prototype member like any other, without throwing', () => {
		const slugs = ['constructor', 'tostring', 'toString', '__proto__', 'hasOwnProperty', 'tasks'];
		const groups = groupResultsByCollection(
			slugs.map((s, i) => hit(`i${i}`, s)),
			[{ slug: 'constructor', icon: '🏗', name: 'Constructor' }],
		);
		expect(Object.keys(groups)).toEqual(slugs);
		for (const s of slugs) {
			expect(groups[s].results, s).toHaveLength(1);
		}
		// A `string`-typed key: `groups.constructor` is typed as Object's Function
		// by TypeScript, the same inherited-member confusion at the type level.
		const ctor: string = 'constructor';
		expect(groups[ctor].name).toBe('Constructor');
		expect(groups[ctor].icon).toBe('🏗');
	});

	it('collects several hits for one slug into one group, in first-seen order', () => {
		const groups = groupResultsByCollection(
			[hit('a', 'constructor'), hit('b', 'tasks'), hit('c', 'constructor')],
			[],
		);
		expect(Object.keys(groups)).toEqual(['constructor', 'tasks']);
		const ctor: string = 'constructor';
		expect(groups[ctor].results.map((r) => r.item.id)).toEqual(['a', 'c']);
	});
});

// TASK-2234 (codex r1): the palette renders results GROUPED by collection, so
// keyboard navigation (and aria-activedescendant) must walk them in that
// order, not in rank order: ranked A1, B1, A2 renders A1, A2, B1.
describe('inRenderedOrder (TASK-2234)', () => {
	it('flattens the groups in the order they render', () => {
		const groups = groupResultsByCollection([hit('a1', 'a'), hit('b1', 'b'), hit('a2', 'a')], []);
		expect(inRenderedOrder(groups).map((r) => r.item.id)).toEqual(['a1', 'a2', 'b1']);
	});
});
