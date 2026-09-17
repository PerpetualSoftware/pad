// TASK-3093 — the control the layercake 11 bump (dependabot #1368) passed for
// want of.
//
// Nothing in the suite asserted anything about `BarChart`'s output. Its only
// consumers are the insights page and its print route; no vitest suite rendered
// either, and the one e2e spec that visits `/insights`
// (page-title-survives-pane-and-nav.spec.ts:88) asserts the URL pathname and
// `document.title` — nothing about chart output.
//
// A title assertion cannot catch this class of break, which was measured rather
// than reasoned: serving the unmigrated v11 build in a real browser, the
// insights page still loaded with its title intact and threw only `pageerror`s
// while rendering zero chart elements. So `Web` went green on a major bump that
// rewrote the library's context API out from under the three chart layers. The
// green was the enumeration (nothing looked), not the property (the charts
// work).
//
// This test renders the chart end-to-end through the real `<LayerCake>` — no
// stubbed context — and asserts that the three layers each drew: bars for both
// series, the x axis's category labels, and the y axis's tick labels. It is the
// leg that fails if a future major moves the context shape again.
//
// COVERAGE, stated precisely because an earlier revision of this header did not.
// Every `k.` read in the three layers is exercised by some assertion here, and
// each was checked by mutation rather than by inspection. What that does NOT
// mean is that every arithmetic slip is caught: several reads are pinned by
// relations (ordering, containment, equality between two reads of one key)
// rather than by exact pixels, deliberately, since exact pixels would encode
// d3's band-scale arithmetic into the test and break on any layout tweak. The
// property this file exists to defend is the CONTEXT SHAPE — that each layer
// still reaches a live context and reads the right key off it — and a mutant
// that merely perturbs one term while leaving the read intact may survive.
//
// jsdom has no layout engine, so a chart bound to `clientWidth` measures 0 and
// every scale collapses into a negative range. The shim below gives the chart a
// box to draw in; it is layout, not behaviour, and it does not touch the code
// under test.
import { afterEach, beforeAll, describe, expect, it } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick } from 'svelte';

import BarChart from './BarChart.svelte';

