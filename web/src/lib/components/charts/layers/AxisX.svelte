<script lang="ts">
	import { getLayerCakeContext } from 'layercake';
	import type { LayerCakeContext, ChartDatum } from '../theme';

	interface Props {
		maxTicks?: number;
	}

	let { maxTicks = 8 }: Props = $props();

	// Held whole, read at each use site — see the note in AxisY.svelte (TASK-3093).
	const k = getLayerCakeContext() as unknown as LayerCakeContext;

	// Show every Nth label so dense category axes stay readable.
	const step = $derived(Math.max(1, Math.ceil(k.data.length / maxTicks)));

	function center(d: ChartDatum): number {
		const half = typeof k.xScale.bandwidth === 'function' ? k.xScale.bandwidth() / 2 : 0;
		return k.xGet(d) + half;
	}
</script>

<g class="axis-x">
	<line x1={0} y1={k.height} x2={k.width} y2={k.height} stroke="var(--border, #e5e7eb)" stroke-width="1" />
	{#each k.data as d, i (i)}
		{#if i % step === 0}
			<text
				x={center(d)}
				y={k.height + 16}
				text-anchor="middle"
				fill="var(--text-muted, #6b7280)"
				font-size="11"
			>
				{k.x(d)}
			</text>
		{/if}
	{/each}
</g>
