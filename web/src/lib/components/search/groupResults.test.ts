import { describe, expect, it } from 'vitest';
import type { SearchResult } from '$lib/types';
import {
	groupResultsByCollection,
	inRenderedOrder,
	reselectAfterAppend,
	resultAnnouncement,
} from './groupResults';

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

// TASK-2234, codex r2: a grouped append can put a row above the selected
// one; the selection follows the item, not the index.
describe('reselectAfterAppend (TASK-2234)', () => {
	const cols = [{ slug: 'a', icon: '', name: 'A' }, { slug: 'b', icon: '', name: 'B' }];
	const order = (rs: SearchResult[]) => inRenderedOrder(groupResultsByCollection(rs, cols));
	const a1 = hit('a1', 'a');
	const b1 = hit('b1', 'b');
	const a2 = hit('a2', 'a');

	it('keeps the selected item selected when an appended row renders above it', () => {
		const before = order([a1, b1]);
		const after = order([a1, b1, a2]);
		expect(after.map((r) => r.item.id)).toEqual(['a1', 'a2', 'b1']);
		expect(reselectAfterAppend(before, after, 1)).toBe(2);
	});

	it('leaves no selection as none, and an unmoved selection where it was', () => {
		const before = order([a1, b1]);
		const after = order([a1, b1, a2]);
		expect(reselectAfterAppend(before, after, -1)).toBe(-1);
		expect(reselectAfterAppend(before, after, 0)).toBe(0);
	});
});

// TASK-2234, codex r2: a body-only term has a local total of 0 while the
// content search runs; announcing "0 results" then would be false.
describe('resultAnnouncement (TASK-2234)', () => {
	it('says nothing while either search is still running', () => {
		expect(resultAnnouncement('quokka', true, false, 0)).toBe('');
		expect(resultAnnouncement('quokka', false, true, 0)).toBe('');
	});

	it('counts once both have answered, and says nothing for an empty query', () => {
		expect(resultAnnouncement('quokka', false, false, 1)).toBe('1 result');
		expect(resultAnnouncement('quokka', false, false, 3)).toBe('3 results');
		expect(resultAnnouncement('  ', false, false, 0)).toBe('');
	});
});
