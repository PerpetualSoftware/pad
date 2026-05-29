<script lang="ts">
	import { getContext } from 'svelte';
	import type { LayerCakeContext, ChartDatum } from '../theme';

	interface Props {
		maxTicks?: number;
	}

	let { maxTicks = 8 }: Props = $props();

	const { data, x, xGet, xScale, width, height } = getContext<LayerCakeContext>('LayerCake');

	// Show every Nth label so dense category axes stay readable.
	const step = $derived(Math.max(1, Math.ceil($data.length / maxTicks)));

	function center(d: ChartDatum): number {
		const half = typeof $xScale.bandwidth === 'function' ? $xScale.bandwidth() / 2 : 0;
		return $xGet(d) + half;
	}
</script>

<g class="axis-x">
	<line x1={0} y1={$height} x2={$width} y2={$height} stroke="var(--border, #e5e7eb)" stroke-width="1" />
	{#each $data as d, i (i)}
		{#if i % step === 0}
			<text
				x={center(d)}
				y={$height + 16}
				text-anchor="middle"
				fill="var(--text-muted, #6b7280)"
				font-size="11"
			>
				{$x(d)}
			</text>
		{/if}
	{/each}
</g>
