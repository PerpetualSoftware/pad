// TASK-3093 — the control the layercake 11 bump (dependabot #1368) passed for
// want of.
//
// Nothing in the suite asserted anything about `BarChart`'s output. Its only
// consumers are the insights page and its print route; no vitest suite rendered
// either, and the one e2e spec that visits `/insights`
// (page-title-survives-pane-and-nav.spec.ts:88) SPA-navigates there and checks
// `document.title` alone — on a fresh fixture workspace the chart's `hasData`
// is false, so the layers never even mount. So `Web` went green on a major bump
// that rewrote the library's context API out from under the three chart layers.
// The green was the enumeration (nothing looked), not the property (the charts
// work).
//
// This test renders the chart end-to-end through the real `<LayerCake>` — no
// stubbed context — and asserts that the three layers each drew: bars for both
// series, the x axis's category labels, and the y axis's tick labels. It is the
// leg that fails if a future major moves the context shape again.
//
// jsdom has no layout engine, so a chart bound to `clientWidth` measures 0 and
// every scale collapses into a negative range. The shim below gives the chart a
// box to draw in; it is layout, not behaviour, and it does not touch the code
// under test.
import { afterEach, beforeAll, describe, expect, it } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';

import BarChart from './BarChart.svelte';

const BOX_WIDTH = 600;
const BOX_HEIGHT = 240;

beforeAll(() => {
	// LayerCake measures its container through `bind:clientWidth` /
	// `bind:clientHeight` alone (LayerCake.svelte:375-376), and Svelte's size
	// binding reads `element[type]` directly rather than a rect. jsdom returns 0
	// for both, so these two getters are the whole shim — measured: with them
	// removed the bar-geometry assertion fails, and with the other candidate
	// shims removed (a ResizeObserver stub, a getBoundingClientRect override)
	// nothing changes, because neither is on the measurement path. `setup-jsdom.ts`
	// already installs a global inert ResizeObserver, which is enough for the
	// binding not to throw.
	Object.defineProperty(HTMLElement.prototype, 'clientWidth', {
		configurable: true,
		get() {
			return BOX_WIDTH;
		},
	});
	Object.defineProperty(HTMLElement.prototype, 'clientHeight', {
		configurable: true,
		get() {
			return BOX_HEIGHT;
		},
	});
});

afterEach(() => cleanup());

// Two series over three categories, with distinct values per cell so an
// assertion can name one bar rather than "some rect exists".
const DATA = [
	{ day: 'Mon', created: 5, completed: 2 },
	{ day: 'Tue', created: 3, completed: 7 },
	{ day: 'Wed', created: 8, completed: 4 },
];

const SERIES = [
	{ key: 'created', label: 'Created' },
	{ key: 'completed', label: 'Completed' },
];

function renderChart() {
	return render(BarChart, {
		props: { data: DATA, x: 'day', series: SERIES, ariaLabel: 'Items per day' },
	});
}

describe('BarChart renders through LayerCake', () => {
	it('draws one bar per series per datum', () => {
		const { container } = renderChart();

		const bars = container.querySelectorAll('g.bars rect:not(.hit)');
		expect(bars.length).toBe(DATA.length * SERIES.length);

		// Every bar is positioned and sized by the scales the context hands the
		// layer. A collapsed or absent context leaves these NaN or 0.
		for (const bar of bars) {
			const width = Number(bar.getAttribute('width'));
			const x = Number(bar.getAttribute('x'));
			expect(Number.isFinite(width)).toBe(true);
			expect(width).toBeGreaterThan(0);
			expect(Number.isFinite(x)).toBe(true);
		}

		// The per-bar <title> carries the series label and value, so the bars are
		// identifiable rather than merely present.
		const titles = Array.from(bars, (b) => b.querySelector('title')?.textContent);
		expect(titles).toContain('Created: 5');
		expect(titles).toContain('Completed: 7');
	});

	it('draws the x axis with a label per category', () => {
		const { container } = renderChart();

		const labels = Array.from(
			container.querySelectorAll('g.axis-x text'),
			(t) => t.textContent?.trim()
		);
		expect(labels).toEqual(['Mon', 'Tue', 'Wed']);
	});

	it('draws the y axis with numeric ticks spanning the data', () => {
		const { container } = renderChart();

		const ticks = Array.from(
			container.querySelectorAll('g.axis-y text'),
			(t) => Number(t.textContent?.trim())
		);
		expect(ticks.length).toBeGreaterThan(1);
		expect(ticks.every((t) => Number.isFinite(t))).toBe(true);
		// yDomain is [0, max], and the tallest bar is 8.
		expect(Math.min(...ticks)).toBe(0);
		expect(Math.max(...ticks)).toBeGreaterThanOrEqual(8);

		// Grid lines are drawn from the y scale; a collapsed context gives NaN.
		const lines = container.querySelectorAll('g.axis-y line');
		expect(lines.length).toBe(ticks.length);
		for (const line of lines) {
			expect(Number.isFinite(Number(line.getAttribute('y1')))).toBe(true);
			expect(Number(line.getAttribute('x2'))).toBeGreaterThan(0);
		}
	});

	it('repaints all three layers when the data changes after mount', async () => {
		// The three legs above assert first paint. This one asserts the property
		// the migration actually turns on: under layercake 11 the context's keys
		// are GETTERS, so a binding destructured at setup is a snapshot that never
		// updates again — and a chart that draws correctly once looks identical
		// either way. Only a post-mount change tells them apart.
		const { container, rerender } = render(BarChart, {
			props: { data: DATA, x: 'day', series: SERIES, ariaLabel: 'Items per day' },
		});

		const titlesOf = () =>
			Array.from(
				container.querySelectorAll('g.bars rect:not(.hit) title'),
				(t) => t.textContent
			);
		expect(titlesOf()).toContain('Created: 5');

		const NEXT = [
			{ day: 'Thu', created: 11, completed: 1 },
			{ day: 'Fri', created: 12, completed: 6 },
		];
		await rerender({ data: NEXT, x: 'day', series: SERIES, ariaLabel: 'Items per day' });

		// Bars followed `k.data` and `k.yScale`...
		expect(titlesOf()).toContain('Created: 11');
		expect(titlesOf()).not.toContain('Created: 5');
		expect(container.querySelectorAll('g.bars rect:not(.hit)').length).toBe(
			NEXT.length * SERIES.length
		);

		// ...the x axis followed `k.data` and `k.x`...
		const labels = Array.from(
			container.querySelectorAll('g.axis-x text'),
			(t) => t.textContent?.trim()
		);
		expect(labels).toEqual(['Thu', 'Fri']);

		// ...and the y axis followed `k.yScale`, whose domain has to have grown
		// past the previous maximum of 8 to cover the new one.
		const ticks = Array.from(
			container.querySelectorAll('g.axis-y text'),
			(t) => Number(t.textContent?.trim())
		);
		expect(Math.max(...ticks)).toBeGreaterThanOrEqual(12);
	});
});
