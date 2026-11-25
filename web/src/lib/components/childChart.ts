// Pure burndown math for ChildChart.svelte (BUG-4838), kept out of the
// component so the point mapping and date parsing are unit-testable.

/** A bare calendar date, as a date field stores it. */
const DATE_ONLY = /^(\d{4})-(\d{2})-(\d{2})$/;

/**
 * Parse a start/end bound. A bare YYYY-MM-DD is a local calendar day: `new
 * Date('2026-11-01')` reads it as UTC midnight, which is the previous evening
 * west of UTC, so the chart started (or ended) a day early there. Anything
 * else (a timestamp, an epoch number, junk) parses exactly as `new Date` does.
 */
export function parseChartBound(v: unknown): Date {
	if (typeof v === 'string') {
		const m = DATE_ONLY.exec(v.trim());
		if (m) return new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3]));
	}
	return new Date(v as string);
}

/**
 * The y of value `v` on an axis running from `total` at the top (y = 0) to 0
 * at the bottom (y = chartH). The ticks, the ideal line and the actual line
 * all use this one mapping.
 */
export function valueToY(v: number, total: number, chartH: number): number {
	return ((total - v) / total) * chartH;
}

export interface BurndownChild {
	created_at: string;
	updated_at: string;
}

export interface BurndownInput<T extends BurndownChild> {
	/** Counted children (abandoned ones already left out). */
	counted: T[];
	isDone: (child: T) => boolean;
	/** The bounds after the component's dateInput guard. */
	startDate?: unknown;
	endDate?: unknown;
	chartW: number;
	chartH: number;
	now?: Date;
}

export interface BurndownData {
	total: number;
	remaining: number;
	actualPoints: { x: number; y: number }[];
	actualPath: string;
	areaPath: string;
	idealLine: { x1: number; y1: number; x2: number; y2: number };
	todayX: number;
	yTicks: { value: number; y: number }[];
	start: Date;
	end: Date;
	startLabel: string;
	endLabel: string;
	showTodayMarker: boolean;
}

export function formatChartDate(d: Date): string {
	return d.toLocaleDateString('en-US', { month: 'short', day: 'numeric' });
}

export function buildBurndown<T extends BurndownChild>(input: BurndownInput<T>): BurndownData | null {
	const { counted, isDone, startDate, endDate, chartW, chartH } = input;
	const total = counted.length;
	if (total < 2) return null;

	// Identify completed children
	const completions: { date: Date; count: number }[] = [];
	for (const child of counted) {
		if (isDone(child)) {
			completions.push({ date: new Date(child.updated_at), count: 1 });
		}
	}
	completions.sort((a, b) => a.date.getTime() - b.date.getTime());

	const allDone = completions.length === total;
	const now = input.now ? new Date(input.now) : new Date();
	now.setHours(23, 59, 59, 999);

	// ── Determine start date ──────────────────────────────────────────
	let start: Date;
	if (startDate) {
		start = parseChartBound(startDate);
	} else {
		// Infer: earliest created_at among the counted children
		let earliest = new Date(counted[0].created_at);
		for (const child of counted) {
			const d = new Date(child.created_at);
			if (d < earliest) earliest = d;
		}
		start = earliest;
	}
	start.setHours(0, 0, 0, 0);

	// ── Determine end date ────────────────────────────────────────────
	let end: Date;
	if (endDate) {
		end = parseChartBound(endDate);
	} else if (allDone && completions.length > 0) {
		// All counted children done → last completion date
		end = new Date(completions[completions.length - 1].date);
	} else {
		// Open-ended → today
		end = new Date(now);
	}
	end.setHours(23, 59, 59, 999);

	// Use the later of end date or today for display range
	const displayEnd = end > now ? end : now;
	const totalMs = displayEnd.getTime() - start.getTime();
	if (totalMs <= 0) return null;

	const yOf = (v: number) => valueToY(v, total, chartH);

	// Build actual line points (step chart)
	const actualPoints: { x: number; y: number }[] = [];
	let remaining = total;

	// Start point
	actualPoints.push({ x: 0, y: yOf(remaining) });

	for (const c of completions) {
		const xPct = (c.date.getTime() - start.getTime()) / totalMs;
		const x = Math.max(0, Math.min(chartW, xPct * chartW));

		// Horizontal step to this date at current remaining level
		actualPoints.push({ x, y: yOf(remaining) });

		remaining -= c.count;

		// Vertical drop to new remaining level
		actualPoints.push({ x, y: yOf(remaining) });
	}

	// Extend to today (or end, whichever is shown)
	const nowX = Math.max(0, Math.min(chartW, ((now.getTime() - start.getTime()) / totalMs) * chartW));
	actualPoints.push({ x: nowX, y: yOf(remaining) });

	// Ideal line: straight from (start, total) to (end, 0)
	const idealEndX = Math.max(0, Math.min(chartW, ((end.getTime() - start.getTime()) / totalMs) * chartW));
	const idealLine = { x1: 0, y1: yOf(total), x2: idealEndX, y2: yOf(0) };

	// Today marker
	const todayX = ((now.getTime() - start.getTime()) / totalMs) * chartW;

	// Y axis ticks: `v` rises strictly by `step >= 1`, and `total` is pushed
	// only if it is not already last, so tick values never repeat.
	const yTicks: { value: number; y: number }[] = [];
	const step = total <= 5 ? 1 : total <= 20 ? Math.ceil(total / 5) : Math.ceil(total / 4);
	for (let v = 0; v <= total; v += step) {
		yTicks.push({ value: v, y: yOf(v) });
	}
	if (yTicks[yTicks.length - 1]?.value !== total) {
		yTicks.push({ value: total, y: yOf(total) });
	}

	// Build the actual line SVG path
	const actualPath = actualPoints.map((p, i) => `${i === 0 ? 'M' : 'L'}${p.x},${p.y}`).join(' ');

	// Filled area: the region under the line (remaining work), closed along
	// the bottom of the chart (value 0).
	const areaPath = actualPath + ` L${actualPoints[actualPoints.length - 1].x},${chartH} L0,${chartH} Z`;

	return {
		total,
		remaining,
		actualPoints,
		actualPath,
		areaPath,
		idealLine,
		todayX,
		yTicks,
		start,
		end,
		startLabel: formatChartDate(start),
		endLabel: formatChartDate(end),
		showTodayMarker: todayX > 0 && todayX < chartW
	};
}
