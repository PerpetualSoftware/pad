<script lang="ts">
	import { getContext } from 'svelte';
	import type { LayerCakeContext, ChartDatum, ResolvedSeries } from '../theme';

	interface Props {
		series: ResolvedSeries[];
	}

	let { series }: Props = $props();

	const { data, xGet, yScale, xScale } = getContext<LayerCakeContext>('LayerCake');

	// Center the point within a band when the x scale is categorical (band).
	function cx(d: ChartDatum): number {
		const half = typeof $xScale.bandwidth === 'function' ? $xScale.bandwidth() / 2 : 0;
		return $xGet(d) + half;
	}

	function num(d: ChartDatum, key: string): number {
		const v = d[key];
		return typeof v === 'number' ? v : Number(v) || 0;
	}

	function path(key: string): string {
		return $data
			.map((d, i) => `${i === 0 ? 'M' : 'L'}${cx(d)},${$yScale(num(d, key))}`)
			.join(' ');
	}
</script>

<g class="lines">
	{#each series as s (s.key)}
		<path d={path(s.key)} fill="none" stroke={s.color} stroke-width="2" stroke-linejoin="round" />
	{/each}
</g>
