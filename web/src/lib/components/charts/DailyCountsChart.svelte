<script lang="ts">
	import { LayerCake, Svg } from 'layercake';
	import { scaleBand, scaleLinear } from 'd3-scale';
	import type { AgendaDayCount } from '$lib/types';
	import Bars from './layers/Bars.svelte';
	import Lines from './layers/Lines.svelte';
	import AxisX from './layers/AxisX.svelte';
	import AxisY from './layers/AxisY.svelte';
	import { resolveColor, type ChartDatum } from './theme';

	let { data }: { data: AgendaDayCount[] } = $props();
	const series = [
		{ key: 'completed', label: 'Completed' },
		{ key: 'open', label: 'Open at day end' },
		{ key: 'burndown', label: 'Burn-down' }
	].map((item, index) => ({ ...item, color: resolveColor(undefined, index) }));
	const chartData = $derived(data.map(item => ({ ...item })));
	const maximum = $derived(Math.max(1, ...data.flatMap(item => [item.completed, item.open, item.burndown])));
	const y = (item: ChartDatum) => Math.max(Number(item.completed), Number(item.open), Number(item.burndown));
	// The API day is already local. UTC here preserves its calendar date for display.
	const formatter = new Intl.DateTimeFormat('en-US', { month: 'short', day: 'numeric', timeZone: 'UTC' });
	const dayLabel = (day: string | number) => formatter.format(new Date(`${day}T12:00:00Z`));
</script>

<section class="daily-counts">
	<div class="legend">
		{#each series as item (item.key)}
			<span class="legend-item"><span class="swatch" style:background-color={item.color}></span>{item.label}</span>
		{/each}
	</div>
	<div class="canvas" role="img" aria-label="Daily counts">
		<LayerCake data={chartData} x="day" {y} xScale={scaleBand().paddingInner(0.2)} yScale={scaleLinear()}
			yDomain={[0, maximum]} padding={{ top: 12, right: 12, bottom: 28, left: 36 }}>
			<Svg>
				<AxisY />
				<AxisX format={dayLabel} />
				<Bars series={series.slice(0, 1)} />
				<Lines series={series.slice(1)} />
			</Svg>
		</LayerCake>
	</div>
	<table class="sr-only">
		<caption>Daily counts</caption>
		<thead><tr><th scope="col">Day</th>{#each series as item (item.key)}<th scope="col">{item.label}</th>{/each}</tr></thead>
		<tbody>
			{#each data as item (item.day)}
				<tr><th scope="row"><time datetime={item.day}>{dayLabel(item.day)}</time></th><td>{item.completed}</td><td>{item.open}</td><td>{item.burndown}</td></tr>
			{/each}
		</tbody>
	</table>
</section>

<style>
	.daily-counts { margin-top: var(--space-5); width: 100%; }
	.canvas { position: relative; width: 100%; height: 240px; }
	.legend { display: flex; flex-wrap: wrap; gap: var(--space-3); margin-bottom: var(--space-2); font-size: 0.8rem; color: var(--text-secondary); }
	.legend-item { display: inline-flex; align-items: center; gap: var(--space-2); }
	.swatch { width: 0.7rem; height: 0.7rem; border-radius: 2px; }
	.sr-only { position: absolute; width: 1px; height: 1px; padding: 0; margin: -1px; overflow: hidden; clip-path: inset(50%); white-space: nowrap; border: 0; }
</style>
