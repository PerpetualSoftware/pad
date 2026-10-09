import { describe, expect, it } from 'vitest';
import { GRAPH_PALETTES, contrast, luminance, type GraphThemeName } from './graphTheme';

// TASK-2239: each palette value does the same job in both themes.
describe('graph palettes (TASK-2239)', () => {
	for (const name of ['dark', 'light'] as GraphThemeName[]) {
		const p = GRAPH_PALETTES[name];
		it(`${name}: labels are readable on the backdrop (AA, 4.5:1)`, () => {
			expect(contrast(p.label, p.backdrop)).toBeGreaterThanOrEqual(4.5);
		});
		it(`${name}: the touch glow moves AWAY from the backdrop`, () => {
			// The glow and the backdrop sit at opposite ends of the luminance range.
			expect(Math.abs(luminance(p.glow) - luminance(p.backdrop))).toBeGreaterThan(0.8);
		});
	}

	it('the dark palette is the one the page always used', () => {
		expect(GRAPH_PALETTES.dark).toEqual({ backdrop: '#0a0a1a', label: '#cbd5e1', glow: '#ffffff', edgeRgb: '148, 163, 184' });
	});

	it("CONTROL: the old dark label on the light backdrop fails, which is the bug", () => {
		expect(contrast(GRAPH_PALETTES.dark.label, GRAPH_PALETTES.light.backdrop)).toBeLessThan(2);
	});
});
