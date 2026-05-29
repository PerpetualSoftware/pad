<script lang="ts">
	import { LayerCake, Svg } from 'layercake';
	import { scaleBand, scaleLinear } from 'd3-scale';
	import Bars from './layers/Bars.svelte';
	import AxisX from './layers/AxisX.svelte';
	import AxisY from './layers/AxisY.svelte';
	import { resolveColor, type ChartDatum, type ResolvedSeries } from './theme';

	interface Props {
		data: ChartDatum[];
		x: string;
		series: { key: string; label: string; color?: string }[];
		height?: number;
		ariaLabel: string;
	}

	let { data, x, series, height = 240, ariaLabel }: Props = $props();

	const resolvedSeries = $derived<ResolvedSeries[]>(
		series.map((s, i) => ({ key: s.key, label: s.label, color: resolveColor(s.color, i) }))
	);

	const hasData = $derived(data.length > 0 && series.length > 0);
	const padding = { top: 12, right: 12, bottom: 28, left: 36 };

	// Per-band hover state, lifted from <Bars> so the parent owns the tooltip.
	let hover = $state<{ index: number; centerX: number } | null>(null);
	let canvasWidth = $state(0);
	let tooltipWidth = $state(0);

	const hovered = $derived(hover ? data[hover.index] : null);

	// Clamp the tooltip's left edge so it never overflows the canvas. The tooltip
	// is centered over the band, then nudged back inside the [0, canvasWidth] range.
	const tooltipLeft = $derived.by(() => {
		if (!hover) return 0;
		const half = tooltipWidth / 2;
		const min = half;
		const max = Math.max(half, canvasWidth - half);
		return Math.min(Math.max(hover.centerX, min), max);
	});

	function num(d: ChartDatum, key: string): number {
		const v = d[key];
		return typeof v === 'number' ? v : Number(v) || 0;
	}

	// y accessor returns the largest series value per datum so the linear
	// y-domain (yDomain={[0, null]}) accommodates the tallest grouped bar.
	const yAccessor = $derived.by(() => {
		const ser = resolvedSeries;
		return (d: ChartDatum) => {
			let max = 0;
			for (const s of ser) {
				const v = d[s.key];
				const n = typeof v === 'number' ? v : Number(v) || 0;
				if (n > max) max = n;
			}
			return max;
		};
	});
</script>

<div class="chart" role="img" aria-label={ariaLabel}>
	{#if hasData}
		<div class="legend">
			{#each resolvedSeries as s (s.key)}
				<span class="legend-item">
					<span class="swatch" style:background-color={s.color}></span>
					{s.label}
				</span>
			{/each}
		</div>
		<div class="canvas" style:height={`${height}px`} bind:clientWidth={canvasWidth}>
			<LayerCake
				{data}
				{x}
				y={yAccessor}
				xScale={scaleBand().paddingInner(0.2)}
				yScale={scaleLinear()}
				yDomain={[0, null]}
				{padding}
			>
				<Svg>
					<AxisY />
					<AxisX />
					<Bars series={resolvedSeries} onHover={(h) => (hover = h)} />
				</Svg>
			</LayerCake>

			{#if hover && hovered}
				<div
					class="tooltip"
					style:left={`${tooltipLeft}px`}
					bind:clientWidth={tooltipWidth}
				>
					<div class="tooltip-header">{hovered[x]}</div>
					{#each resolvedSeries as s (s.key)}
						<div class="tooltip-row">
							<span class="tooltip-swatch" style:background-color={s.color}></span>
							<span class="tooltip-label">{s.label}</span>
							<span class="tooltip-value">{num(hovered, s.key)}</span>
						</div>
					{/each}
				</div>
			{/if}
		</div>
	{:else}
		<div class="empty" style:height={`${height}px`}>No data</div>
	{/if}
</div>

<style>
	.chart {
		width: 100%;
	}

	.canvas {
		width: 100%;
		position: relative;
	}

	.tooltip {
		position: absolute;
		top: 8px;
		transform: translateX(-50%);
		pointer-events: none;
		z-index: 1;
		min-width: max-content;
		padding: 0.4rem 0.55rem;
		border-radius: 6px;
		background: var(--surface, #ffffff);
		border: 1px solid var(--border, #e5e7eb);
		box-shadow: 0 2px 8px rgb(0 0 0 / 0.12);
		font-size: 0.75rem;
		color: var(--text, #111827);
	}

	.tooltip-header {
		font-weight: 600;
		margin-bottom: 0.25rem;
		white-space: nowrap;
	}

	.tooltip-row {
		display: flex;
		align-items: center;
		gap: 0.4rem;
		white-space: nowrap;
	}

	.tooltip-row + .tooltip-row {
		margin-top: 0.15rem;
	}

	.tooltip-swatch {
		display: inline-block;
		width: 0.6rem;
		height: 0.6rem;
		border-radius: 2px;
		flex-shrink: 0;
	}

	.tooltip-label {
		color: var(--text-muted, #6b7280);
	}

	.tooltip-value {
		margin-left: auto;
		font-weight: 600;
		font-variant-numeric: tabular-nums;
	}

	.legend {
		display: flex;
		flex-wrap: wrap;
		gap: 0.75rem;
		margin-bottom: 0.5rem;
		font-size: 0.75rem;
		color: var(--text-muted, #6b7280);
	}

	.legend-item {
		display: inline-flex;
		align-items: center;
		gap: 0.35rem;
	}

	.swatch {
		display: inline-block;
		width: 0.7rem;
		height: 0.7rem;
		border-radius: 2px;
	}

	.empty {
		display: flex;
		align-items: center;
		justify-content: center;
		color: var(--text-muted, #6b7280);
		font-size: 0.875rem;
	}
</style>
