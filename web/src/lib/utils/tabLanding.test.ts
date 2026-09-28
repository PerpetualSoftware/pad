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

	// The two legs below give `after` a shape that is NOT `before` minus the
	// gone tab, which is the only case where these rules differ from their
	// look-alikes (TASK-3280: "first remaining" vs "last remaining", and
	// "right neighbour" vs "first remaining").
	it('the fallback is the FIRST remaining tab, not any remaining tab', () => {
		const before = tabs('a', 'b', 'c', 'd');
		// d went, and so did its neighbour c: two tabs remain.
		expect(tabLanding(before, 'd', tabs('a', 'b'))?.slug).toBe('a');
	});

	it('when the first tab goes, its right neighbour wins even if it is no longer first', () => {
		// The refetch also opened x at the front of the bar.
		const before = tabs('a', 'b', 'c');
		expect(tabLanding(before, 'a', tabs('x', 'b', 'c'))?.slug).toBe('b');
	});

	it('lands on the first tab when the gone workspace was never in the bar', () => {
		expect(tabLanding(tabs('a', 'b'), 'zz', tabs('a', 'b'))?.slug).toBe('a');
	});
});
