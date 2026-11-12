<script lang="ts">
	import { getLayerCakeContext } from 'layercake';
	import type { LayerCakeContext, ResolvedSeries } from '../theme';

	let { series }: { series: ResolvedSeries[] } = $props();
	const k = getLayerCakeContext() as unknown as LayerCakeContext;

	function path(key: string) {
		const half = typeof k.xScale.bandwidth === 'function' ? k.xScale.bandwidth() / 2 : 0;
		return k.data.map((datum, index) => `${index ? 'L' : 'M'}${k.xGet(datum) + half},${k.yScale(Number(datum[key]))}`).join(' ');
	}
</script>

<g class="lines">
	{#each series as item (item.key)}
		<path d={path(item.key)} fill="none" stroke={item.color} stroke-width="2" stroke-dasharray={item.key === 'burndown' ? '5 3' : undefined}>
			<title>{item.label}</title>
		</path>
	{/each}
</g>
