// TASK-3093 — the control the layercake 11 bump (dependabot #1368) passed for
// want of.
//
// Nothing in the suite rendered `BarChart`. Its only consumers are the insights
// page and its print route, neither of which is exercised by vitest or
// Playwright, so `Web` went green on a major bump that rewrote the library's
// context API out from under the three chart layers. The green was the
// enumeration (no test), not the property (the charts work).
//
// This test renders the chart end-to-end through the real `<LayerCake>` — no
// stubbed context — and asserts that the three layers each drew: bars for both
// series, the x axis's category labels, and the y axis's tick labels. It is the
// leg that fails if a future major moves the context shape again.
//
// jsdom has no layout engine, so a chart bound to `clientWidth` measures 0 and
// every scale collapses. The two shims below give the chart a box to draw in;
// they are layout, not behaviour, and neither one touches the code under test.
import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';

import BarChart from './BarChart.svelte';

const BOX_WIDTH = 600;
const BOX_HEIGHT = 240;

beforeAll(() => {
	// `bind:clientWidth` compiles to a ResizeObserver in Svelte 5, which jsdom
	// does not implement. This one reports the fixed box once, synchronously, on
	// observe — enough for LayerCake to compute non-degenerate ranges.
	class StubResizeObserver implements ResizeObserver {
		constructor(private readonly callback: ResizeObserverCallback) {}
		observe(target: Element): void {
			const entry = {
				target,
				contentRect: { width: BOX_WIDTH, height: BOX_HEIGHT } as DOMRectReadOnly,
				borderBoxSize: [{ inlineSize: BOX_WIDTH, blockSize: BOX_HEIGHT }],
				contentBoxSize: [{ inlineSize: BOX_WIDTH, blockSize: BOX_HEIGHT }],
				devicePixelContentBoxSize: [{ inlineSize: BOX_WIDTH, blockSize: BOX_HEIGHT }],
			} as unknown as ResizeObserverEntry;
			this.callback([entry], this);
		}
		unobserve(): void {}
		disconnect(): void {}
	}
	vi.stubGlobal('ResizeObserver', StubResizeObserver);

	// jsdom returns 0 for every layout read. LayerCake falls back to
	// `getBoundingClientRect()` and `clientWidth`/`clientHeight` for its
	// container measurement, so both have to report the same box.
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
	HTMLElement.prototype.getBoundingClientRect = function getBoundingClientRect() {
		return {
			x: 0,
			y: 0,
			top: 0,
			left: 0,
			right: BOX_WIDTH,
			bottom: BOX_HEIGHT,
			width: BOX_WIDTH,
			height: BOX_HEIGHT,
			toJSON: () => ({}),
		} as DOMRect;
	};
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
});
