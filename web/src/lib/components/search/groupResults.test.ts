import { describe, expect, it } from 'vitest';
import type { SearchResult } from '$lib/types';
import { groupResultsByCollection } from './groupResults';

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
		expect(groups.constructor.name).toBe('Constructor');
		expect(groups.constructor.icon).toBe('🏗');
	});

	it('collects several hits for one slug into one group, in first-seen order', () => {
		const groups = groupResultsByCollection(
			[hit('a', 'constructor'), hit('b', 'tasks'), hit('c', 'constructor')],
			[],
		);
		expect(Object.keys(groups)).toEqual(['constructor', 'tasks']);
		expect(groups.constructor.results.map((r) => r.item.id)).toEqual(['a', 'c']);
	});
});
