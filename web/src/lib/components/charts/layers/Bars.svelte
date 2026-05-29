<script lang="ts">
	import { getContext } from 'svelte';
	import { scaleBand } from 'd3-scale';
	import type { LayerCakeContext, ChartDatum, ResolvedSeries } from '../theme';

	interface Props {
		series: ResolvedSeries[];
		onHover?: (hover: { index: number; centerX: number } | null) => void;
	}

	let { series, onHover }: Props = $props();

	const { data, x, xGet, xScale, yScale, height, padding } =
		getContext<LayerCakeContext>('LayerCake');

	const bandwidth = $derived(
		typeof $xScale.bandwidth === 'function' ? $xScale.bandwidth() : 0
	);

	// Inner scale positioning each series side-by-side within a band.
	const inner = $derived(
		scaleBand<string>()
			.domain(series.map((s) => s.key))
			.range([0, bandwidth])
			.paddingInner(0.1)
	);

	function barX(d: ChartDatum, key: string): number {
		return $xGet(d) + (inner(key) ?? 0);
	}

	function num(d: ChartDatum, key: string): number {
		const v = d[key];
		return typeof v === 'number' ? v : Number(v) || 0;
	}

	// Per-band summary for the accessible <title> on hit-rects: "May 28 — Created 5, Completed 3".
	function bandSummary(d: ChartDatum): string {
		const label = String($x(d));
		const parts = series.map((s) => `${s.label} ${num(d, s.key)}`);
		return `${label} — ${parts.join(', ')}`;
	}

	// Band center in canvas pixels: plot-area scale + left padding offset.
	function bandCenter(d: ChartDatum): number {
		return $padding.left + $xGet(d) + bandwidth / 2;
	}
</script>

<g class="bars">
	{#each $data as d, i (i)}
		{#each series as s (s.key)}
			<rect
				x={barX(d, s.key)}
				y={$yScale(num(d, s.key))}
				width={inner.bandwidth()}
				height={Math.max(0, $height - $yScale(num(d, s.key)))}
				fill={s.color}
			>
				<title>{s.label}: {num(d, s.key)}</title>
			</rect>
		{/each}
	{/each}

	<!-- Invisible full-height hit-rect per band drives per-category hover. -->
	{#each $data as d, i (i)}
		<rect
			class="hit"
			x={$xGet(d)}
			y={0}
			width={bandwidth}
			height={$height}
			fill="transparent"
			onpointerenter={() => onHover?.({ index: i, centerX: bandCenter(d) })}
			onpointermove={() => onHover?.({ index: i, centerX: bandCenter(d) })}
			onpointerleave={() => onHover?.(null)}
			role="presentation"
		>
			<title>{bandSummary(d)}</title>
		</rect>
	{/each}
</g>

<style>
	.hit {
		pointer-events: all;
	}
</style>
