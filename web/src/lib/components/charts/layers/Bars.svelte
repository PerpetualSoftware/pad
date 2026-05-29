<script lang="ts">
	import { getContext } from 'svelte';
	import { scaleBand } from 'd3-scale';
	import type { LayerCakeContext, ChartDatum, ResolvedSeries } from '../theme';

	interface Props {
		series: ResolvedSeries[];
	}

	let { series }: Props = $props();

	const { data, xGet, xScale, yScale, height } = getContext<LayerCakeContext>('LayerCake');

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
			/>
		{/each}
	{/each}
</g>
