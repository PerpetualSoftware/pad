// BUG-4838: the burndown's actual line was drawn upside down against its own
// axis, and a date-only start/end landed a day early west of UTC. The zone is
// pinned to America/New_York (west of UTC) for this file; vitest runs
// each file in its own worker, and Node re-reads process.env.TZ on change.
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import ChildChart from './ChildChart.svelte';
import { buildBurndown, parseChartBound, valueToY } from './childChart';

const zone = process.env.TZ;
beforeAll(() => {
	process.env.TZ = 'America/New_York';
});
afterAll(() => {
	process.env.TZ = zone;
});

// ChildChart's plot area: 200 high minus 20 top and 30 bottom padding.
const chartH = 150;
const chartW = 540;

const kid = (id: string, status: string) =>
	({ id, slug: id, title: id, collection_slug: 'tasks', fields: JSON.stringify({ status }),
		created_at: '2026-11-01T12:00:00Z', updated_at: '2026-11-10T12:00:00Z' }) as never;

const kids = (total: number, done: number) =>
	Array.from({ length: total }, (_, i) => kid(`k${i}`, i < done ? 'done' : 'open'));

let instance: ReturnType<typeof mount> | undefined;
let target: HTMLElement | undefined;
afterEach(() => {
	if (instance) unmount(instance);
	instance = undefined;
	target?.remove();
});

function render(props: Record<string, unknown>) {
	target = document.body.appendChild(document.createElement('div'));
	instance = mount(ChildChart, { target, props: { wsSlug: 'ws', ...props } as never });
	flushSync();
	return target;
}

/** The actual line's points, from its SVG path. */
function actualLine(root: HTMLElement): { x: number; y: number }[] {
	const path = root.querySelector('path[fill="none"]');
	const d = path?.getAttribute('d') ?? '';
	return [...d.matchAll(/[ML]([-\d.e]+),([-\d.e]+)/g)].map((m) => ({ x: Number(m[1]), y: Number(m[2]) }));
}

/** The y of the gridline labelled `value`. */
function tickY(root: HTMLElement, value: number): number {
	const label = [...root.querySelectorAll('text')].find((t) => t.textContent?.trim() === String(value));
	return Number(label?.getAttribute('y'));
}

describe('burndown actual line follows its axis (BUG-4838)', () => {
	it('10 total, 4 remaining: the line ends at the tick for 4', () => {
		const root = render({ children: kids(10, 6), startDate: '2026-11-01', endDate: '2026-11-30' });
		const pts = actualLine(root);
		expect(pts.length).toBeGreaterThan(1);
		const last = pts[pts.length - 1];
		expect(last.y).toBeCloseTo(((10 - 4) / 10) * chartH);
		expect(last.y).toBeCloseTo(tickY(root, 4));
	});

	it('10 total, 10 remaining: the line starts at the top, where the ideal line starts', () => {
		const root = render({ children: kids(10, 0), startDate: '2026-11-01', endDate: '2026-11-30' });
		const pts = actualLine(root);
		expect(pts[0].y).toBe(0);
		expect(pts[0].y).toBeCloseTo(tickY(root, 10));
		const ideal = root.querySelector('line[stroke-dasharray="6 4"]');
		expect(Number(ideal?.getAttribute('y1'))).toBe(pts[0].y);
	});

	it('maps every point with the axis mapping (pure)', () => {
		const counted = Array.from({ length: 10 }, (_, i) => ({
			created_at: '2026-11-01T12:00:00Z',
			updated_at: `2026-11-${String(2 + i).padStart(2, '0')}T12:00:00Z`,
			done: i < 6
		}));
		const data = buildBurndown({
			counted,
			isDone: (c) => c.done,
			startDate: '2026-11-01',
			endDate: '2026-11-30',
			chartW,
			chartH,
			now: new Date(2026, 10, 20)
		})!;
		expect(data.remaining).toBe(4);
		expect(data.actualPoints[0].y).toBe(valueToY(10, 10, chartH));
		expect(data.actualPoints[data.actualPoints.length - 1].y).toBe(valueToY(4, 10, chartH));
		// The line only moves down as work completes.
		for (let i = 1; i < data.actualPoints.length; i++) {
			expect(data.actualPoints[i].y).toBeGreaterThanOrEqual(data.actualPoints[i - 1].y);
		}
		// The area closes along the bottom (value 0).
		expect(data.areaPath.endsWith(`L0,${chartH} Z`)).toBe(true);
	});
});

describe('date-only bounds are local days (BUG-4838)', () => {
	it('runs west of UTC', () => {
		// Guard: the zone pin took effect, so the cases below run west of UTC
		// (local midnight on 1 Nov 2026 is still EDT, UTC-4).
		expect(new Date(2026, 10, 1).getTimezoneOffset()).toBe(240);
		expect(new Date(2026, 10, 30).getTimezoneOffset()).toBe(300);
	});

	it("startDate '2026-11-01' starts at local Nov 1 00:00 and is labelled Nov 1", () => {
		const root = render({ children: kids(3, 1), startDate: '2026-11-01', endDate: '2026-11-30' });
		const labels = [...root.querySelectorAll('text')].map((t) => t.textContent?.trim());
		expect(labels).toContain('Nov 1');
		expect(labels).toContain('Nov 30');
		expect(labels).not.toContain('Oct 31');
		expect(labels).not.toContain('Nov 29');
	});

	it('the computed bounds are the stated local days (pure)', () => {
		const counted = [
			{ created_at: '2026-11-01T12:00:00Z', updated_at: '2026-11-02T12:00:00Z' },
			{ created_at: '2026-11-01T12:00:00Z', updated_at: '2026-11-03T12:00:00Z' }
		];
		const data = buildBurndown({
			counted,
			isDone: () => false,
			startDate: '2026-11-01',
			endDate: '2026-11-30',
			chartW,
			chartH,
			now: new Date(2026, 10, 20)
		})!;
		expect(data.start.getTime()).toBe(new Date(2026, 10, 1, 0, 0, 0, 0).getTime());
		expect(data.startLabel).toBe('Nov 1');
		expect(data.end.getTime()).toBe(new Date(2026, 10, 30, 23, 59, 59, 999).getTime());
		expect(data.endLabel).toBe('Nov 30');
	});

	it('a value carrying a time keeps its instant', () => {
		expect(parseChartBound('2026-11-01T03:00:00Z').getTime()).toBe(Date.UTC(2026, 10, 1, 3));
		expect(parseChartBound(Date.UTC(2026, 10, 1)).getTime()).toBe(Date.UTC(2026, 10, 1));
		expect(parseChartBound('2026-11-01').getTime()).toBe(new Date(2026, 10, 1).getTime());
	});
});
