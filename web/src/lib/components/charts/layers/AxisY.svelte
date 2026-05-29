<script lang="ts">
	import { getContext } from 'svelte';
	import type { LayerCakeContext } from '../theme';

	const { yScale, width } = getContext<LayerCakeContext>('LayerCake');

	const ticks = $derived($yScale.ticks(4));
</script>

<g class="axis-y">
	{#each ticks as tick (tick)}
		<line
			x1={0}
			y1={$yScale(tick)}
			x2={$width}
			y2={$yScale(tick)}
			stroke="var(--border, #e5e7eb)"
			stroke-width="0.5"
		/>
		<text
			x={-8}
			y={$yScale(tick)}
			text-anchor="end"
			dominant-baseline="middle"
			fill="var(--text-muted, #6b7280)"
			font-size="11"
		>
			{tick}
		</text>
	{/each}
</g>
