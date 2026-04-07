<script lang="ts">
	import type { Item } from '$lib/types';
	import { parseFields } from '$lib/types';

	interface Props {
		children: Item[];
		startDate?: string;
		endDate?: string;
		terminalStatuses?: string[];
	}

	let { children, startDate, endDate, terminalStatuses }: Props = $props();
	const defaultTerminal = ['done', 'completed', 'resolved', 'cancelled', 'rejected', 'wontfix', 'fixed', 'implemented', 'archived', 'disabled', 'deprecated'];
	const terminal = $derived(terminalStatuses ?? defaultTerminal);

	// Chart dimensions
	const padding = { top: 20, right: 20, bottom: 30, left: 40 };
	const width = 600;
	const height = 200;
	const chartW = width - padding.left - padding.right;
	const chartH = height - padding.top - padding.bottom;

	let chartData = $derived.by(() => {
		const total = children.length;
		if (total < 2) return null;

		// Identify completed children
		const completions: { date: Date; count: number }[] = [];
		for (const child of children) {
			const f = parseFields(child);
			if (terminal.includes(f.status)) {
				completions.push({ date: new Date(child.updated_at), count: 1 });
			}
		}
		completions.sort((a, b) => a.date.getTime() - b.date.getTime());

		const allDone = completions.length === total;
		const now = new Date();
		now.setHours(23, 59, 59, 999);

		// ── Determine start date ──────────────────────────────────────────
		let start: Date;
		if (startDate) {
			start = new Date(startDate);
		} else {
			// Infer: earliest created_at among children
			let earliest = new Date(children[0].created_at);
			for (const child of children) {
				const d = new Date(child.created_at);
				if (d < earliest) earliest = d;
			}
			start = earliest;
		}
		start.setHours(0, 0, 0, 0);

		// ── Determine end date ────────────────────────────────────────────
		let end: Date;
		if (endDate) {
			end = new Date(endDate);
		} else if (allDone && completions.length > 0) {
			// All children done → last completion date
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

		// Build actual line points (step chart)
		const actualPoints: { x: number; y: number }[] = [];
		let remaining = total;

		// Start point
		actualPoints.push({
			x: 0,
			y: (remaining / total) * chartH
		});

		for (const c of completions) {
			const xPct = (c.date.getTime() - start.getTime()) / totalMs;
			const x = Math.max(0, Math.min(chartW, xPct * chartW));

			// Horizontal step to this date at current remaining level
			actualPoints.push({ x, y: (remaining / total) * chartH });

			remaining -= c.count;

			// Vertical drop to new remaining level
			actualPoints.push({ x, y: (remaining / total) * chartH });
		}

		// Extend to today (or end, whichever is shown)
		const nowX = Math.max(0, Math.min(chartW, ((now.getTime() - start.getTime()) / totalMs) * chartW));
		actualPoints.push({ x: nowX, y: (remaining / total) * chartH });

		// Ideal line: straight from (start, total) to (end, 0)
		const idealEndX = Math.max(0, Math.min(chartW, ((end.getTime() - start.getTime()) / totalMs) * chartW));
		const idealLine = {
			x1: 0,
			y1: 0,
			x2: idealEndX,
			y2: chartH
		};

		// Today marker
		const todayX = ((now.getTime() - start.getTime()) / totalMs) * chartW;

		// Y axis ticks
		const yTicks: { value: number; y: number }[] = [];
		const step = total <= 5 ? 1 : total <= 20 ? Math.ceil(total / 5) : Math.ceil(total / 4);
		for (let v = 0; v <= total; v += step) {
			yTicks.push({ value: v, y: ((total - v) / total) * chartH });
		}
		if (yTicks[yTicks.length - 1]?.value !== total) {
			yTicks.push({ value: total, y: 0 });
		}

		// X axis labels
		const formatDate = (d: Date) => {
			return d.toLocaleDateString('en-US', { month: 'short', day: 'numeric' });
		};

		// Build the actual line SVG path
		const actualPath = actualPoints
			.map((p, i) => `${i === 0 ? 'M' : 'L'}${p.x},${p.y}`)
			.join(' ');

		// Build filled area path (close to bottom)
		const areaPath =
			actualPath +
			` L${actualPoints[actualPoints.length - 1].x},${chartH} L0,${chartH} Z`;

		return {
			total,
			remaining,
			actualPath,
			areaPath,
			idealLine,
			todayX,
			yTicks,
			startLabel: formatDate(start),
			endLabel: formatDate(end),
			showTodayMarker: todayX > 0 && todayX < chartW
		};
	});
</script>

{#if chartData}
	<div class="child-chart">
		<svg viewBox="0 0 {width} {height}" preserveAspectRatio="xMidYMid meet">
			<g transform="translate({padding.left},{padding.top})">
				<!-- Y axis gridlines -->
				{#each chartData.yTicks as tick (tick.value)}
					<line
						x1={0}
						y1={tick.y}
						x2={chartW}
						y2={tick.y}
						stroke="var(--border)"
						stroke-width="0.5"
					/>
					<text
						x={-8}
						y={tick.y}
						text-anchor="end"
						dominant-baseline="middle"
						fill="var(--text-muted)"
						font-size="11"
					>
						{tick.value}
					</text>
				{/each}

				<!-- Ideal line (dashed, gray) -->
				<line
					x1={chartData.idealLine.x1}
					y1={chartData.idealLine.y1}
					x2={chartData.idealLine.x2}
					y2={chartData.idealLine.y2}
					stroke="var(--text-muted)"
					stroke-width="1.5"
					stroke-dasharray="6 4"
					opacity="0.6"
				/>

				<!-- Actual area fill -->
				<path
					d={chartData.areaPath}
					fill="var(--accent-green)"
					opacity="0.1"
				/>

				<!-- Actual line -->
				<path
					d={chartData.actualPath}
					fill="none"
					stroke="var(--accent-green)"
					stroke-width="2"
					stroke-linejoin="round"
				/>

				<!-- Today marker -->
				{#if chartData.showTodayMarker}
					<line
						x1={chartData.todayX}
						y1={0}
						x2={chartData.todayX}
						y2={chartH}
						stroke="var(--accent-blue)"
						stroke-width="1"
						stroke-dasharray="4 3"
						opacity="0.7"
					/>
					<text
						x={chartData.todayX}
						y={-6}
						text-anchor="middle"
						fill="var(--accent-blue)"
						font-size="10"
						opacity="0.8"
					>
						today
					</text>
				{/if}

				<!-- X axis: baseline -->
				<line
					x1={0}
					y1={chartH}
					x2={chartW}
					y2={chartH}
					stroke="var(--border)"
					stroke-width="1"
				/>

				<!-- X axis labels -->
				<text
					x={0}
					y={chartH + 18}
					text-anchor="start"
					fill="var(--text-muted)"
					font-size="11"
				>
					{chartData.startLabel}
				</text>
				<text
					x={chartW}
					y={chartH + 18}
					text-anchor="end"
					fill="var(--text-muted)"
					font-size="11"
				>
					{chartData.endLabel}
				</text>
			</g>
		</svg>
	</div>
{/if}

<style>
	.child-chart {
		width: 100%;
		margin: var(--space-3) 0;
	}

	.child-chart svg {
		width: 100%;
		height: 200px;
		display: block;
	}
</style>