const BOX_WIDTH = 600;
const BOX_HEIGHT = 240;
const TOOLTIP_WIDTH = 80;
// BarChart's own padding literal; bandCenter offsets by its left edge.
const PADDING_LEFT = 36;

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
		get(this: HTMLElement) {
			// The tooltip reports a NARROW width on purpose. BarChart clamps the
			// tooltip's left edge into [w/2, canvasWidth - w/2]; if the tooltip
			// measured the full canvas width that interval would collapse to a
			// single point and every position assertion below would hold no matter
			// what the layer computed. Measured: with both at 600 the hover leg
			// could not tell `bandCenter` apart from one with `k.padding.left`
			// dropped.
			return this.classList?.contains('tooltip') ? TOOLTIP_WIDTH : BOX_WIDTH;
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
		// layer — in BOTH dimensions. x/width come from k.xGet and the band
		// scale; y/height from k.yScale and k.height. A collapsed or absent
		// context leaves these NaN or 0. Checking only the x half would leave
		// `k.height` and the y-scale reads at Bars.svelte:55,57 uncovered, which
		// is exactly what it did until review round 3 said so.
		for (const bar of bars) {
			const width = Number(bar.getAttribute('width'));
			const x = Number(bar.getAttribute('x'));
			const y = Number(bar.getAttribute('y'));
			const height = Number(bar.getAttribute('height'));
			expect(Number.isFinite(width)).toBe(true);
			expect(width).toBeGreaterThan(0);
			expect(Number.isFinite(x)).toBe(true);
			expect(Number.isFinite(y)).toBe(true);
			expect(y).toBeGreaterThanOrEqual(0);
			expect(height).toBeGreaterThan(0);
		}

		// A relation rather than a bound: the y scale has to ORDER the bars, so
		// the tallest value draws the tallest bar and starts highest. A y read
		// that is stuck, zeroed or inverted fails here even when it stays finite.
		const barFor = (title: string) => {
			// Not a bare `!`: if the <title> format ever changes, `find` returns
			// undefined and the first attribute read below would throw a TypeError
			// naming neither the missing title nor the cause, BEFORE the assertions
			// that would have said so plainly.
			const bar = Array.from(bars).find((b) => b.querySelector('title')?.textContent === title);
			expect(bar, `no bar titled ${title}`).toBeDefined();
			return bar!;
		};
		const tallest = barFor('Created: 8');
		const shortest = barFor('Completed: 2');
		expect(Number(tallest.getAttribute('height'))).toBeGreaterThan(
			Number(shortest.getAttribute('height'))
		);
		expect(Number(tallest.getAttribute('y'))).toBeLessThan(Number(shortest.getAttribute('y')));

		// The hit rects span the full plot height, which is a direct k.height read.
		for (const hit of container.querySelectorAll('g.bars rect.hit')) {
			expect(Number(hit.getAttribute('height'))).toBeGreaterThan(0);
		}

		// barX = k.xGet(d) + inner(key): bands march left to right, and within a
		// band the series sit side by side in declared order. Finiteness alone
		// would let a positive constant through, which is the x-dimension twin of
		// the y hole review round 3 closed.
		const xOf = (title: string) => Number(barFor(title).getAttribute('x'));
		expect(xOf('Created: 5')).toBeLessThan(xOf('Created: 3')); // Mon before Tue
		expect(xOf('Created: 3')).toBeLessThan(xOf('Created: 8')); // Tue before Wed
		expect(xOf('Created: 5')).toBeLessThan(xOf('Completed: 2')); // within Mon

		// The per-bar <title> carries the series label and value, so the bars are
		// identifiable rather than merely present.
		const titles = Array.from(bars, (b) => b.querySelector('title')?.textContent);
		expect(titles).toContain('Created: 5');
		expect(titles).toContain('Completed: 7');
	});

	it('draws the x axis with a label per category', () => {
		const { container } = renderChart();

		const texts = Array.from(container.querySelectorAll('g.axis-x text'));
		expect(texts.map((t) => t.textContent?.trim())).toEqual(['Mon', 'Tue', 'Wed']);

		// The baseline spans the plot at its foot — a direct k.width / k.height
		// read that nothing asserted until review round 3 named it.
		const baseline = container.querySelector('g.axis-x line')!;
		const plotWidth = Number(baseline.getAttribute('x2'));
		const plotFoot = Number(baseline.getAttribute('y2'));
		expect(plotWidth).toBeGreaterThan(0);
		expect(plotFoot).toBeGreaterThan(0);
		expect(Number(baseline.getAttribute('y1'))).toBe(plotFoot);

		// Each label is placed by center(), which reads k.xGet and the band
		// scale's bandwidth. Assert ORDER and CONTAINMENT rather than pixels:
		// labels march left to right and all land within the baseline's span,
		// which cross-ties these two assertions instead of bounding one side only.
		const xs = texts.map((t) => Number(t.getAttribute('x')));
		expect(xs.every(Number.isFinite)).toBe(true);
		expect(xs).toEqual([...xs].sort((a, b) => a - b));
		expect(xs.at(-1)).toBeLessThan(plotWidth);
		// Receipt for the lower bound, which is what kills a center() that drops
		// the half-bandwidth offset: BarChart builds the x scale as
		// `scaleBand().paddingInner(0.2)` and d3's paddingOuter defaults to 0, so
		// k.xGet of the FIRST band is exactly 0 and any label x above 0 must have
		// come from the offset. Give that scale a paddingOuter and this stops
		// discriminating — assert the offset directly then.
		expect(xs[0]).toBeGreaterThan(0);

		// The labels sit BELOW the baseline (y = k.height + 16) — a k.height read
		// distinct from the baseline's, and uncovered until review round 4.
		const labelYs = texts.map((t) => Number(t.getAttribute('y')));
		expect(labelYs.every((y) => y > plotFoot)).toBe(true);
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
		const lines = Array.from(container.querySelectorAll('g.axis-y line'));
		expect(lines.length).toBe(ticks.length);
		for (const line of lines) {
			expect(Number.isFinite(Number(line.getAttribute('y1')))).toBe(true);
			expect(Number(line.getAttribute('x2'))).toBeGreaterThan(0);
			// y1 and y2 are two separate k.yScale reads on the same tick.
			expect(Number(line.getAttribute('y2'))).toBe(Number(line.getAttribute('y1')));
		}

		// Each tick's LABEL is placed by its own k.yScale read (AxisY:29),
		// distinct from the grid line's, and uncovered until review round 4. The
		// scale descends — a larger tick value sits higher — and each label lines
		// up with its own grid line.
		const labelYs = Array.from(
			container.querySelectorAll('g.axis-y text'),
			(t) => Number(t.getAttribute('y'))
		);
		expect(labelYs).toEqual(lines.map((l) => Number(l.getAttribute('y1'))));
		expect(labelYs[0]).toBeGreaterThan(labelYs[labelYs.length - 1]);
	});

	it('thins x labels once the categories outnumber maxTicks', () => {
		// AxisX's `step` is ceil(k.data.length / maxTicks), and maxTicks defaults
		// to 8 — so with the 3- and 2-row fixtures above it is always 1 and the
		// k.data.length read is inert. This is the only leg that makes it bite.
		const many = Array.from({ length: 20 }, (_, i) => ({
			day: `d${String(i).padStart(2, '0')}`,
			created: i + 1,
			completed: 1,
		}));
		const { container } = render(BarChart, {
			props: { data: many, x: 'day', series: SERIES, ariaLabel: 'Items per day' },
		});

		// One bar per series per datum regardless — thinning is labels only.
		expect(container.querySelectorAll('g.bars rect:not(.hit)').length).toBe(
			many.length * SERIES.length
		);

		const labels = Array.from(
			container.querySelectorAll('g.axis-x text'),
			(t) => t.textContent?.trim()
		);
		// step = ceil(20/8) = 3 → indices 0,3,6,9,12,15,18.
		expect(labels).toEqual(['d00', 'd03', 'd06', 'd09', 'd12', 'd15', 'd18']);
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

	it('drives the hover tooltip from context read at event time', async () => {
		// `bandCenter` (Bars.svelte:45-47) is the ONE context read in the unit that
		// happens outside a tracking context: it runs in the pointer handler and
		// wants the value at event time rather than a dependency. Review round 1
		// judged that correct by design; this leg is what makes it checked rather
		// than argued, and it is the only coverage of `k.padding` in this file.
		//
		// `bandSummary` is NOT in that category, though it looks like it — its one
		// call site is `<title>{bandSummary(d)}</title>`, a template expression, so
		// its `k.x` read is tracked like any other. The assertion on it below runs
		// before the pointer dispatch and is checking first paint.
		const { container } = renderChart();

		const hits = container.querySelectorAll('g.bars rect.hit');
		expect(hits.length).toBe(DATA.length);

		// The accessible per-band summary is built from k.x plus the series values.
		expect(hits[1].querySelector('title')?.textContent).toBe(
			'Tue — Created 3, Completed 7'
		);

		hits[1].dispatchEvent(new PointerEvent('pointerenter', { bubbles: true }));
		await tick();

		const tooltip = container.querySelector('.tooltip') as HTMLElement | null;
		expect(tooltip).not.toBeNull();
		expect(tooltip?.textContent).toContain('Tue');
		expect(tooltip?.textContent).toContain('7');

		// bandCenter = k.padding.left + k.xGet(d) + bandwidth/2. Both operands are
		// readable off the hit-rect itself (its x IS k.xGet(d), its width IS the
		// bandwidth), so this asserts the actual relation rather than a magic
		// pixel — and it fails if the k.padding.left term is dropped.
		const hitX = Number(hits[1].getAttribute('x'));
		const hitW = Number(hits[1].getAttribute('width'));
		const left = Number.parseFloat(tooltip!.style.left);
		expect(Number.isFinite(left)).toBe(true);
		expect(left).toBeCloseTo(PADDING_LEFT + hitX + hitW / 2, 5);
	});
});
