import { describe, expect, it } from 'vitest';
import { tabLanding } from './tabLanding';

const tabs = (...slugs: string[]) => slugs.map((slug) => ({ slug }));
const without = (list: { slug: string }[], gone: string) => list.filter((t) => t.slug !== gone);

describe('tabLanding (PLAN-3002 Q3)', () => {
	it('lands on the left neighbour', () => {
		const before = tabs('a', 'b', 'c');
		expect(tabLanding(before, 'b', without(before, 'b'))?.slug).toBe('a');
		expect(tabLanding(before, 'c', without(before, 'c'))?.slug).toBe('b');
	});

	it('lands on the tab that becomes first when the first tab goes', () => {
		const before = tabs('a', 'b', 'c');
		expect(tabLanding(before, 'a', without(before, 'a'))?.slug).toBe('b');
	});

	it('answers null (/console) when the only tab goes', () => {
		expect(tabLanding(tabs('a'), 'a', [])).toBeNull();
	});

	it('closing a tab that is not the landing neighbour leaves the others in order', () => {
		// A non-active close owes no navigation; that decision is the
		// caller's. The helper still answers from the positions it is given,
		// and never lands on the tab that went away.
		const before = tabs('a', 'b', 'c', 'd');
		const landing = tabLanding(before, 'c', without(before, 'c'));
		expect(landing?.slug).toBe('b');
		expect(landing?.slug).not.toBe('c');
	});

	it('falls back to the first remaining tab when the neighbour went too', () => {
		// A refetch after a lost workspace can drop more than one tab.
		const before = tabs('a', 'b', 'c');
		expect(tabLanding(before, 'c', tabs('a'))?.slug).toBe('a');
		expect(tabLanding(before, 'a', tabs('c'))?.slug).toBe('c');
	});

	it('lands on the first tab when the gone workspace was never in the bar', () => {
		expect(tabLanding(tabs('a', 'b'), 'zz', tabs('a', 'b'))?.slug).toBe('a');
	});
});
