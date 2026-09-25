import { describe, expect, it } from 'vitest';
import { GRAPH_PALETTE, createCollectionColorMap } from './palette';

// BUG-3054: the assigner is keyed by collection slug, which a user picks. A
// plain object answered `colors['constructor']` with Object's constructor, so a
// collection named "Constructor" was coloured by a function and never entered
// the legend. Both graph views use this one assigner.
describe('createCollectionColorMap (BUG-3054)', () => {
	it('gives a slug that names an Object.prototype member a real palette colour, and a legend entry', () => {
		const palette = createCollectionColorMap();
		const slugs = ['tasks', 'constructor', 'tostring', 'toString', '__proto__', 'valueOf'];
		const colors = slugs.map((s) => palette.colorForCollection(s));
		for (const [i, c] of colors.entries()) {
			expect(GRAPH_PALETTE, `${slugs[i]} got ${String(c)}`).toContain(c);
		}
		// First-seen order, one entry per slug, in the map the legend reads.
		expect(Object.keys(palette.colors)).toEqual(slugs);
		expect(colors).toEqual(slugs.map((_, i) => GRAPH_PALETTE[i % GRAPH_PALETTE.length]));
	});

	it('is stable: the same slug gets the same colour on every ask', () => {
		const palette = createCollectionColorMap();
		const first = palette.colorForCollection('constructor');
		palette.colorForCollection('tasks');
		expect(palette.colorForCollection('constructor')).toBe(first);
	});
});
