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
		<div class="canvas" style:height={`${height}px`}>
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
					<Bars series={resolvedSeries} />
				</Svg>
			</LayerCake>
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
