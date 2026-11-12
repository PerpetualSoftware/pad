<script lang="ts">
	import { getLayerCakeContext } from 'layercake';
	import type { LayerCakeContext, ResolvedSeries } from '../theme';

	let { series }: { series: ResolvedSeries[] } = $props();
	const k = getLayerCakeContext() as unknown as LayerCakeContext;

	const half = () => (typeof k.xScale.bandwidth === 'function' ? k.xScale.bandwidth() / 2 : 0);
	function points(key: string) {
		return k.data.map(datum => ({ x: k.xGet(datum) + half(), y: k.yScale(Number(datum[key])) }));
	}
	function path(key: string) {
		return points(key).map((point, index) => `${index ? 'L' : 'M'}${point.x},${point.y}`).join(' ');
	}
</script>

<!-- A dot marks each value, so a one-day window (a path with no segment) still shows every series. -->
<g class="lines">
	{#each series as item (item.key)}
		<path d={path(item.key)} fill="none" stroke={item.color} stroke-width="2" stroke-dasharray={item.key === 'burndown' ? '5 3' : undefined}>
			<title>{item.label}</title>
		</path>
		{#each points(item.key) as point, index (index)}
			<circle class="point" data-series={item.key} cx={point.x} cy={point.y} r="3" fill={item.color} />
		{/each}
	{/each}
</g>
